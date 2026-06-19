<!-- openrails issue tracker — SECURITY issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. Security ids use the SEC- prefix space (separate
> from the progress/future/completed numeric space).
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.

---

# #SEC-1: ccbill-ip-list-configurable

**Status:** implemented — verified in code 2026-06-12 (automated review; sub-task checkboxes left for the closing agent). `processors.ccbill.allowed_cidrs` config (config/config.go ProcessorsConfig.AllowedCIDRs) feeds `iputil.Configure` (internal/shared/iputil/ccbill.go), which parses CIDRs at boot and fails fast on invalid entries; hard-coded ranges remain only as documented fallback defaults.

**Priority:** high
**Files:** internal/shared/iputil/ccbill.go

Move CCBill IP allowlist from hard-coded source into config (env vars / CI/CD pipeline). Currently lives in internal/shared/iputil/ccbill.go as static CIDR constants. Must be updatable without a code deploy when CCBill adds or rotates IP ranges.

**Tasks:**
- [ ] Move CIDR list from hard-coded Go constants to config.yaml / env var (e.g. ccbill.allowed_cidrs)
- [ ] Parse CIDRs at boot, fail-fast on invalid entries
- [ ] Add config reload or at minimum document the restart-to-update requirement
- [ ] Update IsValidCCBillIP to read from the runtime config instead of package-level constants
- [ ] Add integration test that verifies the config-driven allowlist rejects unknown IPs

---

# #SEC-2: security-scanning-ci-cd

**Priority:** medium
**Files:** .github/workflows/

Add security scanning tools to the GitHub CI/CD pipeline. Run on PR open and/or commit. Tools: gosec (SAST), golangci-lint (quality/style), govulncheck (dependency vulns). Consider Trivy, Semgrep, or Snyk for additional SAST and SCA coverage.

**Tasks:**
- [ ] Add gosec step to CI pipeline (Go SAST - source code security scanning)
- [ ] Add golangci-lint step (code quality, linting, style enforcement)
- [ ] Add govulncheck step (dependency vulnerability scanning against Go vuln DB)
- [ ] Consider adding Snyk or Trivy as an additional SCA scanner in pipeline
- [ ] Configure pipeline to run on PR open and push to main
- [ ] Set appropriate severity thresholds (fail on HIGH+, warn on MEDIUM)

---

# #SEC-3: webhook-body-size-limit

**Status:** implemented — verified in code 2026-06-12 (automated review). `ginmw.BodyLimit` no longer exempts webhook routes (internal/http/middleware/ginmw/security.go) and per-processor `http.MaxBytesReader` caps with 413 responses live in internal/http/handlers/webhook.go.

**Priority:** high
**Files:** internal/http/middleware/security.go, internal/http/handlers/webhook.go

Webhook routes currently opt out of body-size limit entirely (internal/http/middleware/security.go). An attacker who can reach the webhook port can send multi-gigabyte payloads and exhaust memory. Apply per-processor caps using http.MaxBytesReader.

**Tasks:**
- [ ] Remove the blanket body-limit exemption for /v1/webhooks/*
- [ ] Apply per-processor limits via http.MaxBytesReader on r.Request.Body:
-     - CCBill: 2 KiB (form-encoded key-value pairs, always small)
-     - Stripe: 5 KiB (thin events; hydrated objects are fetched server-side)
-     - NMI: 5 KiB (JSON webhook payloads)
- [ ] Return 413 Payload Too Large if limit exceeded before any further processing
- [ ] Add test that sends oversized body and asserts 413

---

# #SEC-4: rate-limiting-enhancements

**Priority:** medium
**Files:** internal/http/middleware/ratelimit.go

Implement more granular rate limiting within the OpenRails service: by IP, by authenticated user ID, and by payload size. Current rate limiter only uses raw IP and collapses behind a proxy.

**Tasks:**
- [ ] Add per-authenticated-user rate limiting (keyed on JWT user_id) for mutation endpoints
- [ ] Add per-IP rate limiting that works correctly behind a proxy (requires SEC-6 trusted proxies fix)
- [ ] Add payload-size-based throttling: reject early if Content-Length exceeds route threshold
- [ ] Consider /24 aggregation for the IP-based limiter to handle NAT/cloud scenarios
- [ ] Document the rate-limit tiers per endpoint category (webhooks, checkout, admin, credits)

---

# #SEC-5: remove-static-expected-audience-verifier

**Status:** superseded — the config-declared static JWT verifier was removed. `auth.expected_audience` is now a retired/ignored config key; standalone OpenRails authenticates users/admins through its own AuthKit control plane, whose audience is fixed to `openrails`.

**Priority:** high
**Files:** internal/auth/verifier.go, config/config.go

The old risk was: when `expected_audience` config was empty, the static JWT verifier accepted tokens with any audience claim. The long-term fix is stronger than validation: remove the static verifier config surface entirely.

**Tasks:**
- [x] Remove `auth.expected_audience` from runtime config.
- [x] Remove standalone verifier construction from `auth.issuers` / `expected_audience`.
- [x] Use the control-plane AuthKit verifier for standalone user/admin tokens.

---

# #SEC-6: constant-time-api-key-compare

**Priority:** high
**Files:** internal/http/middleware/apikey.go

Superseded by SEC-12 / issue 220 hard cut. The X-API-KEY service-auth path should be removed entirely rather than repaired; there is no deprecated or legacy API-key support after the mTLS cutover.

**Tasks:**
- [x] Delete `internal/http/middleware/apikey.go` if no non-service use remains
- [x] Remove X-API-KEY service-auth tests instead of updating timing behavior
- [x] Verify SEC-12 mTLS tests cover rejection of API-key-only service requests

---

# #SEC-7: proration-rounding-fix

**Status:** implemented — verified in code 2026-06-12 (automated review). `CalculateProration` computes remaining time in seconds and rounds UP to the nearest whole day, with the policy documented in a code comment (internal/modules/checkout/service.go ~2153).

**Priority:** medium
**Files:** internal/modules/checkout/service.go

CalculateProration truncates to whole days, causing $0 proration near period end. Should compute in hours/seconds and round UP to the nearest day (1h left = 1 day of charge). Prevents free upgrades near period boundary.

**Tasks:**
- [ ] Change CalculateProration to compute remaining time in seconds/hours instead of truncated days
- [ ] Round UP: any partial day remaining counts as 1 full day of proration charge
- [ ] Minimum proration: if daysRemaining rounds to 0 but time > 0, charge 1 day
- [ ] Update the proration test to verify edge case: upgrade with 1 hour left charges for 1 day
- [ ] Document the rounding policy in a code comment

---

# #SEC-8: dunning-hard-decline-stop

**Status:** implemented — verified in code 2026-06-12 (automated review). NMI decline codes are categorized hard-vs-soft (internal/modules/subscriptions/dunning.go `DeclineHard`), hard declines terminate without further retries and emit a high-visibility log (internal/river/jobs_dunning.go).

**Priority:** medium
**Files:** internal/modules/subscriptions/dunning.go, internal/river/jobs_dunning.go

FailMembership / dunning retries should only apply on NMI and must check the decline error code. Hard declines (account closed, stolen card, do_not_honor) must stop retries immediately. Only soft declines (insufficient_funds, try_again_later) should retry.

**Tasks:**
- [ ] Parse the NMI decline response code from the rebill attempt result
- [ ] Categorize decline codes into hard-decline vs soft-decline:
-     - Hard (stop immediately): account_closed, stolen_card, do_not_honor, card_expired, pickup_card
-     - Soft (allow retry): insufficient_funds, try_again_later, processing_error
- [ ] On hard decline: immediately transition subscription to cancelled/failed, do NOT schedule another retry
- [ ] On soft decline: continue existing retry logic (72h interval, max 5 attempts)
- [ ] Log the decline reason code for operational visibility
- [ ] Emit alert/metric when hard decline terminates a subscription

---

# #SEC-9: operator-admin-db-check

**Status:** superseded/implemented — verified in code 2026-06-12 (automated review). The #312 hard cut replaced claims-trusting admin checks with live, request-time permission evaluation: `AdminPermissionChecker.HasAdminPermission` / `PlatformSuperadminChecker` (internal/auth/policy/admin.go), so a revoked admin loses authority immediately rather than at JWT expiry.

**Priority:** medium
**Files:** internal/auth/policy/admin.go

IsOperatorAdmin currently trusts JWT org_roles claims without a DB cross-check. A revoked admin retains authority until their JWT expires. Add a DB verification on write endpoints to prevent stale-JWT privilege abuse.

**Tasks:**
- [ ] For write endpoints (refunds, entitlement grants, off-channel payments, catalog mutations):
-     - After JWT claim check passes, query the DB to confirm the user still holds the admin role
- [ ] Cache the DB result for a short TTL (e.g., 30s) to avoid per-request DB hit on read endpoints
- [ ] If DB says revoked but JWT says admin: reject with 403 and log a security event
- [ ] Keep the claims-only fast path for read-only admin endpoints (GET /admin/*)

---

# #SEC-10: nmi-tier-upgrade-atomicity

**Priority:** high
**Files:** internal/modules/checkout/service.go

The NMI tier upgrade flow (processUpgrade) charges the card at step 2, cancels old sub at step 3, then does DB writes at steps 4-5. If step 4 or 5 fails, the user is charged, old sub is cancelled at NMI, new sub is active at NMI billing every period — but the local DB has no record. Must add proper rollback/compensation for post-charge failures.

**Tasks:**
- [ ] Before any processor calls, persist a pending_upgrade record (idempotency-protected) with all step state
- [ ] After each step succeeds, update the pending record with step completion status
- [ ] If DB write at step 4/5 fails:
-     - Attempt to refund the proration charge at NMI
-     - Attempt to cancel the new NMI subscription
-     - Attempt to reactivate the old NMI subscription (or at minimum flag for manual intervention)
- [ ] Add a recovery worker that periodically scans for stuck pending_upgrade records and resumes/compensates
- [ ] Add idempotency key to the NMI proration sale call to prevent double-charging on retry
- [ ] Add integration test that simulates DB failure after charge and verifies compensation fires

---

# #SEC-11: eliminate-newraw-pattern

**Status:** implemented — verified in code 2026-06-12 (automated review). No `NewRaw` usages remain in the tree; the audit package uses parameterized sqlc queries. The lint-rule follow-up folds into the broader #334 sqlc migration.

**Priority:** low
**Files:** internal/audit/checks_subscription_state.go, internal/audit/checks_payment_method.go, internal/audit/checks_temporal.go

Replace db.NewRaw(...) usage with proper parameterized queries or direct Bun query builder methods. The NewRaw-then-reserialize pattern in the audit package drops parameter bindings and is a future SQL injection risk if anyone adds placeholders.

**Tasks:**
- [ ] In checks_subscription_state.go: replace db.NewRaw(q.String()).Scan(...) with q.Scan(ctx, &results) directly
- [ ] Audit all files in internal/audit/ for the same pattern and fix
- [ ] Add a golangci-lint custom rule or code review policy that flags fmt.Sprintf inside NewRaw arguments
- [ ] Consider using Bun's typed query builder instead of NewRaw for all audit queries

---

# #SEC-12: mtls-service-to-service

**Priority:** medium
**Files:** internal/http/middleware/apikey.go, internal/http/routes/routes.go, cmd/openrails/main.go, docs/mtls-vault-pki.md

Replace the shared X-API-KEY bearer-secret model for service-to-service auth with mTLS using HashiCorp Vault PKI certs. This eliminates the single-secret-equals-full-access problem and enables per-service identity without key rotation downtime. Tracked in implementation issue 220. This is a hard cut: remove API-key service auth entirely, with no deprecated compatibility path or legacy fallback. The default standalone service listener should be `SERVICE_MTLS_PORT=2054`, separate from public `PORT=2053`, replacing the old `PRIVATE_PORT=8060` model. Local Docker Compose/dev should use a Vault PKI profile/service, not OpenRails-owned root CA or self-signed certificate generation.

**Tasks:**
- [x] Set up HashiCorp Vault PKI secret engine for issuing short-lived service/client certs
- [x] Add Docker Compose support for local mTLS through a HashiCorp Vault dev PKI profile/service; do not generate an OpenRails-owned root CA or self-signed cert chain
- [x] Configure the billing service TLS listener on `SERVICE_MTLS_PORT=2054` to require and verify client certificates
- [x] Map client cert CN/SAN to a service identity with defined scopes (credits:read, credits:write, etc.)
- [x] Update RegisterServiceRoutes to check the resolved service identity's scopes instead of a shared key
- [x] Remove API-key service-auth support entirely: delete middleware/config/docs/tests and reject API-key-only service requests
- [x] Document the cert rotation lifecycle and Vault PKI configuration
- [x] Add integration test with Vault PKI-issued test certs verifying mutual auth handshake and service-scope authorization

---

# #SEC-13: credit-deposit-overflow-guard

**Status:** implemented — verified in code 2026-06-12 (automated review). Overflow guard before the addition in depositTx (internal/modules/credits/credits_service.go ~399).

**Priority:** low
**Files:** internal/modules/credits/credits_service.go

In credits_service.go depositTx(), the line `newBal := bal.Balance + params.Amount` has no int64 overflow check. A MaxInt64 deposit on a user with balance > 0 wraps the balance negative, permanently bricking their credit account. NOTE: This is entirely dependent on already having the shared API key — and if an attacker has the key they can already withdraw any user's balance directly, making this strictly worse for them. Not a real exploit, just a good preventative coding hygiene step.

**Tasks:**
- [ ] Add overflow guard before the addition at line ~253:
-     if params.Amount > math.MaxInt64-bal.Balance {
-         return nil, fmt.Errorf("deposit would overflow balance")
-     }
- [ ] Import math if not already imported
- [ ] Add unit test: deposit MaxInt64 on a user with balance=1, assert error returned
- [ ] Consider adding a configurable max single-deposit cap as a secondary safeguard

---

# #SEC-14: upgrade-pgx-dependency

**Status:** done — go.mod bumped to github.com/jackc/pgx/v5 v5.9.2 in this change (was v5.9.0 after an earlier partial upgrade; tracker filed against v5.7.6). Build and unit tests pass.

**Priority:** high
**Files:** go.mod

Upgrade github.com/jackc/pgx/v5 from v5.7.6 to v5.9.2+. Fixes 4 genuine CVEs: 2x HIGH (pgproto3 malicious server crash) and 2x LOW (client-side SQL sanitization injection). While exploitation requires a malicious/compromised Postgres server, the fix is a version bump with no breaking changes.

**Tasks:**
- [ ] Run: go get github.com/jackc/pgx/v5@v5.9.2
- [ ] Run: go mod tidy
- [ ] Run full test suite to verify no regressions
- [ ] Verify go.sum updated with new hashes

---

# #SEC-15: upgrade-x-net-dependency

**Status:** implemented — verified 2026-06-12 (automated review): go.mod already pins golang.org/x/net v0.55.0.

**Priority:** low
**Files:** go.mod

Upgrade golang.org/x/net from v0.47.0 to v0.55.0+. Fixes 5 MEDIUM CVEs (XSS in html tokenizer + HTTP/2 client infinite loop). All are unreachable in this codebase (no HTML rendering, HTTP/2 client loop requires compromised payment processor), but removes Snyk noise and is good hygiene.

**Tasks:**
- [ ] Run: go get golang.org/x/net@v0.55.0
- [ ] Run: go mod tidy
- [ ] Run full test suite to verify no regressions

---
