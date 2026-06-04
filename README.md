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
task docker-up            # Postgres + Garnet(Redis) + ClickHouse + billing, zero-config
curl http://localhost:2053/health
```

- **Public API** on `:2053` — user billing routes, admin routes, and webhooks.
- **Server-to-server** calls hit `/v1/service/*` on the same port, authenticated with an
  OpenRails-issued **Operator Access Token** (`Authorization: Bearer <openrails_oat_...>`).
- Your services authorize the user, then call OpenRails to hold/capture/release credits or
  read entitlements.

This is the right mode when billing is a separate microservice, or when non-Go services
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
billing, err := embedded.New(embedded.Options{
    Config:       cfg,
    PGXPool:      myPool,   // share your pools, or omit to let OpenRails connect from cfg
    Redis:        myRedis,
    AuthProvider: authprovider.ProviderFromAuthenticator(myAuth),
})
if err != nil {
    log.Fatal(err)
}
defer billing.Close(ctx)

// Background workers: renewals, dunning, credit/hold expiry, reconciliation.
go billing.RunWorkers(ctx)

// Mount the billing surface anywhere. Routes live under /billing/v1/*.
//   user routes  → products, prices, checkout, subscriptions, payments, credits
//   admin routes → subscription/payment/user management, metrics  (admin-gated)
//   webhooks     → processor callbacks
handler := billing.NewHTTPHandler(embedded.HTTPHandlerOptions{
    IncludeUser:     true,
    IncludeAdmin:    true,
    IncludeWebhooks: true,
})
mux := http.NewServeMux()
mux.Handle("/billing/v1/", handler) // plain net/http; or gin.WrapH(handler) / chi r.Mount
```

**c. Call billing in-process** (no HTTP) for your hot paths — e.g. metered usage:

```go
svc, _ := billing.Service()

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
  OpenRails honors it — it never interprets your role names or invents admin rights. Standalone
  multi-org mode derives the same signal from the operator org's roles.
- **Sandbox vs live:** `test_mode` (default `true`) routes every processor to its
  test/sandbox environment so you can't accidentally charge a real card.

## Configuration

Zero-config against the bundled compose stack. Override with a `config.yaml` (repo root or
`./config/`) or env vars (koanf mapping, e.g. `DB_URL` → `db.url`,
`AUTH_EXPECTED_AUDIENCE` → `auth.expected_audience`). See `config.example.yaml` and
`.env.example`.

## Documentation

- **HTTP API reference:** [docs/api/endpoints.md](docs/api/endpoints.md)
- **Entitlements model:** [docs/entitlements_timeline.md](docs/entitlements_timeline.md)
- **Tenant provisioning & OATs:** [docs/tenant-provisioning.md](docs/tenant-provisioning.md)
- **Testing with business time:** [docs/business-time.md](docs/business-time.md)
- More runbooks (Solana, NMI/Mobius sandbox, vault secrets, reconciliation) under `docs/`.

## Developer tasks

```bash
task dev      # run locally (go run ./cmd/billing server)
task build    # → bin/billing
task test
task docker-up / docker-down / docker-logs
```
