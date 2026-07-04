OpenRails is a self-hostable billing server that makes adding billing to your vibe-coded app safe and trivial. It can be used as a standalone service or embedded into your Go application (it's a library and an application). Both options cost you $0.

OpenRails is perfect for:

- SaaS products: Your merchants buy a plan, and we bill them per seat. Merchants can upgrade/downgrade their plan-tier with proration.
- Adult sites: Your users buy recurring subscriptions; we handle the subscription-lifecycle (prevent duplicate subscriptions, cancellation, manual-dunning, etc.) Build your own OnlyFans, Pornhub, etc.
- Digital Storefronts: Your users buy videos / courses / downloads individually. Define your own products + prices; we manage ownership-records. Build your own Shopify, Gumroad, etc.
- Video Games: in-game transactions.

OpenRails supports many payment rails:

- Stripe
- Any NMI-compatible payment-gateway (PaymentCloud, PayKings, SoarPay, Zen Payments, Corepay, etc.; there are literally thousands of these companies).
- Solana with USDC (crypto)

OpenRails is one system to rule them all; one API to integrate. Reduces integration time from weeks to hours.

OpenRails was built to help break the parasitic monopoly Visa + MasterCard currently hold upon America's financial lives. The pain is especially acute for 'high risk' businesses (crypto, porn, gambling, THC / CBD, etc.) who are banned from all 1st-tier payment providers. OpenRails makes it much easier to integrate with 'high risk payment gateways', which have really shitty APIs usually, and lack all of the dev-ex nicities you get with Stripe and other 1st-tier payment gateways.

---

### Required Services

Requires a Postgres 18+ instance to use (can be shared with your web-server). Optionally also uses Redis for rate-limiting.

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
| Deployment | Separate HTTP service (own process, port `:3053`) | Compiled into your Go binary |
| Your backend calls it via | HTTP (`/v1/merchant/*`, API key) | In-process function calls (`pkg/service`) or HTTP |
| Your frontend calls it via | HTTP, browser-direct (`/v1/me/*`, delegated token) | HTTP routes mounted on **your** server, **your** credential |
| Auth | OpenRails verifies tokens at its network edge | You hand OpenRails an identity; it never sees a credential |
| Language requirement | None — any stack that speaks HTTP | Host must be Go |
| Database | Owns its schema (can share your Postgres instance) | Shares your `pgx` pool (or connects itself) |

Pick **standalone** when OpenRails should be its own service, when non-Go services need it,
or when one OpenRails instance serves multiple applications (multi-merchant). Pick **embedded**
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
- **Standalone:** OpenRails is a separate system across a network boundary, and it always
  runs its own AuthKit control plane — the in-process authority that issues and verifies
  these credentials, holds the runtime merchant/issuer registry, and gates admin routes.
  (There is no control-plane-less "verifier-only" standalone; private/self-hosted
  registration is the only standalone mode in this repo. Public hosted registration
  belongs in the private OpenRails SaaS layer.)
  Identity claims that cross that boundary must be independently verifiable, so each caller
  class gets a credential scoped to exactly what it may do:
  - your **backend** uses an **API key** (`openrails_st_...`) or a first-party OIDC
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
4. **Least privilege.** Delegated tokens carry only the OpenRails audience, the acting
   delegated subject, an optional narrow OpenRails permission set, and a short TTL.
   Your session token can do everything your app allows; it should never be spendable
   as a billing credential.

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
| Backend / server-to-server | Service token (`/v1/merchant/*`) | In-process call — no credential |
| Browser self-service | Delegated token, minted by your backend (`/v1/me/*`) | Your session credential, via `DelegatedAuthenticator` |
| User billing routes | AuthKit user JWT (AuthKit-backed deployments) | Your session credential, via `Authenticator` |
| Merchant/admin routes | Live `merchant:*` permissions, checked per request | Same (requires the control plane) |

---

## Integration guide: standalone service

### 1. Run it

```bash
task docker-up            # Postgres + Garnet(Redis) + OpenRails, zero-config
curl http://localhost:3053/health
```

The public API listens on `:3053`: public routes, the self-service surface,
merchant routes, and webhooks all share the port. See
[docs/api/endpoints.md](docs/api/endpoints.md) for the full HTTP reference and
[docs/merchant-provisioning.md](docs/merchant-provisioning.md) for creating your merchant and
its first API key.

### 2. Backend integration (API keys)

Your backend authorizes its user however it normally does, then calls OpenRails
server-to-server with its API key. The high-traffic surface is credits/usage:

```bash
# Pre-authorize + place a hold atomically before doing expensive work
curl -X POST https://openrails.example/v1/merchant/admissions \
  -H "Authorization: Bearer openrails_st_..." \
  -d '{"items":[{"customer_id":"...","invoker":"user-123",
       "estimated_amount":50000,"request_id":"req-789"}]}'

# Settle the real cost (or POST .../holds/{id}/release on failure)
curl -X POST https://openrails.example/v1/merchant/admissions/{id}/capture \
  -H "Authorization: Bearer openrails_st_..." \
  -d '{"amount":43000,"usage":{"event_type":"chat.completion"}}'
```

API keys carry explicit `merchant:*` permissions and are bound to your merchant —
a key can never act on another merchant's data. The `/v1/merchant/*` surface
covers admission (`/admissions`), trust-tier policies, merchant settings,
credit windows, usage rollups, and the issuer registry used in the next step.

### 3. Frontend integration (delegated tokens)

Your users are not OpenRails users — they are *subjects of your merchant*. The browser talks
to OpenRails directly using a short-lived delegated token that **your backend signs**.

**3a. Register your remote application (one-time setup).** The host application
is registered in the AuthKit/control-plane model as an owner of the merchant group.
That stored authority determines which OpenRails permissions the host can put on
delegated JWTs; OpenRails rejects over-claimed delegated tokens.

**3b. Mint delegated tokens on your backend.** Add one endpoint to your API that exchanges
a logged-in session for a delegated token. The claim contract:

```jsonc
{
  "iss": "https://api.yourapp.com",          // your registered issuer
  "aud": ["openrails"],                       // a registered audience
  "delegated_sub": "user-123",                // YOUR user id — becomes the billing subject
  // permissions omitted for /v1/me/*; add customer:* grants only for
  // /v1/customers/:customer_id/* shared customer-treasury routes.
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
    // No permissions needed for /v1/me/* self-service.
    TTL:              5 * time.Minute,
})
```

Grant only the permissions the page needs. `/v1/me/*` is authenticated
self-service and needs no grant; shared customer treasury routes require
explicit `customer:*` grants:

| Permission | Allows |
|---|---|
| none | Call `/v1/me/*` as the token's own `delegated_sub` |
| `customer:balance:read` | Read a shared customer's balance, transactions, usage, payments, and invoices |
| `customer:billing:update` | Set a shared customer's billing mode and spend caps |
| `customer:payment-methods:update` | Manage a shared customer's payment methods and Stripe portal |
| `customer:checkout:create` | Pre-pay / load credits onto a shared customer balance |
| `customer:spend-delegations:read` | Read a shared customer's spend-delegation policy |
| `customer:spend-delegations:update` | Replace a shared customer's spend-delegation policy |

**3c. Call the self-service API from the browser.** Have your frontend fetch the delegated
token from your exchange endpoint (cache it, re-fetch on expiry — a ~30-line helper makes
this invisible to the rest of your client code), then hit `/v1/me/*` directly:

```
GET  /v1/me/status                      balance + account overview
GET  /v1/me/transactions                credit transactions
GET  /v1/me/usage                       metered usage rolled up by event type
GET  /v1/me/invoices[/:id]              monthly itemized statements
GET  /v1/me/subscriptions               own subscriptions
POST /v1/me/subscriptions/:id/cancel    cancel/resume/change-tier
GET|POST|PUT|DELETE /v1/me/payment-methods
POST /v1/me/checkout                    hosted/tokenized checkout session
```

There is no `:user_id` anywhere on this surface — every route is scoped to the token's
`delegated_sub`, so a browser token can only ever act on its own subject. Browser origin
policy for delegated calls is configured on the AuthKit `remote_application` issuer record,
not in OpenRails runtime config. Staff/support and machine actions live under
`/v1/merchant/*` and are gated by `merchant:*` permissions.

### 4. Webhooks

Point each payment rail's webhook at OpenRails directly (Stripe/NMI/CCBill →
OpenRails, not through your app). OpenRails verifies rail signatures, updates
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
            Merchant:      claims.Merchant,      // optional merchant context
            MerchantRoles: claims.MerchantRoles, // optional roles within Merchant
            Roles:        claims.Roles,        // optional host-level roles
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
    Config:  cfg,
    PGXPool: myPool,  // share your pgx pool, or omit to let OpenRails connect from cfg
    Redis:   myRedis,
})
if err != nil {
    log.Fatal(err)
}
defer openrails.Close(ctx)

// Background workers: renewals, dunning, credit/hold expiry, reconciliation.
go openrails.RunWorkers(ctx)

// Mount the billing surface. Routes live under /billing/v1/*.
// Zero value uses embedded.EmbeddedDefaultRouteSets:
// checkout, customer, merchant admin, catalog, and webhooks.
handler, err := embedded.MountHandler(openrails, embedded.MountOptions{
    MountPrefix: "/billing",
    Authenticator: myAuth,
    Gate: myGate,
    DelegatedAuthenticator: myDelegatedAuth,
    RouteSets: []embedded.RouteSet{
        embedded.RouteSetCheckout,
        embedded.RouteSetCustomer,
        embedded.RouteSetMerchantAdmin,
        embedded.RouteSetCatalog,
        embedded.RouteSetWebhooks,
    },
})
if err != nil {
    log.Fatal(err)
}
mux := http.NewServeMux()
mux.Handle("/billing/", handler) // plain net/http; or gin.WrapH(handler) / chi Mount
```

The handler is framework-neutral `net/http` — OpenRails links no web framework at all
(#670). Gin/chi/echo hosts mount the same handler with their framework's wrapper
(`gin.WrapH(handler)`, `chi.Mount`, …).

Your frontend now calls these routes with its **normal session credential**.
`Authenticator` protects checkout/user routes; `Gate` protects merchant routes.

**Rate-limiting & captcha (built in).** The embedded surface runs OpenRails' own
per-IP/per-user rate limiting + captcha **in-process** — you do not need to front it
with your own gateway. It is on but **inert until you set `Config.RateLimits`** (a
safe no-op otherwise). Set it on the config you pass to `embedded.New`/`embed.New`
(Redis-backed when `Options.Redis` is set, in-memory per-process otherwise):

```go
cfg.RateLimits = &config.RateLimitsConfig{
    "default":   {RequestsPerMinute: 300},  // general API (SPA/NAT-friendly)
    "checkout":  {RequestsPerMinute: 10},   // tight: deters card-testing
    "subscribe": {RequestsPerMinute: 20},
    "payment":   {RequestsPerMinute: 40},
    "webhook":   {RequestsPerMinute: 1200}, // per source IP; absorbs rail bursts
}
// Optional captcha escalation on extreme abuse (needs your Turnstile/reCAPTCHA keys):
// cfg.Captcha = &config.CaptchaConfig{Provider: config.CaptchaProviderTurnstile,
//     SiteKey: "...", SecretKey: "..."}
```

Buckets key per-IP **and** per-authenticated-user (strictest wins), and apply to both
the user surface and the delegated self/admin surface (`embgin.SelfHandler`). The
values above mirror the standalone defaults. **Embedding hosts: do this when you wire
the engine — it's easy to forget, and without it billing endpoints are unthrottled.**

### 3. Call OpenRails in-process

Skip HTTP entirely on hot paths — e.g. metered usage:

```go
svc, _ := openrails.Service()

// Pre-authorize before doing expensive work…
hold, _ := svc.HoldCredits(ctx, service.HoldCreditsRequest{
    Invoker: userID, CreditType: "api_credits", Amount: 100,
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

The browser-direct self-service surface (`/v1/me/*`, `/v1/customers/:customer_id/*`) exists in
embedded mode too — authenticated by **your** credential, not a delegated token. Implement
`billingauth.DelegatedAuthenticator`: verify the request however you like, then return the
explicitly mapped principal:

```go
myDelegatedAuth := billingauth.DelegatedAuthenticatorFunc(
    func(ctx context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
        user, err := myIdP.Verify(r.Header.Get("Authorization")) // your own session check
        if err != nil {
            return nil, billingauth.ErrUnauthenticated
        }
        return &billingauth.DelegatedPrincipal{
            // Resolve this explicitly from your deployment/session.
            MerchantID:  "a5a5a5a5-0000-4000-8000-000000000001",
            SubjectID:   user.ID,                       // the billing subject (= delegated_sub)
            Issuer:      "https://auth.yourapp.com",    // audit: who vouched
            // Permissions are optional for /v1/me/*; use customer:* only for
            // shared customer treasury routes.
        }, nil
    })
```

The mapping is **explicit and fail-closed**: an empty merchant or subject is rejected with
401. This interface is the in-process twin of
the standalone delegated token: same translation, no wire credential, because there is no
wire. The self surface is mounted by the gin/standalone handler (`embgin.StandaloneHandler`); the
plain `NewHTTPHandler` mux carries the user/admin/webhook groups.

### 5. Admin routes

Admin authority is **live `merchant:*` permission in the caller's merchant group**,
checked per request against the control plane. OpenRails never interprets your role names,
and there is no role-string fallback. The standalone service always runs the control plane;
for embedded hosts it is opt-in
(`pkg/embedded/controlplane.Attach`); if you don't attach one, omit
`embedded.RouteSetMerchantAdmin` and run admin operations through the in-process `Service()` facade
or your own tooling instead — admin routes without a permission checker fail closed.

---

## How it integrates with your app

- **Premium access:** read `billing.entitlements` (current time ∈ `[start_at, end_at)` and
  `revoked_at IS NULL`). Don't infer premium status from subscription rows.
- **User identity:** OpenRails treats the subject id (`UserContext.UserID` embedded,
  `delegated_sub` standalone) as an opaque principal — it is your user id, and OpenRails
  keys billing state to it verbatim. Identity attributes (email, username) are optional,
  non-authoritative metadata for things like checkout prefill.
- **Admin authority:** live `merchant:*` permissions evaluated at request time in
  the caller's merchant group (see embedded guide §5). Never derived from role names.
- **Sandbox vs live:** `TEST_MODE=true` routes every rail to its test/sandbox
  environment and enforces sandbox credentials so you can't accidentally charge a real
  card. It defaults on in development, must be explicit for live local runs, and is
  rejected outside development (see Operating modes below).

## Configuration

Zero-config against the bundled compose stack. Override with a `config.yaml` (repo root or
`./config/`) or env vars (koanf mapping, e.g. `DB_URL` → `db.url`,
`AUTH_ISSUER` → `auth.issuer`). See `config.example.yaml`
and `.env.example`.

## Operating Modes

Two orthogonal settings:

- **`provider_write_mode`** (yaml) / `PROVIDER_WRITE_MODE` (env) /
  `--provider-write-mode` (CLI flag; flag beats env beats yaml) —
  the pure **behavior** dial: how much OpenRails may do against the payment providers.
  One of `full | limited | readonly`. The old `mode` / `MODE` / `--mode` alias is
  removed (#710) — a set key fails loudly at load.
- **`test_mode`** (yaml) / `TEST_MODE` (env) / `--test-mode` (CLI flag) — the
  **credential** axis: `true` enforces sandbox rail credentials. Default `false`.

| `PROVIDER_WRITE_MODE=` | What runs |
|---|---|
| `full` | Full behavior — charges, dunning, deletes all run. |
| `limited` | **Reactive-only provider writes.** Nothing system-initiated touches a provider: no dunning charges, no auto-top-ups, no arrears collection, no Solana pulls, no catalog provider-object writes (provider slots defer to `pending_manual_link` and converge on a later apply). Local dunning decisions still materialize: stale past-due subscriptions cancel/downgrade locally, and in-window charges become parked system-origin intents. Everything user/admin-initiated works — checkout charges, card/vault saves, tier changes, cancels (including their rail-side delete), resumes, refunds, webhooks. |
| `readonly` | **Zero provider writes, even reactive ones** — a checkout/charge attempt fails loudly (`ErrProviderReadOnly`). Wire-enforced on all three rails: NMI (direct-post gate), Stripe (transport gate), Solana (transaction-submission gate). Provider *reads* (query APIs, catalog verification) and local serving still work. For reconciliation/forensics boots. |

### `test_mode` — sandbox credentials, orthogonal to provider write mode

`TEST_MODE=true` is sandbox money with whatever behavior `provider_write_mode` selects
(typically `full`): every rail routes to its test/sandbox environment, and the
**credential guarantees** attach — a live Stripe key (`sk_live_`/`rk_live_`) refuses to boot; each
configured NMI account is probed at boot with one auth on the non-issued test card —
only a simulator can approve it, so a decline proves a live account and refuses the boot
(NMI sandbox accounts are otherwise undetectable — same URL, unmarked keys). Probe
verdicts are cached for 12h in `billing.probe_verdicts` keyed by sha256 of the key
(#348): a fresh `live` verdict refuses the boot from cache without re-probing — a
crash-looping supervisor pays one declined auth total, not one per restart — a fresh
`simulated` verdict skips the probe, and a rotated key or stale verdict always
re-probes (cache failures degrade to probing); CCBill uses
`sandbox-api.ccbill.com`; Solana derives devnet structurally. `test_mode=true` is
**rejected outside `env=development`** — sandbox money is dev-only. The old `mode=test`
is exactly `TEST_MODE=true` + `PROVIDER_WRITE_MODE=full`.

At a glance — what each provider write mode permits (the `test_mode` axis applies orthogonally: with
`TEST_MODE=true` the same matrix holds against sandbox rails, so no real money can
move in any mode):

| Operation | `full` | `limited` | `readonly` |
|---|---|---|---|
| Real money can move | ✅ | ✅ | ❌ (writes blocked) |
| User checkout / charge | ✅ | ✅ | ❌ fails loudly |
| Card/vault save, tier change, resume, refund | ✅ | ✅ | ❌ |
| User/admin cancel → rail-side delete | ✅ | ✅ | ❌ marker left for replay |
| Dunning charges + window-expiry cancellations | ✅ | ❌ runs dry | ❌ |
| Auto-top-ups, arrears collection, Solana pulls | ✅ | ❌ | ❌ |
| Catalog provider-object writes (`push-merchant-catalog`) | ✅ | ❌ deferred | ❌ deferred |
| Provider reads (query APIs, catalog verification) | ✅ | ✅ | ✅ |
| Webhook ingestion + local serving | ✅ | ✅ | ✅ |

Each mode is strictly more restrictive than the one before it (`readonly` ⊃ `limited`):
`limited` draws the line at *who initiates* (the system initiates nothing; humans get
everything), `readonly` draws it at *the wire* (nothing writes to a provider, not even a
customer clicking buy). Typical uses: `limited` = migration cutover with the site fully
usable; `readonly` = reconciliation/forensics boots that must only observe.

`provider_write_mode` is **required outside development** (the server refuses to boot
without one); unset in dev defaults to `full`. `test_mode` defaults to sandbox in
development and live outside development. Set `TEST_MODE=false` (or `--test-mode=false`)
for a deliberate live local run. The old `mode=test` and `mode=production` values no
longer exist.

The dunning **schedule** is not a knob — it is a hardcoded function of the price's
billing cycle (#359):

| Billing cycle | Retry schedule (offsets from the failed rebill) |
|---|---|
| < 4 days (daily) | none — the first failed rebill is terminal |
| 4–27 days (weekly-ish) | +1d, +2d (3 attempts total) |
| ≥ 28 days (monthly, yearly) | +2d, +5d, +9d, +13d — progressive, 5 attempts over ~2 weeks (capped: yearly is never more generous than monthly) |

The staleness window ("never charge a months-old failure") is **derived** from the same
schedule: dunning may only charge within `period_end` + the last retry offset + one day
of slack (14 days for monthly subs, 3 days for weekly, zero for daily). Anything older
is cancelled + downgraded **without** a charge — a card that failed months ago is never
surprise-charged by a catch-up run.

**Silence is owned too (#632/#665/#368).** Dunning covers the failures we *saw*; a
subscription whose period lapses with NO webhook either way (lost success webhook,
provider billing on its own day boundary, dead webhook pipe) is covered by two
mechanisms working together. First, access doesn't cut off at the period-end second:
every activation/renewal pre-appends a bounded, revocable **grace window** (half the
billing cycle, capped at 72h; 12h for daily — not a knob) that any resolution revokes,
and that a deliberate cancel deletes (no generosity for explicit cancellation). Second,
convergence parks the evidence-less row as `unknown` (access intact, #664) and the
4-hourly **unknown-cohort reconcile** verifies it against the provider (read-only —
it never charges): verified charge → renewed and the payment backfilled exactly once;
declined/stalled → routed into dunning (or cancelled with a deferred provider delete
once past the window); remote alive with a future billing date → the remote period end
is adopted (no access granted); remote gone → cancelled locally with entitlements
revoked. Unreachable providers just leave rows `unknown` for the next pass. Runs under
`full` and `limited`; skipped under `readonly`. Details in docs/operations.md.

### Safe boot with production credentials

Booting against real rail accounts (e.g. a migration cutover or reconciliation run),
set **before first start** — imported stale `past_due` subscriptions are immediately "due"
and full-behavior modes would start charging them within hours:

```bash
PROVIDER_WRITE_MODE=limited
```

(or `PROVIDER_WRITE_MODE=readonly` for a strictly-observing boot where even user checkouts must fail.)

Exit path, in order: (1) unset `DISABLE_RAIL_SUBSCRIPTION_DELETIONS` and restart —
deletes skipped while the switch was on parked as pending intents on the provider intent
ledger (#358) and the scheduled intent executor drains them; (2) once converged,
set `PROVIDER_WRITE_MODE=full` — dunning resumes, and the derived staleness window guarantees the stale
backlog is cancelled + downgraded rather than charged.

All paused work is **delayed, not lost** — the workers are state-scan loops, so the first
enabled run processes whatever is still outstanding (low balances top up, owed arrears
collect, due Solana subscriptions pull). Missed periods are never back-billed: a Solana
subscription that skipped whole periods gets exactly ONE pull with the new period anchored
at the pull moment (the on-chain program independently caps pulls at one plan-amount per
period), and dunning past the window cancels instead of charging.

Under `PROVIDER_WRITE_MODE=limited` the paused backlog is also **visible, not just implied** (#366): the
dunning scan still runs and *materializes* its decisions — months-stale past_due subs
(the migration-import shape) are cancelled + downgraded locally right away (no charge),
and in-window charges are enqueued as
**parked** system-origin intents. So after a migration the cutover sequence is: boot
`limited` → the first dunning cycle materializes the backlog → `openrails intents` shows
the real drain forecast → review (and `refingerprint` if credentials moved) → `PROVIDER_WRITE_MODE=full`
drains exactly what you saw. Under `readonly` nothing materializes — that mode stays a
pure observer for forensics.

## Consistency checks & corrections

**The Convergence Engine** — OpenRails keeps every grant effect (entitlements,
credits, product access), subscription lifecycle state, and internal accounting
consistent with the source events, *continuously*. It runs inline after a mutation
(scoped to the affected customer) and on a 15-minute background sweep; a clean
scope does zero writes. There is no separate `audit` command and no enforce crank —
convergence is always on. Findings carry a self-describing four-plane slug —
`pull.*` (provider-observed truth), `derive.*` (source → grant → grant effect),
`life.*` (clock / state machine), `consistency.*` (internal accounting /
references) — replacing the old `PS-*` / `P-E-*` codes. The model and the full
taxonomy live in [docs/consistency-invariants.md](docs/consistency-invariants.md).

**`pull-provider`** — pull the payment rails' state (the source of truth for
observed charges / refunds / disputes / subscription / vault state) into the local
mirror, then run one idempotent `Converge` pass. Safety-first: a bare
`pull-provider` is a **DRY-RUN** (discovers divergences, logs, writes nothing);
`--overwrite` applies the mirror upserts; `--prune` additionally deletes local rows
absent from the provider source (account-bound, ledger-safe). Every pull resolves +
verifies the provider account and **aborts on a credential/account mismatch**.

```bash
openrails pull-provider --merchant=<slug>                       # DRY-RUN: fetch + diff + log, zero writes
openrails pull-provider --merchant=<slug> --overwrite           # apply mirror upserts, then post-pull Converge
openrails pull-provider --merchant=<slug> --overwrite --prune   # also delete provider-absent local rows
openrails pull-provider report --merchant=<slug>                # latest run: open + admin-pending + held findings
openrails pull-provider report --merchant=<slug> --run=<uuid>   # a specific run

# common flags:
#   --merchant=<slug-or-id>
#   --provider=nmi,ccbill,stripe,solana   (default: all configured)
#   --provider-account=<uuid>             (target one provider account explicitly)
#   --since=2026-01-01 --until=2026-06-01 (historical backfill window; default: head)
#   --format=table|json
```

See [docs/operations.md](docs/operations.md) for the finding taxonomy and the
confirmed-absence gate (destructive repairs are held until the source domain is
fully reconciled). `pull-provider` records local mirror plans/changes only; remote
provider writes executed later through provider intents are recorded separately in
`openrails intents-log`.

**`intents`** — read-only view of the provider intent ledger (#358): every queued
outbound provider mutation. Under `MODE=limited`/`readonly` this is the dry-run
view of what the executor will drain when the mode lifts — user/admin-origin
intents execute under `limited`, system-origin only under `full`, nothing under
`readonly`.

```bash
openrails intents --merchant=<slug>                         # pending intents + drain forecast by mode
openrails intents --merchant=<slug> --status=all            # full ledger history
openrails intents --merchant=<slug> --type=nmi_delete_subscription --format=json
openrails intents-log --merchant=<slug> --provider=stripe   # remote provider mutation attempts/results
openrails intents-log --merchant=<slug> --intent=<uuid> --format=json

# No public HTTP route is exposed for this ledger in core OpenRails.
```

Intents are bound to the provider ACCOUNT they were enqueued against (an
account fingerprint stamped at enqueue): swapping credentials to a different
NMI/Stripe account parks the pending queue instead of executing it against the
wrong account. After a confirmed same-account credential change, re-stamp with
`openrails intents refingerprint --provider=<name> --merchant=<slug> --yes`.
See docs/operations.md ("Account guard / credential rotation").

### The two pending-work queues

Before a cutover (raising `MODE=limited`/`readonly` to `full`), there are exactly two
places where deferred work accumulates. Check both:

**1. What fires automatically when the mode is raised** — the provider intent ledger.
Every outbound provider mutation OpenRails wanted but couldn't execute under the
current mode is parked here as `pending`, and the executor drains it the moment the
mode allows. This is the dry run for "what happens when we let it loose":

```bash
openrails intents --merchant=<slug>          # pending rows + the drain forecast footer:
                                           #   "N execute under mode=limited, M require mode=full"
# No public HTTP route is exposed for this ledger in core OpenRails.
```

If the forecast shows something you do NOT want to fire (e.g. a backlog of dunning
charges), resolve it before raising the mode — the staleness window already protects
against months-old charges, but review is cheap and irreversible mistakes aren't.

**2. What never fires automatically — the admin findings queue.** Findings whose
fix requires a *remote* mutation or a judgment call (`pull.subscription.missing`
ghost subscriptions, `pull.dispute.chargeback`, `pull.subscription.duplicate`
needing cancel + refund at the rail, a `derive.grant.excess` refunded-payment
grant) are queued for a human and stay `requires_review` (or `reconcile_required`
behind the confirmed-absence gate) until acted on. Raising the mode does nothing
to this queue — that is the safety design, not an oversight:

```bash
openrails pull-provider report --merchant=<slug>   # open + admin-pending + held findings of the latest run

# No public HTTP route is exposed for this queue in core OpenRails.
```

Work the queue by doing the remote action yourself at the rail (cancel, refund,
investigate), then `ack` the finding with notes; `dismiss` records a false positive.
The next pull + Converge independently verifies reality converged.

## Documentation

- **HTTP API reference:** [docs/api/endpoints.md](docs/api/endpoints.md)
- **Operations manual:** [docs/operations.md](docs/operations.md) — provider consistency, the durability model, dunning, safety levers, and `openrails pull-provider` (dry-run by default; `--overwrite`/`--prune` + post-pull Converge): the manual batch truth-pull that overwrites the local mirror from the payment rails and converges local state (#107/#511; never writes to a provider).
- **Entitlements model:** [docs/entitlements_timeline.md](docs/entitlements_timeline.md)
- **Merchant provisioning & API keys:** [docs/merchant-provisioning.md](docs/merchant-provisioning.md)
- **Testing with business time:** [docs/business-time.md](docs/business-time.md)
- More runbooks (Solana, NMI sandbox, vault secrets, reconciliation) under `docs/`.

## Developer tasks

```bash
task dev      # run locally
task build    # -> bin/openrails
task test
task docker-up / docker-down / docker-logs
```

## Money units (#494)

Native-money integer fields are named as amounts, balances, limits, or thresholds;
their precision is implied by the row/request currency. USD and EUR use
micro-units; other currencies use the precision registered for that currency
(for example JPY uses 1/10,000 yen). `_cents` remains reserved for payment-gateway
boundaries only: NMI/CCBill charges, refunds, and top-ups. Human-authored config
uses dollar strings ("$0.05") parsed once at load. Budget windows are FIXED
per-user-anchored (session or fixed cadence — see internal/modules/budgets),
never rolling.

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
