OpenRails is a self-hostable billing server that makes adding billing to your vibe-coded app safe and trivial. It can be used as a standalone service or embedded into your Go application (it's a library and an application). Both options cost you $0.

OpenRails is perfect for:

- SaaS products: Your tenants buy a plan, and we bill them per seat. Tenants can upgrade/downgrade their plan-tier with proration.
- Adult sites: Your users buy recurring subscriptions; we handle the subscription-lifecycle (prevent duplicate subscriptions, cancellation, manual-dunning, etc.) Build your own OnlyFans, Pornhub, etc.
- Digital Storefronts: Your users buy videos / courses / downloads individually. Define your own products + prices; we manage ownership-records. Build your own Shopify, Gumroad, etc.
- Video Games: in-game transactions.

OpenRails supports many payment processors:

- Stripe
- Any NMI-compatible payment-gateway (PaymentCloud, PayKings, SoarPay, Zen Payments, Corepay, etc.; there are literally thousands of these companies).
- Solana with USDC (crypto)

OpenRails is one system to rule them all; one API to integrate. Reduces integration time from weeks to hours.

OpenRails was built to help break the parasitic monopoly Visa + MasterCard currently hold upon America's financial lives. The pain is especially acute for 'high risk' businesses (crypto, porn, gambling, THC / CBD, etc.) who are banned from all 1st-tier payment providers. OpenRails makes it much easier to integrate with 'high risk payment gateways', which have really shitty APIs usually, and lack all of the dev-ex nicities you get with Stripe and other 1st-tier payment gateways.

---

### Required Services

Requires a Postgres 18+ instance to use (can be shared with your web-server). Optionally also uses Redis for rate-limiting and Clickhouse for analytics.

In embedded (library) mode, OpenRails obviously requires your webserver http-handlers to be written in Go (a great choice btw).

---

## Scope / PCI-Compliance

OpenRails does not store or process customer credit card details; instead we use two flows to avoid this:

- **Redirect flow**: Browser -> payment provider's checkout page (ex. Stripe) -> user enters credit card details -> payment provider redirects back to your frontend. Behind the scenes Stripe webhook -> Your OpenRails server -> updates entitlements in your database.
- **Tokenized-vault flow**: Browser -> sends credit card details directly to your payment provider -> browser receives a token in response -> browser sends the token to OpenRails -> OpenRails sends the charge + token to the payment provider -> payment provider charges the card and returns the result to OpenRails -> OpenRails updates entitlements in your database.

As a result, self-hosted instances only need to meet PCI compliance requirements of SAQ-A, which is a self-assessment + annual questionnaire.

---

## Two ways to run it

| | **Standalone service** | **Embedded library** |
|---|---|---|
| Deployment | Separate HTTP service (own process, port `:2053`) | Compiled into your Go binary |
| Your backend calls it via | HTTP (`/v1/service/*`, service token) | In-process function calls (`pkg/service`) or HTTP |
| Your frontend calls it via | HTTP, browser-direct (`/v1/self/*`, delegated token) | HTTP routes mounted on **your** server, **your** credential |
| Auth | OpenRails verifies tokens at its network edge | You hand OpenRails an identity; it never sees a credential |
| Language requirement | None — any stack that speaks HTTP | Host must be Go |
| Database | Owns its schema (can share your Postgres instance) | Shares your `pgx` pool (or connects itself) |

Pick **standalone** when OpenRails should be its own service, when non-Go services need it,
or when one OpenRails instance serves multiple applications (multi-tenant). Pick **embedded**
for one-binary deployments where your app is the only consumer. The two modes expose the
same billing engine and the same route surface; the difference is **where the trust boundary
sits**, which is also what decides how authentication works in each mode — read the next
section first, it makes both guides below make sense.

---

## The auth model: one credential or two?

The rule across both modes is: **one credential per trust domain.**

- **Embedded:** your app and OpenRails are the same process — one trust domain. The frontend
  uses its normal session credential for everything, including the mounted billing routes.
  Your code verifies it and hands OpenRails the resulting identity through a Go interface.
  **One token.** OpenRails never parses your credential at all.
- **Standalone:** OpenRails is a separate system across a network boundary. Identity claims
  that cross that boundary must be independently verifiable, so each caller class gets a
  credential scoped to exactly what it may do:
  - your **backend** uses a **service token** (`openrails_st_...`) or a first-party OIDC
    service JWT — server-to-server, never sent to browsers;
  - your **frontend** uses a short-lived **delegated access token** that *your own backend*
    mints and signs — browser-direct, self-service-scoped.

So in standalone mode the browser does hold two tokens: its normal session token for your
API, and a delegated token for OpenRails. **This is deliberate, not incidental.** The
alternative — OpenRails accepting your webserver's session JWTs directly — was considered
and rejected for four reasons:

1. **Your session tokens would leave your trust domain.** Every billing call would ship a
   full-power webapp credential to another system. If that system (or its logs) is ever
   compromised, the attacker holds tokens that unlock *your* API. A delegated token is
   worthless anywhere except the OpenRails self-service surface — a fully compromised
   OpenRails yields nothing replayable against you.
2. **Every session leak would become a billing leak.** Session tokens pass through many
   hands (browser extensions, analytics, your own microservices). Today none of those
   exposures touch billing; with pass-through acceptance, all of them would.
3. **Audience discipline.** A JWT recipient must reject tokens not addressed to it
   (RFC 7519 `aud`). Accepting foreign-audience tokens is the classic confused-deputy
   anti-pattern, and OpenRails fails closed on it: a token carrying a normal `sub` is
   rejected on sight.
4. **Least privilege.** Delegated tokens carry only `openrails:self:*` permissions and a
   short TTL. Your session token can do everything your app allows; it should never be
   spendable as a billing credential.

The cost is small, because **your backend mints the delegated token itself** — with the
same signing key it already uses for its own auth, if you like. "Getting a token for
OpenRails" is one authenticated fetch to *your own* API, not a separate login or a
round-trip to a foreign identity provider. Wrap it in a token-exchange endpoint plus a
small frontend helper that fetches and auto-refreshes, and client code sees one system
(this is the same shape as Stripe's ephemeral keys or Plaid's link tokens). The full flow
is in the standalone guide below.

The two modes have exact design parity here: both translate *your* credential into a billing
principal at the trust boundary. Embedded does the translation through an in-process
interface (`billingauth.Authenticator` / `DelegatedAuthenticator`); standalone does the same
translation as a signed wire artifact (the delegated token, verified against your registered
JWKS). Same seam, two serializations.

| Surface | Standalone credential | Embedded credential |
|---|---|---|
| Backend / server-to-server | Service token (`/v1/service/*`) | In-process call — no credential |
| Browser self-service | Delegated token, minted by your backend (`/v1/self/*`) | Your session credential, via `DelegatedAuthenticator` |
| User billing routes | AuthKit user JWT (AuthKit-backed deployments) | Your session credential, via `Authenticator` |
| Admin routes | Live `openrails:admin` permission, checked per request | Same (requires the control plane) |

---

## Integration guide: standalone service

### 1. Run it

```bash
task docker-up            # Postgres + Garnet(Redis) + ClickHouse + OpenRails, zero-config
curl http://localhost:2053/health
```

The public API listens on `:2053`: user routes, the self-service surface, admin routes,
webhooks, and the server-to-server `/v1/service/*` routes all share the port. See
[docs/api/endpoints.md](docs/api/endpoints.md) for the full HTTP reference and
[docs/tenant-provisioning.md](docs/tenant-provisioning.md) for creating your tenant and
its first service token.

### 2. Backend integration (service tokens)

Your backend authorizes its user however it normally does, then calls OpenRails
server-to-server with its service token. The high-traffic surface is credits/usage:

```bash
# Pre-authorize + place a hold atomically before doing expensive work
curl -X POST https://openrails.example/v1/service/credits/authorize \
  -H "Authorization: Bearer openrails_st_..." \
  -d '{"tenant_subject_id": "...", "actor": "user-123", "credit_type": "api_credits",
       "estimate_micros": 50000, "request_id": "req-789"}'

# Settle the real cost (or POST .../holds/{id}/release on failure)
curl -X POST https://openrails.example/v1/service/credits/holds/{id}/capture \
  -H "Authorization: Bearer openrails_st_..." \
  -d '{"amount": 43000, "event_type": "chat.completion"}'
```

Service tokens carry explicit permissions (`openrails:credits:write`,
`openrails:credits:read`, `openrails:catalog:write`, …) and are bound to your tenant —
a token can never act on another tenant's data. Other `/v1/service/*` groups cover
admission/rate-limiting (`/admit`, `/budget`, `/tier-policies`), account settings, credit
windows, usage rollups, and the issuer registry used in the next step.

### 3. Frontend integration (delegated tokens)

Your users are not OpenRails users — they are *subjects of your tenant*. The browser talks
to OpenRails directly using a short-lived delegated token that **your backend signs**.

**3a. Register your issuer (one-time setup).** Tell OpenRails which signing keys speak for
your tenant. Publish a JWKS endpoint (you almost certainly already have one for your own
auth) and register it:

```bash
curl -X POST https://openrails.example/v1/service/tenant/issuers \
  -H "Authorization: Bearer openrails_st_..." \
  -d '{"issuer": "https://api.yourapp.com",
       "jwks_uri": "https://api.yourapp.com/.well-known/jwks.json",
       "audiences": ["openrails"]}'
```

The tenant is bound from your service token, so you can only register issuers for your own
tenant; issuer strings are globally unique, which is what makes cross-tenant token forgery
impossible. `POST /v1/service/tenant/issuers/disable` is the per-issuer kill switch;
key *rotation* is just re-`POST`ing with the new JWKS.

**3b. Mint delegated tokens on your backend.** Add one endpoint to your API that exchanges
a logged-in session for a delegated token. The claim contract:

```jsonc
{
  "iss": "https://api.yourapp.com",          // your registered issuer
  "aud": ["openrails"],                       // a registered audience
  "delegated_sub": "user-123",                // YOUR user id — becomes the billing subject
  "permissions": ["openrails:self:billing:read",
                  "openrails:self:checkout:create"],
  "iat": 1760000000,
  "exp": 1760000300                           // keep it short; minutes, not hours
}
// No "sub" claim — a token with a normal `sub` is rejected as not-delegated.
// Sign RS256 with a `kid` header that resolves in your registered JWKS.
```

Go backends can use the AuthKit helper instead of hand-rolling claims:

```go
import authhttp "github.com/open-rails/authkit/http"

token, err := authhttp.MintDelegatedAccessToken(ctx, signer, authhttp.DelegatedAccessParams{
    Issuer:           "https://api.yourapp.com",
    Audiences:        []string{"openrails"},
    DelegatedSubject: user.ID,
    Permissions:      []string{"openrails:self:billing:read", "openrails:self:checkout:create"},
    TTL:              5 * time.Minute,
})
```

Grant only the permissions the page needs:

| Permission | Allows |
|---|---|
| `openrails:self:billing:read` | Read own balance, credits, usage, invoices, subscriptions, payments |
| `openrails:self:billing:write` | Configure own account settings (billing mode, caps, auto-top-up) |
| `openrails:self:checkout:create` | Create own checkout sessions |
| `openrails:self:subscriptions:cancel` | Cancel / resume / change-tier own subscriptions |
| `openrails:self:payment-methods:manage` | Add / update / remove own payment methods |
| `openrails:self:wallets:manage` | Manage own Solana wallet link |

**3c. Call the self-service API from the browser.** Have your frontend fetch the delegated
token from your exchange endpoint (cache it, re-fetch on expiry — a ~30-line helper makes
this invisible to the rest of your client code), then hit `/v1/self/*` directly:

```
GET  /v1/self/status                      balance + account overview
GET  /v1/self/credits[/:type]             credit balances and transactions
GET  /v1/self/usage                       metered usage rolled up by event type
GET  /v1/self/invoices[/:id]              monthly itemized statements
GET  /v1/self/subscriptions               own subscriptions
POST /v1/self/subscriptions/:id/cancel    …cancel/resume/change-tier
GET|POST|PUT|DELETE /v1/self/payment-methods
POST /v1/self/checkout                    hosted/tokenized checkout session
```

There is no `:user_id` anywhere on this surface — every route is scoped to the token's
`delegated_sub`, so a browser token can only ever act on its own subject. CORS origins for
browser-direct calls are configured per tenant. A parallel `/v1/tenant-admin/*` surface
exists for your staff, using the same token mechanism with `openrails:tenant:*` permissions.

### 4. Webhooks

Point each payment processor's webhook at OpenRails directly (Stripe/NMI/CCBill →
OpenRails, not through your app). OpenRails verifies processor signatures, updates
subscriptions/entitlements, and your app just reads the results. See
[docs/cloudflared-webhooks.md](docs/cloudflared-webhooks.md) for exposing webhooks in dev.

---

## Integration guide: embedded library

### 1. Bring your auth

Implement one interface — any credential scheme works (JWT, session cookie, API key,
gateway header). OpenRails never inspects your tokens; it consumes the identity you return:

```go
import "github.com/open-rails/openrails/pkg/billingauth"

var myAuth billingauth.Authenticator = billingauth.AuthenticatorFunc(
    func(ctx context.Context, r *http.Request) (billingauth.UserContext, error) {
        claims, err := myIdP.Verify(r.Header.Get("Authorization"))
        if err != nil {
            return billingauth.UserContext{}, billingauth.ErrUnauthenticated
        }
        return billingauth.UserContext{
            UserID:       claims.Subject,      // required: the payer/principal (opaque to OpenRails)
            Email:        claims.Email,        // optional
            Username:     claims.Username,     // optional
            Tenant:       claims.Tenant,       // optional: empty if you have no tenant model
            Roles:        claims.Roles,        // optional
            Entitlements: claims.Entitlements, // optional
        }, nil
    })
```

### 2. Initialize, mount, and run workers

```go
import (
    "github.com/open-rails/openrails/config"
    "github.com/open-rails/openrails/pkg/embedded"
)

cfg, _ := config.Load()
openrails, err := embedded.New(embedded.Options{
    Config:        cfg,
    PGXPool:       myPool,  // share your pgx pool, or omit to let OpenRails connect from cfg
    Redis:         myRedis,
    Authenticator: myAuth,  // nil => default AuthKit-backed verifier built from cfg
})
if err != nil {
    log.Fatal(err)
}
defer openrails.Close(ctx)

// Background workers: renewals, dunning, credit/hold expiry, reconciliation.
go openrails.RunWorkers(ctx)

// Mount the billing surface. Routes live under /billing/v1/*.
//   user routes  → products, prices, checkout, subscriptions, payments, credits
//   admin routes → subscription/payment/user management, metrics (see §5)
//   webhooks     → processor callbacks
handler := openrails.NewHTTPHandler(embedded.HTTPHandlerOptions{
    IncludeUser:     true,
    IncludeAdmin:    true,
    IncludeWebhooks: true,
})
mux := http.NewServeMux()
mux.Handle("/billing/v1/", handler) // plain net/http; or gin.WrapH(handler) / chi Mount
```

The handler is framework-neutral `net/http` with zero gin on the request path. Hosts on gin
can instead use `pkg/embedded/gin` (`embgin.RegisterUserRoutes(e, group, …)` /
`embgin.Handler(e)` for the full standalone surface).

Your frontend now calls these routes with its **normal session credential** — your
`Authenticator` is the only gate. One system, one token.

### 3. Call OpenRails in-process

Skip HTTP entirely on hot paths — e.g. metered usage:

```go
svc, _ := openrails.Service()

// Pre-authorize before doing expensive work…
hold, _ := svc.HoldCredits(ctx, service.HoldCreditsRequest{
    Actor: userID, CreditType: "api_credits", Amount: 100,
    Source: "api_call", SourceID: requestID,          // idempotent on (type, source, id)
    ExpiresAt: time.Now().Add(5 * time.Minute),
})

// …then settle the real cost, or release on failure.
svc.CaptureHold(ctx, service.CaptureHoldRequest{HoldID: hold.ID, Amount: actualCost})
// svc.ReleaseHold(ctx, hold.ID)   // if the operation failed

// Read entitlements to gate premium features.
ents, _ := svc.ListActiveEntitlements(ctx, userID, time.Now())
```

### 4. Browser self-service in embedded mode (one credential)

The browser-direct self-service surface (`/v1/self/*`, `/v1/tenant-admin/*`) exists in
embedded mode too — authenticated by **your** credential, not a delegated token. Implement
`billingauth.DelegatedAuthenticator`: verify the request however you like, then return the
explicitly mapped principal:

```go
opts.DelegatedAuthenticator = billingauth.DelegatedAuthenticatorFunc(
    func(ctx context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
        user, err := myIdP.Verify(r.Header.Get("Authorization")) // your own session check
        if err != nil {
            return nil, billingauth.ErrUnauthenticated
        }
        return &billingauth.DelegatedPrincipal{
            // Single-tenant deployments use the well-known default tenant id.
            TenantID:    "00000000-0000-0000-0000-000000000001",
            SubjectID:   user.ID,                       // the billing subject (= delegated_sub)
            Actor:       "https://auth.yourapp.com",    // audit: who vouched
            Permissions: []string{"openrails:self:billing:read",
                                  "openrails:self:checkout:create"},
        }, nil
    })
```

The mapping is **explicit and fail-closed**: an empty tenant or subject is rejected with
401, and a principal carrying any non-`self`/`tenant` permission is refused — the same
catalog gate real delegated tokens pass through. This interface is the in-process twin of
the standalone delegated token: same translation, no wire credential, because there is no
wire. The self surface is mounted by the gin/standalone handler (`embgin.Handler`); the
plain `NewHTTPHandler` mux carries the user/admin/webhook groups.

### 5. Admin routes

Admin authority is the **live `openrails:admin` permission in the caller's own tenant**,
checked per request against the control plane — OpenRails never interprets your role names,
and there is no role-string fallback. The control plane is opt-in for embedded hosts
(`pkg/embedded/controlplane.Attach`); if you don't attach one, mount with
`IncludeAdmin: false` and run admin operations through the in-process `Service()` facade
or your own tooling instead — admin routes without a permission checker fail closed.

---

## How it integrates with your app

- **Premium access:** read `billing.entitlements` (current time ∈ `[start_at, end_at)` and
  `revoked_at IS NULL`). Don't infer premium status from subscription rows.
- **User identity:** OpenRails treats the subject id (`UserContext.UserID` embedded,
  `delegated_sub` standalone) as an opaque principal — it is your user id, and OpenRails
  keys billing state to it verbatim. Identity attributes (email, username) are optional,
  non-authoritative metadata for things like checkout prefill.
- **Admin authority:** the live `openrails:admin` permission evaluated at request time in
  the caller's own tenant (see embedded guide §5). Never derived from role names.
- **Sandbox vs live:** `MODE=test` (the dev default) routes every processor to its
  test/sandbox environment so you can't accidentally charge a real card; outside
  development an explicit mode is required (see Operating modes below).

## Configuration

Zero-config against the bundled compose stack. Override with a `config.yaml` (repo root or
`./config/`) or env vars (koanf mapping, e.g. `DB_URL` → `db.url`,
`AUTH_EXPECTED_AUDIENCE` → `auth.expected_audience`). See `config.example.yaml` and
`.env.example`.

## Operating modes & feature flags

One dial picks how much OpenRails may do against the payment providers: `mode` (yaml) /
`MODE` (env) / `--mode` (CLI flag; flag beats env beats yaml). Fine-grained feature flags under `feature_flags.*` (env: `FEATURE_FLAGS_<NAME>`)
remain as overrides on top — **the strictest setting always wins**. Every restrictive
setting is announced with a `⚠️` warning at startup; if you expected a safety posture and
don't see its warning in the boot log, it isn't on. Full docs in `config.example.yaml`.

| `MODE=` | Money | What runs |
|---|---|---|
| `test` (default in dev) | sandbox | **Full behavior** — charges, dunning, deletes all run against sandbox processors; no real money can move. Rejected outside `env=development`. **Credential guarantees**: a live Stripe key (`sk_live_`/`rk_live_`) refuses to boot; each configured NMI account is probed at boot with one auth on the non-issued test card — only a simulator can approve it, so a decline proves a live account and refuses the boot (NMI sandbox accounts are otherwise undetectable — same URL, unmarked keys); Solana derives devnet structurally. |
| `production` | live | Full behavior. |
| `limited` | live | **Reactive-only.** Nothing system-initiated touches a provider: no dunning charges or window-expiry cancellations (dunning runs dry), no auto-top-ups, no arrears collection, no Solana pulls, no catalog provider-object writes (provider slots defer to `pending_manual_link` and converge on a later apply). Everything user/admin-initiated works — checkout charges, card/vault saves, tier changes, cancels (including their processor-side delete), resumes, refunds, webhooks. |
| `readonly` | live creds | **Zero provider writes, even reactive ones** — a checkout/charge attempt fails loudly (`ErrProviderReadOnly`). Provider *reads* (query APIs, catalog verification) and local serving still work. Implies `limited` + the deletion kill switch. For reconciliation/forensics boots. |

At a glance — what each mode permits:

| Operation | `test` | `production` | `limited` | `readonly` |
|---|---|---|---|---|
| Real money can move | ❌ (sandbox) | ✅ | ✅ | ❌ (writes blocked) |
| User checkout / charge | ✅ sandbox | ✅ | ✅ | ❌ fails loudly |
| Card/vault save, tier change, resume, refund | ✅ sandbox | ✅ | ✅ | ❌ |
| User/admin cancel → processor-side delete | ✅ sandbox | ✅ | ✅ | ❌ marker left for replay |
| Dunning charges + window-expiry cancellations | ✅ sandbox | ✅ | ❌ runs dry | ❌ |
| Auto-top-ups, arrears collection, Solana pulls | ✅ sandbox | ✅ | ❌ | ❌ |
| Catalog provider-object writes (bootstrap apply) | ✅ sandbox | ✅ | ❌ deferred | ❌ deferred |
| Provider reads (query APIs, catalog verification) | ✅ | ✅ | ✅ | ✅ |
| Webhook ingestion + local serving | ✅ | ✅ | ✅ | ✅ |

Each mode is strictly more restrictive than the one before it (`readonly` ⊃ `limited`):
`limited` draws the line at *who initiates* (the system initiates nothing; humans get
everything), `readonly` draws it at *the wire* (nothing writes to a provider, not even a
customer clicking buy). Typical uses: `limited` = migration cutover with the site fully
usable; `readonly` = reconciliation/forensics boots that must only observe.

`mode` is **required outside development** (the server refuses to boot without one);
unset in dev defaults to `test`. The old `test_mode` boolean no longer exists.

Feature-flag dials on top:

| Flag | Default | What it does |
|---|---|---|
| `disable_processor_subscription_deletions` | `false` | Kill switch for outbound NMI `delete_subscription` — **stricter than `limited`**: blocks even the deletes that finalize user-asked cancels. Local cancellation proceeds; each skipped delete leaves a durable `deletion_scheduled_at` marker, and a boot-time rescan re-enqueues all of them once the flag is lifted. Implied by `mode=readonly`. |
| `dunning_mode` | `on` | `on` = retry failed rebills (every 72h, max 5 attempts); `dry_run_only` = workflow runs and logs due subscriptions but never charges (retry state preserved — this is "pause"); `off` = no dunning at all AND rebill failures cancel immediately with no recovery (changes failure semantics — not a pause). |
| `dunning_window_days` | `15` | Dunning may only charge within N days of the missed rebill (`current_period_ends_at`). Anything older is cancelled + downgraded **without** a charge — a card that failed months ago is never surprise-charged by a catch-up run. |
| `disable_entitlement_expiration` | `false` | Freezes local access lifecycle: credit/hold expiry and entitlement revocation pause; users keep premium even after their subscription ends. Orthogonal to the provider-facing settings. |

### Safe boot with production credentials

Booting against real processor accounts (e.g. a migration cutover or reconciliation run),
set **before first start** — imported stale `past_due` subscriptions are immediately "due"
and full-behavior modes would start charging them within hours:

```bash
MODE=limited
FEATURE_FLAGS_DISABLE_PROCESSOR_SUBSCRIPTION_DELETIONS=true
# optional: also freeze local downgrades while reconciling
FEATURE_FLAGS_DISABLE_ENTITLEMENT_EXPIRATION=true
```

(or `MODE=readonly` for a strictly-observing boot where even user checkouts must fail.)

Exit path, in order: (1) unset `DISABLE_PROCESSOR_SUBSCRIPTION_DELETIONS` and restart —
the boot rescan replays every delete skipped while the switch was on; (2) once converged,
set `MODE=production` — dunning resumes, and the dunning window guarantees the stale
backlog is cancelled + downgraded rather than charged.

All paused work is **delayed, not lost** — the workers are state-scan loops, so the first
enabled run processes whatever is still outstanding (low balances top up, owed arrears
collect, due Solana subscriptions pull). Missed periods are never back-billed: a Solana
subscription that skipped whole periods gets exactly ONE pull with the new period anchored
at the pull moment (the on-chain program independently caps pulls at one plan-amount per
period), and dunning past the window cancels instead of charging.

## Documentation

- **HTTP API reference:** [docs/api/endpoints.md](docs/api/endpoints.md)
- **Entitlements model:** [docs/entitlements_timeline.md](docs/entitlements_timeline.md)
- **Tenant provisioning & service tokens:** [docs/tenant-provisioning.md](docs/tenant-provisioning.md)
- **Testing with business time:** [docs/business-time.md](docs/business-time.md)
- More runbooks (Solana, NMI/Mobius sandbox, vault secrets, reconciliation) under `docs/`.

## Developer tasks

```bash
task dev      # run locally
task build    # -> bin/openrails
task test
task docker-up / docker-down / docker-logs
```

## Money units (#337)

Integer money fields MUST carry their unit in the name: `_micros` (micro-dollars,
1e-6 USD — ALL sub-cent amounts: budgets, credits, spend caps, pricing) or
`_cents` (payment-gateway boundaries ONLY: NMI/CCBill charges, refunds, top-ups).
Millicents no longer exist anywhere. Human-authored config uses dollar strings
("$0.05") parsed once at load. Budget windows are FIXED per-user-anchored
(session or fixed cadence — see internal/modules/budgets), never rolling.

### Database queries (sqlc)

All SQL lives in hand-written query files under `internal/db/queries/*.sql`,
compiled by [sqlc](https://sqlc.dev) into the type-safe `internal/db/gen`
package over pgx/v5. There is no ORM, and identifiers are never assembled at
runtime — every query is vetted against a real database schema.

```bash
task sqlc        # regenerate internal/db/gen + vet queries against a real DB
task sqlc-check  # same, then fail if generated code is out of date (CI)
```

Both targets need `SQLC_DATABASE_URL` (defaulting to the local dev DB);
`scripts/sqlc-vet-db.sh` builds a throwaway vet database from the migrations.
