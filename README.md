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

### 1. Standalone service (self-hosted)

Run OpenRails as its own HTTP service. Your frontend and backend call it over HTTP; it owns
its database and workers.

```bash
task docker-up            # Postgres + Garnet(Redis) + ClickHouse + OpenRails, zero-config
curl http://localhost:2053/health
```

- **Public API** on `:2053` — user billing routes, admin routes, and webhooks.
- **Server-to-server** calls hit `/v1/service/*` on the same port, authenticated with a
  generated OpenRails **service token** (`openrails_st_...`) or a first-party OIDC
  service JWT signed by a registered tenant issuer.
- Your services authorize the user, then call OpenRails to hold/capture/release credits or
  read entitlements.

This is the right mode when OpenRails is a separate service, or when non-Go services
need to call it. See [docs/api/endpoints.md](docs/api/endpoints.md) for the full HTTP API.

### 2. Embedded library (single binary)

Embed OpenRails inside your Go app for one-binary deployment, shared DB pools, and direct
in-process calls. The embedded surface is **framework-neutral** (`net/http`) — mount it in
`net/http`, gin, or chi — and makes **no assumption about your auth stack**.

> **Note:** the embedded library is mid-refactor toward a fully `net/http`, AuthKit-optional
> surface. The API below reflects the new shape (`NewHTTPHandler` + the `billingauth`
> boundary); the older gin `RegisterUserRoutes(*gin.RouterGroup, …)` helpers still exist as
> a transitional path. Admin authority is moving to an **OpenRails-defined capability the
> host sets** (`CanAdministerBilling`) rather than OpenRails interpreting your role names —
> shown below.

**a. Bring your auth** by implementing one tiny interface — any token scheme works:

```go
import "github.com/open-rails/openrails/pkg/billingauth"

// Validate the request however you like, then hand OpenRails an identity.
var myAuth billingauth.Authenticator = billingauth.AuthenticatorFunc(
    func(ctx context.Context, r *http.Request) (billingauth.UserContext, error) {
        claims, err := myIdP.Verify(r.Header.Get("Authorization"))
        if err != nil {
            return billingauth.UserContext{}, billingauth.ErrUnauthenticated
        }
        return billingauth.UserContext{
            UserID: claims.Subject, // required: the payer/principal
            Email:  claims.Email,
            Org:    claims.Org,     // optional: empty if you have no tenants

            // YOU decide who may administer billing. OpenRails never inspects
            // your role *names* — it only honors the capability you set here.
            CanAdministerBilling: claims.HasRole("billing-admin"),

            Entitlements: claims.Entitlements, // optional: drives feature gating
        }, nil
    })
```

**b. Initialize, mount, and run workers:**

```go
import (
    "github.com/open-rails/openrails/config"
    "github.com/open-rails/openrails/pkg/authprovider"
    "github.com/open-rails/openrails/pkg/embedded"
)

cfg, _ := config.Load()
openrails, err := embedded.New(embedded.Options{
    Config:       cfg,
    PGXPool:      myPool,   // share your pools, or omit to let OpenRails connect from cfg
    Redis:        myRedis,
    AuthProvider: authprovider.ProviderFromAuthenticator(myAuth),
})
if err != nil {
    log.Fatal(err)
}
defer openrails.Close(ctx)

// Background workers: renewals, dunning, credit/hold expiry, reconciliation.
go openrails.RunWorkers(ctx)

// Mount the OpenRails surface anywhere.
//   user routes  → products, prices, checkout, subscriptions, payments, credits
//   admin routes → subscription/payment/user management, metrics  (admin-gated)
//   webhooks     → processor callbacks
handler := openrails.NewHTTPHandler(embedded.HTTPHandlerOptions{
    IncludeUser:     true,
    IncludeAdmin:    true,
    IncludeWebhooks: true,
})
mux := http.NewServeMux()
mux.Handle("/openrails/v1/", handler) // plain net/http; or gin.WrapH(handler) / chi r.Mount
```

**c. Call OpenRails in-process** (no HTTP) for your hot paths — e.g. metered usage:

```go
svc, _ := openrails.Service()

// Pre-authorize before doing expensive work…
hold, _ := svc.HoldCredits(ctx, service.HoldCreditsRequest{
    UserID: userID, CreditType: "api_credits", Amount: 100,
    Source: "api_call", SourceID: requestID,          // idempotent on (type, source, id)
    ExpiresAt: time.Now().Add(5 * time.Minute),
})

// …then settle the real cost, or release on failure.
svc.CaptureHold(ctx, service.CaptureHoldRequest{HoldID: hold.ID, Amount: actualCost})
// svc.ReleaseHold(ctx, hold.ID)   // if the operation failed

// Read entitlements to gate premium features.
ents, _ := svc.ListActiveEntitlements(ctx, userID, time.Now())
```

---

## How it integrates with your app

- **Premium access:** read `billing.entitlements` (current time ∈ `[start_at, end_at)` and
  `revoked_at IS NULL`). Don't infer premium status from subscription rows.
- **User email/identity:** OpenRails treats `UserContext.UserID` as an opaque principal. In
  AuthKit-backed deployments it can enrich from `profiles.users`; for any other host, identity
  comes entirely from your `Authenticator`.
- **Admin authority:** the host decides. Set `UserContext.CanAdministerBilling` (a capability
  OpenRails defines; your `Authenticator` populates it from whatever roles/claims you use) and
  OpenRails honors it -- it never interprets your role names or invents admin rights. Standalone
  deployments derive the same signal from bootstrap-managed admin claims.
- **Sandbox vs live:** `test_mode` (default `true`) routes every processor to its
  test/sandbox environment so you can't accidentally charge a real card.

## Configuration

Zero-config against the bundled compose stack. Override with a `config.yaml` (repo root or
`./config/`) or env vars (koanf mapping, e.g. `DB_URL` → `db.url`,
`AUTH_EXPECTED_AUDIENCE` → `auth.expected_audience`). See `config.example.yaml` and
`.env.example`.

## Safety modes & feature flags

Runtime safety controls under `feature_flags.*` (env: `FEATURE_FLAGS_<NAME>`). Every
non-default flag is announced with a `⚠️` warning at startup — if you expected a safety
mode and don't see its warning in the boot log, it isn't on. Full per-flag docs live in
`config.example.yaml`.

| Flag | Default | What it does |
|---|---|---|
| `test_mode` (top-level) | `true` | Routes every processor to its sandbox; you can't charge a real card. Production = `false`. |
| `limited_mode` | `false` | **No proactive action against any payment provider**: dunning attempts no charges and no window-expiry cancellations (demoted to dry-run), auto-top-ups, arrears collection, and Solana recurring pulls are skipped. Everything user/admin-initiated still works — checkout charges, card/vault saves, tier changes, cancels (including their processor-side delete), resumes, refunds, webhooks. |
| `disable_processor_subscription_deletions` | `false` | Kill switch for outbound NMI `delete_subscription` — **stricter than `limited_mode`**: blocks even the deletes that finalize user-asked cancels. Local cancellation proceeds; each skipped delete leaves a durable `deletion_scheduled_at` marker, and a boot-time rescan re-enqueues all of them once the flag is lifted. |
| `dunning_mode` | `on` | `on` = retry failed rebills (every 72h, max 5 attempts); `dry_run_only` = workflow runs and logs due subscriptions but never charges (retry state preserved — this is "pause"); `off` = no dunning at all AND rebill failures cancel immediately with no recovery (changes failure semantics — not a pause). |
| `dunning_window_days` | `15` | Dunning may only charge within N days of the missed rebill (`current_period_ends_at`). Anything older is cancelled + downgraded **without** a charge — a card that failed months ago is never surprise-charged by a catch-up run. |
| `disable_entitlement_expiration` | `false` | Freezes local access lifecycle: credit/hold expiry and entitlement revocation pause; users keep premium even after their subscription ends. Orthogonal to the provider-facing flags. |
| `verify_processor_mappings` | `false` | Catalog applies remotely verify provider-link ids (e.g. that a Stripe `price_id` exists) instead of shape-checking only. |

### Safe mode with production credentials

Booting against real processor accounts (e.g. a migration cutover or reconciliation run),
set **before first start** — imported stale `past_due` subscriptions are immediately "due"
and default flags would start charging them within hours:

```bash
FEATURE_FLAGS_LIMITED_MODE=true
FEATURE_FLAGS_DISABLE_PROCESSOR_SUBSCRIPTION_DELETIONS=true
# optional: also freeze local downgrades while reconciling
FEATURE_FLAGS_DISABLE_ENTITLEMENT_EXPIRATION=true
```

`limited_mode` subsumes `dunning_mode=dry_run_only`, so you don't need that separately.

Exit path, in order: (1) unset `DISABLE_PROCESSOR_SUBSCRIPTION_DELETIONS` and restart —
the boot rescan replays every delete skipped while the switch was on; (2) once converged,
unset `LIMITED_MODE` — dunning resumes, and the dunning window guarantees the stale
backlog is cancelled + downgraded rather than charged.

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
