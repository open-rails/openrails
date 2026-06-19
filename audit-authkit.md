# Architecture Review — Authentication

*2026-06-18 · companion to `openrails/docs/architecture-review.md` and the June 18 API audit*

---

## Purpose

The June 18 API design audit catalogued route-shape problems for authkit (AK-API-H1, H2, M1, M2, L1, L2, L3). This document is the next layer down: implementation-level findings in the authentication-critical paths — token lifecycle, session model, signing key management, and revocation propagation. authkit has no payment surface; this review is auth-only.

The openrails review identified that delegated tokens have a permission-staleness problem (OR-IMPL-3) because permissions are baked into the JWT. authkit owns the issuer side of that contract, and the equivalent issue exists here at the *session* layer — a revoked session, banned user, or rotated password does not invalidate already-issued access tokens. Plus three concrete categories of swallowed errors in security-critical paths, and the absence of a key-rotation lifecycle.

What is **out of scope**:

- Surface-API issues already in the June 18 audit (`/remote-applications` org scoping, `/token` vs `/token/org` flow, `/admin/users/set-*` REST verbs, `/register/availability` enumeration, dead 404 routes). These are surface; they don't change the implementation-layer findings here and should land in a separate hygiene pass.
- Database transaction discipline, SQL injection, constant-time comparisons, password hashing — exploration found these to be **clean**. Argon2id with bcrypt legacy support, lazy rehash on login, `subtle.ConstantTimeCompare` used throughout, parameterized queries via sqlc. Not perfect (Argon2id `Time: 1` is on the low side) but not the issue.
- Rate limiting — implementation is dual per-IP + per-identifier with Redis or in-process backend. Clean.

## Findings — at a glance

| ID | Surface | Severity | What |
|----|---------|----------|------|
| AK-IMPL-1 | Sessions / tokens | High | Access tokens remain valid until expiry after session revoke, user ban, or password change — no server-side check on JWTs at request time |
| AK-IMPL-2 | Sessions / OIDC / SIWS | High | Swallowed errors on three security-critical paths: refresh-token-theft family revoke, disabled-user revoke-all, OAuth provider link |
| AK-IMPL-3 | Keys / JWT | Medium-High | No JWT signing key rotation flow — keys are loaded once and held until restart; `kid` is plumbed through but lifecycle isn't |

Each section: diagnosis, recommended approach, critical files, verification.

---

## AK-IMPL-1 — Session and JWT revocation lag

### Diagnosis

authkit issues short-lived **access tokens** (RS256 JWTs) and longer-lived **refresh tokens** (random strings hashed in the `sessions` table). The refresh side is well-built: refresh tokens are SHA-256-hashed at rest (never stored plaintext), refresh-token rotation is atomic (`SessionRotate` at `core/service_sessions.go:124-128` swaps `current_token_hash` → `previous_token_hash` and writes a new current), and re-use of a previous-hash triggers `revokeFamily` (line 102-105). This is the right shape.

The gap is at the **access-token** layer:

- **Logout** (`http/logout_delete.go:25`) calls `RevokeSessionByIDForUser`, which sets `revoked_at` on the session row. The access token JWT remains cryptographically valid and accepted by any verifier that only checks signature, issuer, audience, and expiry — which is the default contract. The session row is consulted only at `/token` refresh time (`ExchangeRefreshToken` at `service_sessions.go:118` re-checks `IsUserAllowed`). Between now and access-token expiry, a stolen access token still works.
- **Admin set-password** (`AdminSetPassword`) and **password change** (`ChangePassword`) call `RevokeAllSessions`, but the existing access tokens remain valid for the rest of their TTL.
- **User ban** has the same shape — the user can be banned, future refreshes will fail (`ensureUserAccessByID` + `IsUserAllowed`), but the in-flight access token works until expiry.
- **Service token revocation** is the one bright spot: service tokens are opaque, looked up per request (`ResolveServiceTokenWithResources`), so revocation is immediate. JWT-bearing access tokens are not.

This is the **same shape** as OR-IMPL-3 in the openrails review (delegated tokens carry permissions in the JWT; revocation lags), but at authkit's session-token layer it is the root cause: openrails inherits the model. Fixing it here fixes the consumer side automatically.

The exposure window equals the access-token TTL. The configured TTL is not literal-coded in the source (`Options.AccessTokenDuration` at `core/service.go:37`), but defaults of 15-60 minutes are typical. Whatever the actual value, the structural problem is: a revocation event has no synchronous effect on already-issued JWTs.

### Recommended approach

Three options ordered by cost. The recommendation is to ship 1a + 1b together; 1c only if a security review demands it.

**1a. Aggressively shorten access-token TTL to 5 minutes; rely on refresh-token rotation.**

Refresh-token rotation is already correct: each refresh issues a new access token (with a fresh permission snapshot), invalidates the prior refresh token, and re-checks `IsUserAllowed`. With a 5-minute access-token TTL, the worst-case revocation lag is 5 minutes, and the family-revoke-on-reuse mechanism catches stolen refresh tokens. This is operationally cheap — no new infrastructure, only a config change — and bounds the worst case.

Trade-off: more `/token` round trips. Acceptable; refresh is cheap.

**1b. Add a `jti` claim and a fast revocation lookup on each request.**

Authkit's access tokens currently embed `sub`, `org`, `roles`, etc. but not a `jti` (JWT ID). Add one. On each request, after JWT verification, consumers look up `(jti, status)` in Redis or a small revocation table. Revoking a session writes the active `jti`s for that session to the revocation set. The lookup is O(1) and stays under 1ms with Redis.

Crucially, this is a primitive authkit should *provide* — both as a server-side issuer write at revocation time AND as a verifier helper that openrails (and any other consumer) can call. Today every consumer is on its own; an opt-in middleware `authkit/http/verifier.RequireSessionLive(...)` would close the gap by default.

This addresses the cases 1a doesn't: admin set-password and ban should be enforced *immediately*, not within 5 minutes. The two together give: 5-minute worst case for ordinary logout, immediate for admin actions that explicitly want immediacy.

**1c. (Reject for v1.)** Switch access tokens to opaque, look up per request like service tokens do. Cleanest semantics, highest per-request cost, breaks the offline-verifiable JWT contract that federated consumers depend on. Revisit only if 1a+1b prove insufficient.

### Critical files

- `core/service.go:37` (`AccessTokenDuration` default) — shorten default to 5 minutes
- `core/service.go` (`IssueAccessToken`) — add `jti` claim at issue time
- `core/service_sessions.go` — on `RevokeSession*`, `RevokeAllSessions`, `AdminSetPassword`, `BanUser` paths, write the active `jti` set to the revocation store
- `jwt/verifier.go` (the consumer-side verifier) — add `WithSessionLivenessCheck(store)` option that performs the `jti` lookup
- `http/verifier.go` or `authprovider/` — a middleware wrapper that makes session-liveness checks opt-in for the embedded `Authenticator` openrails uses (parallels openrails' `ginmw` integration)
- `migrations/postgres/` — table or Redis schema for `revoked_jti(jti, revoked_at, reason)` with TTL matching the access-token max lifetime + grace

### Verification

- Unit: issue a token, revoke the session, immediately verify with liveness check → 401 with `session_revoked`.
- Unit: issue a token, ban the user, immediately call a verified endpoint → 401 with `user_disabled`.
- Unit: with 1a only (no `jti` check), the same scenario succeeds until the 5-minute TTL elapses, then refresh fails. (Confirms 1a's narrower contract.)
- Integration: end-to-end — log in, copy the access token, log out, retry the original token; with 1b enabled, the retry fails immediately.
- Performance: Redis liveness check < 1ms p99; without Redis (in-process), measure cache hit rate and stale-entry tolerance.

---

## AK-IMPL-2 — Swallowed errors on security-critical revocation and linking paths

### Diagnosis

Three categories of `_ = ...err` patterns in code paths that are exactly the wrong places to silently fail.

**2a. Refresh-token theft detection** (`core/service_sessions.go:103`):

```go
if prev, e2 := s.q.SessionByPreviousTokenHash(ctx, ...); e2 == nil {
    _ = s.revokeFamily(ctx, prev.FamilyID)
    return "", time.Time{}, "", errors.New("refresh token reuse detected")
}
```

This is the path that fires when a refresh token is reused — i.e., the family-revocation defense against refresh-token theft. If `revokeFamily` errors (DB hiccup, connection pool exhaustion), both the legitimate user's sessions AND the attacker's stolen-but-now-rotated session remain valid. The user gets the generic "refresh token reuse detected" error and is told to log in again, but the attacker's refresh token is still alive in the database. The defense degrades to "tell the legitimate user something went wrong while leaving the attacker fully in possession."

**2b. Disabled-user session revoke** (`core/service_sessions.go:118`):

```go
if ok, e := s.IsUserAllowed(ctx, uid); e != nil || !ok {
    _ = s.RevokeAllSessions(WithSessionRevokeReason(ctx, SessionRevokeReasonUserDisabled), uid, nil)
    return "", time.Time{}, "", errors.New("user_disabled")
}
```

When a refresh attempt hits this path, the user has been banned/disabled since the last token issue. The intent is "kill all their sessions on the way out." If `RevokeAllSessions` errors, the sessions remain. Next refresh attempt also fails (and also tries-and-fails to revoke), but in the meantime the user's in-flight access tokens stay valid until expiry — the ban is silently soft.

**2c. OIDC provider link writes** (`http/oauth2_browser.go:346, 348, 382`):

```go
_ = s.svc.LinkProviderByIssuer(r.Context(), sd.LinkUserID, cfg.Issuer, cfg.Name, info.Subject, emailPtr)
_ = s.svc.SetProviderUsername(r.Context(), sd.LinkUserID, cfg.Issuer, info.Subject, info.Preferred)
...
_ = s.svc.SetEmailVerified(r.Context(), u.ID, true)
```

After an OIDC callback succeeds, these writes establish "this OIDC identity now links to this authkit user" and "this email is verified." If any of them errors, the user appears authenticated this session but the persistent link doesn't exist or the email isn't marked verified. Next login via the same OIDC provider might fail to find the link and either error out or — depending on the flow — provision a *new* authkit user with the same email, producing two accounts that share an identity.

**2d. Solana SIWS challenge delete** (`core/service_solana.go:117`):

```go
_ = cache.Del(ctx, parsedInput.Nonce)
```

After verifying a SIWS-signed login, the challenge nonce is supposed to be consumed (single-use). If `Del` errors (Redis connection drop, cache backend issue), the same signed message can be replayed within the 15-minute challenge TTL. This is the only path in the audit where a swallowed error opens a *replay window* rather than a *state-divergence window*.

### Recommended approach

This is not one fix; it's a category. The remediation is uniform:

**For 2a, 2b, 2c:** Wrap each revocation/link write in a `defer`-checked error path. On failure:
- Log the error with structured context (the family ID, user ID, OIDC provider, etc.) at ERROR level, not WARN.
- Return a 500 to the caller — let the user retry rather than silently leaving the system in a divergent state.
- For 2a specifically: the user-facing message can still be "log in again" but the operator gets paged.
- Optionally: enqueue a deferred sweeper job that retries the revocation/link write with backoff. The right pattern is a `pending_revocations` table that a worker drains.

**For 2d:** Same shape, but with a twist — the SIWS verify has *already* succeeded by the time `Del` fails. Returning a 500 forces the user to redo the wallet signature, which is friction but correct. Alternative: write to a "consumed" set BEFORE the verify path completes its success branch, so a Del failure is impossible because deletion is by-construction. (Atomic "verify and consume" rather than "verify then consume.")

The infrastructure for structured error logging already exists in this codebase; the change is mostly mechanical. The architectural addition is the `pending_revocations` retry queue if the team wants graceful degradation rather than 500-and-retry.

### Critical files

- `core/service_sessions.go:103` — refresh-token reuse path
- `core/service_sessions.go:118` — disabled-user revoke path
- `http/oauth2_browser.go:346, 348, 382` — OIDC linking and email verification writes
- `core/service_solana.go:117` — SIWS challenge deletion (or refactor to atomic verify+consume)
- `core/sweepers.go` (new) or extend existing sweepers — `pending_revocations` worker if going the deferred-retry route
- `migrations/postgres/` — `pending_revocations(id, kind, target_id, reason, attempts, last_error, next_attempt_at)` if applicable

### Verification

- Unit: inject a DB error into `revokeFamily` during refresh-token reuse; expect a 500 with `family_revoke_failed`, an ERROR log line with family_id, and (if sweeper-pattern adopted) a row in `pending_revocations`.
- Unit: same for the disabled-user revoke path.
- Unit: simulate Redis Del failure on SIWS verify; expect either 500 (refactor option A) or successful consume-then-verify ordering (refactor option B).
- Integration: full OIDC callback with the linking write deliberately failing; expect the user-facing flow to surface the error rather than completing the session with no link.
- Operational: a dashboard or query that lists current `pending_revocations` so ops can monitor for stuck retries.

---

## AK-IMPL-3 — JWT signing key rotation lifecycle

### Diagnosis

`jwt/keys.go` implements correct key loading and storage: a three-tier cascade (env vars → vault-mounted `/vault/auth/keys.json` → auto-generated dev keys), production blocks the dev fallback (line 176-178), and every issued JWT carries a `kid` header (`jwt/jwt.go:76`, `token.Header["kid"] = s.kid`). The JWKS endpoint (`jwt/jwks.go`) serves the active public key with proper ETag caching. RS256-only on issuance (other algorithms are accepted only on JWKS parsing for federated tokens) — no key-confusion risk.

What's missing is the **rotation lifecycle**. The signing key is loaded once at boot and held until restart:

- No mechanism for issuing tokens with a *new* `kid` while still serving the *old* `kid` from JWKS for the duration of in-flight token TTLs.
- No "promote pending → active" or "demote active → retired" state machine.
- Rotation requires: regenerate the key, update the vault, restart the process. During the restart, in-flight tokens signed by the previous key go from "valid" to "verifiable" (still in JWKS for federated consumers) to "rejected once restart completes" — there is no graceful overlap.
- Catastrophe scenario: a signing key is suspected compromised. Today's response is "rotate the vault secret, restart everything, accept that legitimate users' in-flight tokens are also dead." The right response is "issue with new kid immediately; accept old kid for a 5-minute grace; then reject."

This compounds with AK-IMPL-1: without revocation, key compromise is the *only* tool for invalidating in-flight tokens, and rotating the key is needlessly disruptive. Fix both together and the system gains both proper revocation AND graceful key rotation.

### Recommended approach

A small state machine on the keystore. Keys live in one of three states: `pending` (provisioned but not yet signing), `active` (signing AND in JWKS), `retired` (no longer signing but still in JWKS until its grace window expires).

Flow:

1. **Rotation initiated** — provision a new key, persist as `pending` with metadata (`kid`, `created_at`, `algorithm`).
2. **Promote pending → active** — atomically: the old active becomes `retired` with a `retire_at` timestamp, the new pending becomes `active`. Future tokens sign with the new key.
3. **Both keys in JWKS** during the grace window. Verifiers (authkit consumers and federated) accept both.
4. **Sweeper retires the old key** when `retire_at` < now. It drops out of JWKS. Any tokens still signed by it are rejected.

The grace window equals the access-token max TTL + a safety margin. With AK-IMPL-1's 5-minute TTL, the grace window can be tight (10-15 minutes). Today's effectively-unbounded TTL would force much longer grace windows.

Operator surface: a `POST /admin/keys/rotate` endpoint (admin permission) that triggers the pending→active promotion, and `GET /admin/keys` that lists the keystore state. Background workers handle the eventual retirement.

This is mostly **storage and lifecycle** work; the cryptographic primitives are already correct. Estimate is modest.

### Critical files

- `jwt/keys.go` — extend the keystore from a single in-memory key to a state-aware structure; add `Pending(kid)`, `Active(kid)`, `Retired(kid, retire_at)` operations
- `jwt/jwks.go` — serve both `active` and `retired` keys from JWKS for the duration of the grace window
- `jwt/jwt.go:76` — signer reads from `keystore.Active()` not from a frozen-at-boot reference
- `http/admin_keys.go` (new) — `POST /admin/keys/rotate`, `GET /admin/keys`, `POST /admin/keys/retire-now` (emergency)
- `core/sweepers.go` — sweeper job that retires keys past their `retire_at`
- `migrations/postgres/` — table `signing_keys(kid, algorithm, public_key, private_key_encrypted, state, created_at, promoted_at, retired_at, retire_at)`

### Verification

- Unit: rotate a key; both `kid`s appear in JWKS; tokens issued post-rotation carry the new `kid`; tokens issued pre-rotation continue to verify until retire_at.
- Unit: emergency retire-now; the old key is removed from JWKS immediately; pre-rotation tokens are immediately rejected.
- Integration: rolling rotation across multiple replicas — every replica reads the keystore on each issue, so the rotation event propagates without a restart.
- Operational: a dashboard or `GET /admin/keys` showing current keystore state, last rotation, time-to-next-rotation if scheduled.

---

## Sequencing

| Step | Issue | Estimate | Rationale |
|------|-------|----------|-----------|
| 1 | AK-IMPL-2 (swallowed errors) | ~3-4 days | Concrete, mechanical, immediate security improvement — no architectural design needed |
| 2 | AK-IMPL-1a (shorten access-token TTL) | ~1 day | Config + a few tests; bounds the worst case before the bigger lift |
| 3 | AK-IMPL-1b (`jti` + liveness check) | 1-2 weeks | Architectural change with a clean migration path; the openrails consumer gets a turnkey middleware as part of this |
| 4 | AK-IMPL-3 (key rotation lifecycle) | 1-2 weeks | Different code area — can run in parallel with #3 |

Total: 3-4 weeks of focused work to close all three. Reuses much of the openrails team's familiarity (AK-IMPL-1 mirrors OR-IMPL-3) so the design conversation is short.

---

## Backlog — secondary findings

Surfaced during exploration; worth tracking but not in the top-3 deep-dive.

- **Audit logging is incomplete and the sink is optional.** `core/audit.go` defines `AuthEventLogger`, but several security-sensitive paths don't write events: 2FA enable/disable (`Enable2FA`/`Disable2FA`), password change (relies on session-revoke event), admin set-password / set-email / set-username, service token issue and revoke, role grant and revoke, federated remote-application membership changes. The logger can be nil, in which case the audit trail silently disappears. Two fixes: (i) require a non-nil logger in production, (ii) add explicit events for each of the listed actions. Incident-response readiness depends on this.
- **OIDC state validation responsibility lives with the consumer.** `oidc/manager.go` produces the auth URL with `state` and `nonce`, but the *validation* on callback is the consumer's job. `storage/memory/state_cache.go:59-65` shows authkit has the state-cache primitive; it should also ship the validation middleware so that the default secure path is the easy path. Consumers that forget to validate open a CSRF on OIDC flows.
- **Service token resolution is one DB query per request.** No caching layer. At low volume this is fine; at high volume it's a bottleneck and a single-point-of-failure on the DB. A small TTL'd cache (with explicit invalidation on revoke) keyed by the token's `key_id` would close this. The same revocation propagation question as AK-IMPL-1 applies — cache TTL becomes the revocation lag.
- **No remote-application JWKS caching.** `service_remote_applications.go` fetches JWKS from the remote issuer on verify with no built-in caching. Either authkit ships HTTP caching (with `Cache-Control` respect) or each consumer must layer their own. The former is the right default.
- **Argon2id `Time: 1` is below OWASP guidance.** `password/password.go:24` — `Params{Time: 1, Memory: 64 * 1024, Threads: 1, ...}`. OWASP currently recommends Time: 2-3 for 64 MiB memory. Bump to 2 and re-hash lazily on login.
- **Backup-code entropy is ~48 bits.** 8-char base36 (excluding ambiguous chars). Acceptable for single-use, rate-limited consumption; document the choice or bump to 10 chars.

## Deferred to other planning sessions

- **Surface-API audit items** (AK-API-H1/H2/M1/M2/L1/L2/L3 from June 18) — single hygiene PR.
- **Authkit-as-library packaging** — the embedded vs. standalone story isn't covered here; if the team plans to ship authkit as a standalone service, the operational surface (admin UI, key rotation tooling, JWKS health endpoints, audit-log access) becomes a separate workstream.
