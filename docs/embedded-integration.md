# Embedding OpenRails in your Go server

The full guide to running the OpenRails billing engine in-process. The README's
"Embedded Mode: How To Integrate" is the quickstart; this covers the depth it skips.
All money amounts are **micros** (millionths of a currency unit). Vocabulary: a
**rail** is a gateway kind (`nmi` / `ccbill` / `stripe` / `solana`); a **PSP** is your
concrete account on a rail (e.g. `mobius` on nmi).

### 1. What embedding means

Your Go binary imports the engine and runs it in-process: no second service, no
network hop, no second credential. Concretely:

- The engine owns the `openrails` schema inside **your** Postgres database.
- Its HTTP routes mount on **your** mux under a prefix you choose; your users call
  them with their normal session credential.
- Your backend calls the engine through `rt.Client()` — the **same**
  `openrails.Client` interface a standalone consumer gets from `openrails.NewRemote`.
  Parity is structural: one client implementation, one handler surface, joined by an
  in-process `http.RoundTripper` instead of a socket (enforced by a dual-mode
  conformance test).
- One embedded engine serves **one merchant** (#770).

```mermaid
flowchart LR
    B[Browser] -- your session credential --> S[Your Go server]
    subgraph P[Your process]
        S -- billingauth --> OR[OpenRails engine]
        C[Your backend code] -- openrails.Client --> OR
    end
    OR --> PG[(Postgres, openrails schema)]
    R[Stripe / NMI / CCBill / Solana] -- webhooks --> S
```

### 2. Install and migrations

```bash
go get github.com/open-rails/openrails
```

Migrations ship in the module as an embedded FS. Apply them from your own migration
step with [migratekit](https://github.com/open-rails/migratekit):

```go
import (
    "github.com/open-rails/openrails/config"
    postgresmigrations "github.com/open-rails/openrails/migrations/postgres"
)

migratekit.MigrationSource{
    App:    config.MigratekitApp, // tracking key "openrails"
    FS:     postgresmigrations.FS,
    Schema: "openrails",
}
```

The engine never runs migrations itself — it validates the tracking key at boot and
refuses to start if any migration is missing.

### 3. Config

Embedded mode never runs `config.Load` — you build `*config.Config` programmatically.
Start from `config.GetDefaultBillingConfig()` (seeds dev DB/Redis endpoints, logger,
curated `RateLimits`, and `Captcha`), then set your own values. Construction refuses
to boot unless you declare posture explicitly (#745):

| Field | Required | Meaning |
|---|---|---|
| `Env` | yes | `"development"` / `"staging"` / `"production"`. Empty errors — an empty Env reads as dev and would silently disable hard gates (RLS, secret encryption). |
| `TestMode` | yes | `config.CredentialPostureSandbox` or `config.CredentialPostureLive`. The zero value is UNSET and rejected — it can never silently mean "live". |
| `ProviderWriteMode` | recommended | `config.ProviderWriteModeFull` etc.; unset fail-closes to readonly. |
| `MerchantSource` | defaults to `config.MerchantSourceManifest` | Mode 1 (manifest-is-truth, secrets in memory, reboot to change) vs `MerchantSourceAPI` (mode 2: provision via HTTP APIs + persistent secret store). |
| `DB` | yes | Schema defaults to `openrails`. The **pool you inject must connect as a non-superuser, `NOBYPASSRLS` role** — see below. |

#### The database role you connect as (required)

**The pool you hand OpenRails must connect as a role that is neither a superuser
nor `BYPASSRLS` — in every environment, local development included** (or#782,
or#885). There is no config knob, no dev exemption, and no warn-and-continue.

Why it is a hard gate: OpenRails' merchant isolation is `FORCE ROW LEVEL
SECURITY` keyed on the `app.merchant_id` GUC. A privileged role skips every
policy, so isolation degrades to whatever `WHERE merchant_id = …` predicate each
query happens to carry — and, worse, a query that forgets its merchant scope
returns *rows* instead of the empty result the policy would give it. That is the
"the worker ran and did nothing" class: scheduled work that silently matches
nothing in production while looking healthy on a privileged connection.

Where it is checked: `embedded.New` (and `embed.New`), plus every entry point
that takes a pool or opens one from `Config.DB` — `PushMerchantCatalog`,
`DumpMerchantCatalog`, `ConvergeMerchant`, `PruneRollback`/`PruneList`,
`ImportAdminGrants`, `ImportBilling`, `PullProvider`/`PullProviderReport`. Each
refuses with the role name in the error.

What to do:

1. Run migrations as your owner/admin role (DDL, `GRANT`s and role creation need
   it). `openrails migrate` is the only job that runs privileged.
2. The baseline migration creates `openrails_app` `NOLOGIN NOBYPASSRLS` and
   grants it exactly what the runtime needs: the `openrails` schema, AuthKit's
   `profiles` schema, River's `public.river_*` tables, and `SELECT` on
   `public.migrations`. Attach a login credential out of band
   (`ALTER ROLE openrails_app WITH LOGIN PASSWORD '…'`) — that grants no
   privilege and the role stays `NOBYPASSRLS`.
3. Give OpenRails a pool on that role. Your own application tables are **not**
   granted to `openrails_app`, so either open a **second pool** for OpenRails
   (simplest, and the pools cost nothing at rest) or run your whole app on one
   unprivileged role and grant it on your own schema too.

Any role of your own works as long as it holds the same grants and has neither
`rolsuper` nor `rolbypassrls`. Verify with:

```sql
SELECT rolname, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user;
```

If a query starts failing under the unprivileged role, it is missing its
merchant scope (or a grant) — that is the gate working, not a reason to hand
back the owner role.

Under `TestMode = sandbox` every rail routes to its test environment and live
credentials refuse to boot — no real money can move. NMI accounts get an arm-time
probe: a conclusively-live gateway is refused under sandbox (a probe error only
warns). See [operations.md](operations.md).

**Rate limiting is on by default** (#742): if you leave `RateLimits`/`Captcha` nil,
`embedded.New` seeds the same curated defaults `config.Load` applies — per-IP and
per-authenticated-user buckets, tight on checkout (10/min) to deter card-testing,
Redis-backed when `Redis` is set, in-memory otherwise. Override `cfg.RateLimits`, or
set `cfg.RateLimitsDisabled = true` if your own gateway fronts billing. See
[rate-limiting.md](rate-limiting.md).

### 4. Boot

```go
import (
    "github.com/open-rails/openrails/config"
    "github.com/open-rails/openrails/embed"
    "github.com/open-rails/openrails/pkg/embedded"
)

rt, err := embed.New(ctx, embed.Options{
    Options: embedded.Options{
        Config:  cfg,
        PGXPool: pool, // share your app's pgx/v5 pool; nil = engine opens its own from Config.DB
        Redis:   rdb,  // optional — Redis-backed rate limits; omit for in-memory
    },
    RunWorkers: true,
})
if err != nil { log.Fatal(err) }
defer rt.Close(ctx)
```

| Option | Type | Notes |
|---|---|---|
| `Config` | `*config.Config` | Required. |
| `PGXPool` | `*pgxpool.Pool` | Host-supplied pool (pgx/v5). |
| `Redis` | `*redis.Client` | Optional (rate limits, admission holds). |
| `Cache` | `cache.Cache` | Optional cache override. |
| `RunWorkers` | `bool` | Runs the River background workers (renewals, dunning, credit/hold expiry, reconciliation) on a Runtime-owned goroutine, detached from the ctx you pass to `New` — `Close` stops them. Leave false to drive `rt.RunWorkers(ctx)` yourself. |
| `embed.WithAdminConsole(fs.FS)` | variadic `embed.Option` | Host-built admin console SPA (see §6). |

**Runtime surface**: `rt.Client()` (unified SDK client), `rt.Service()` (engine-native
facade, typed IDs), `rt.Embedded()` (the underlying `*embedded.Embedded` for advanced
wiring), `rt.ActiveRouteSets()` (route groups of the mounted surface),
`rt.RunWorkers(ctx)`, `rt.Close(ctx)`.

**Shared-River pattern** (advanced): a host that already runs its own
[River](https://riverqueue.com) client keeps `RunWorkers: false` and folds OpenRails'
workers + periodic jobs into it — one client, one `public.river_*` table set (River
tables always live in `public`, never the billing schema):

```go
emb := rt.Embedded()
workers := river.NewWorkers()
river.AddWorkerSafely(workers, &MyAppWorker{})
periodic, register, err := embedded.FoldIntoRiver(ctx, emb, workers)

client, err := river.NewClient(driver, &river.Config{
    Workers:    workers,
    Middleware: []rivertype.Middleware{emb.WorkerMiddleware()}, // worker-health rows
    Queues: map[string]river.QueueConfig{
        river.QueueDefault:    {MaxWorkers: 10},
        embedded.QueueBilling: {MaxWorkers: 5}, // MUST be configured or billing jobs never drain
    },
})
if err := register(client); err != nil { ... } // adds periodic jobs + SetRiverClient
client.Start(ctx)
```

Once injected, OpenRails enqueues through your client and never builds its own;
`RunWorkers` becomes a no-op. The three-call form
(`AddWorkersTo` / `GetPeriodicJobs` / `SetRiverClient`) exists for hosts that need
finer control.

**No job clock.** Your `river.Config.JobTimeout` (River's default is one minute)
does not apply to OpenRails' workers: each declares `Timeout() = -1` and ends
on observed lack of progress instead — a job that reports no progress past
3× its declared cadence (floored at 30 min) is cancelled with the reason on the
job row. While a job runs it also beats `river_job.attempted_at`, so your
`RescueStuckJobsAfter` measures silence from a dead process, never the age of
a live job (a dunning pass over many merchants may legitimately outlive it).
Both need the pool you gave OpenRails to be able to write River's tables (it is
the pool River itself writes through).

### 5. Declaring the merchant

Idempotent — call on every boot: create-if-missing, reconcile-if-present. The first
upsert binds the engine to the merchant; a later call with a different slug errors.
Real fields (`embed.MerchantConfig` aliases `internal/bootstrap.MerchantConfig`;
same shape as `config/merchants_config.example.yaml`):

```go
mid, err := rt.UpsertMerchantConfig(ctx, "myapp", embed.MerchantConfig{
    DisplayName: "My App",
    Profile: embed.MerchantProfileConfig{
        DisplayName: "My App Billing",
        FromEmail:   "billing@myapp.example",
        SupportURL:  "https://myapp.example/support",
    },
    Invoice: &embed.InvoiceConfig{ // optional; amounts in micros
        BillingPeriodBoundary: "calendar_month",
    },
    PSPs: map[string]embed.PSPConfig{ // PSP key -> rail -> account
        "my-nmi-sandbox": {
            "nmi": embed.ProviderRailAccountConfig{
                Environment: "test", // assertion, cross-checked against TestMode
                AccountID:   "000000", // NMI dashboard "Gateway ID"
                Settings: map[string]any{ // non-secret knobs
                    "tokenization_url": "https://secure.networkmerchants.com/token/Collect.js",
                    "tokenization_key": "placeholder-tokenization-key",
                },
                Secrets: map[string]string{
                    "security_key":           "placeholder-security-key",
                    "webhook_signing_secret": "placeholder-webhook-secret",
                },
            },
        },
    },
})
```

Semantics by mode:

- **Mode 1 (`merchant_source=manifest`, the default)**: this call IS the manifest —
  it steamrolls the DB projections and seeds secrets into the runtime's **in-memory**
  plane (never a persistent store) on every run, then arms checkout/vault/webhooks
  immediately. Change credentials = change the config + reboot.
- **Mode 2 (`merchant_source=api`)**: a manifest-shaped upsert (PSPs, profile,
  invoice, remote-application trust) refuses loudly — two truths. Only a bare
  identity bind (slug + top-level `DisplayName`) is legal; provision providers via
  `PUT /v1/merchant/payment-providers` and the catalog APIs instead.

YAML-first hosts can keep the merchant in a file: `embed.ParseMerchantConfig` (one
merchant, strict — unknown fields rejected) or `embed.LoadMerchantConfigManifest`
(multi-merchant manifest + `BILLING_MERCHANTS_*` env overlays, so committed files
hold placeholders and real secrets arrive from the environment).

**Catalog push at boot**: products/prices/entitlements converge from a catalog
manifest via `embedded.PushMerchantCatalog`:

```go
err := embedded.PushMerchantCatalog(ctx, embedded.CatalogPushOptions{
    Config:   cfg,
    PGXPool:  pool,
    Manifest: catalogYAML, // or File: "catalog.yaml"
    Insert:   true,        // zero mutation flags = plan-only
})
```

In mode 1 a mutating push always upgrades to full converge (insert+overwrite+prune —
the YAML is the truth); in mode 2 a mutating push refuses (plan-only diff stays
legal). The manifest is `version: 1` + `catalogs: [{merchant, tier_groups, products,
meters, credit_balances, usage_limits}]`.

### 6. Mounting HTTP

OpenRails never parses your credentials. You implement two small `pkg/billingauth`
interfaces over whatever auth you already have:

- `billingauth.Authenticator` → `UserContext` for checkout/user routes. `UserID` is
  **required and MUST be a UUID** (it becomes the payable customer_id; non-UUID
  subjects 401 on required routes and silently downgrade to anonymous on optional
  ones — map non-UUID native ids to a stable UUID). Optional metadata: `Email`,
  `EmailVerified`, `Username`, `Roles`, `Entitlements`, `Merchant`/`MerchantRoles`.
  Adapt closures with `billingauth.AuthenticatorFunc`.
- `billingauth.DelegatedAuthenticator` → `*billingauth.DelegatedPrincipal` for
  `/v1/me/*` and `/v1/customers/*`. `MerchantID` and `SubjectID` are required —
  explicit mapping, no fallbacks, fail-closed 401. Optional: `MerchantSlug`,
  `Issuer` (audit), `Permissions` (trusted verbatim for in-process hosts — grant
  only what you mean), contact metadata. Adapt with
  `billingauth.DelegatedAuthenticatorFunc`.
- **Invoker-scoped principals** (or#930). Set `Invoker` when the credential
  spends a payer's money WITHOUT being the payer — your platform's end user
  drawing on your org's balance under a spend delegation. Use the SAME opaque
  invoker string you pass to admission, so the identity that is metered is the
  identity that reads. OpenRails then narrows the principal to exactly one
  thing, `GET /v1/me/spend-limits`; every other `/v1/me/*` and `/v1/customers/*`
  route refuses it `403 invoker_scoped_principal`, because `SubjectID` there
  names an account the invoker does not own. That guard is what makes it safe to
  map an end-user credential onto a payer account at all — without it the
  self-service surface is all-or-nothing, and hosts correctly reject end-user
  tokens outright.
- `billingauth.Gate` (merchant-admin routes only): `Authorize(ctx, r, permission)
  (Principal, error)` — checks a live `merchant:*` permission per request.

AuthKit hosts should not hand-write these. `pkg/embedded/authkit` ships the
bridges, in two flavours that differ only in where the verifier comes from:

| your situation | use |
|---|---|
| you already have an AuthKit verifier (in-process AuthKit, embedded control plane) | `NewAuthenticator(v, …)` / `NewDelegatedAuthenticator(v, boundMerchantID, …)` |
| you trust a REMOTE issuer over JWKS | `NewVerifierAuthenticator(issuers, aud, …)` / `NewVerifierDelegatedAuthenticator(issuers, aud, boundMerchantID, …)` |

Inject your own verifier whenever you have one: the request is then verified
through `VerifyRequest` — your whole credential chain (API-key branch,
2FA-enrollment gate, issuer enrichment) — so billing cannot end up with a
weaker check than the rest of your app, and an in-process host never refetches
its own keys over HTTP. The merchant pin is YOUR engine's bound merchant in
every flavour, never anything from the caller's token (#913/th#1765).

By default `Permissions` comes from the canonical role→permission preset
`permissions.ForRoles` (owner/admin → `merchant:*` + `customer:*`; member → the
customer self-service set; read-only → its `:read` subset). The options
(or#918):

- `WithAdmission(func(ctx, *http.Request, verify.Claims) error)` — a per-request
  veto that runs after verification, before the principal is built. JWT verify
  is stateless, so a **banned or deleted user keeps a valid token until it
  expires**: put your liveness gate here and every delegated principal,
  `/v1/me` included, is checked. A non-nil error is logged and answered 401;
  the message never reaches the client. `WithUserAdmission` is the same veto on
  the non-delegated authenticator.
- `WithPermissionResolver(func(ctx, *http.Request, verify.Claims) ([]string, error))`
  — permissions from the live request instead of the token's roles, for a grant
  that is a DB read ("is this user a billing admin?") and worth scoping to the
  admin path so the hot self path stays lookup-free. Runs after the admission
  veto. An error fails the request closed. Mutually exclusive with
  `WithRolePermissions` (the simple case: your own role vocabulary).
- `WithMerchantSlug(slug)` — required if principals must address the merchant's
  OWN treasury account by slug (or#916); without it the uuid is the only
  address that resolves.
- `WithIssuer(iss)` — override the audit issuer (default: the token's `iss`),
  e.g. `openrails.SelfIssuer` when your customer rows are keyed to it.
- `WithoutTokenRoles()` (non-delegated) — drop the token's role snapshot from
  `UserContext`. It is stale for the token's lifetime; omit it rather than pass
  a snapshot nothing should authorize on.

Mount everything as one framework-neutral `net/http` handler (gin hosts use
`gin.WrapH`, chi `Mount`, …):

```go
handler, err := embedded.MountHandler(rt.Embedded(), embedded.MountOptions{
    MountPrefix:            "/billing", // routes arrive at /billing/v1/*
    Authenticator:          myAuth,
    DelegatedAuthenticator: myDelegatedAuth,
    // RouteSets: nil,      // = EmbeddedDefaultRouteSets
    // Gate:                // required for RouteSetMerchantAdmin
    // ProviderRoutes:      // *embedded.ProviderRoutes{StripePortal, Solana, Webhooks} — nil derives from armed accounts
})
mux.Handle("/billing/", handler)
```

| RouteSet | Mounts | Default? |
|---|---|---|
| `RouteSetCheckout` | Buyer-facing products, prices, config, checkout | yes |
| `RouteSetCustomer` | `/v1/me/*` self-service + `/v1/customers/:id/*` treasury | yes |
| `RouteSetMerchantAdmin` | Human merchant-admin customer/support routes | yes |
| `RouteSetCatalog` | Merchant catalog routes | yes |
| `RouteSetWebhooks` | Merchant-scoped inbound rail webhooks | yes |
| `RouteSetPaymentProviders` | Provider config + secret routes | opt-in |
| `RouteSetMerchantAPI` | The standalone service/API-key surface (`/v1/merchant/*` over the wire) — most embedded hosts use `Client()` instead | opt-in |

Admin routes **fail closed**: without a `Gate` and an attached control plane
(`pkg/embedded/controlplane.Attach(ctx, rt.Embedded().App(), cfg, pool)` for
AuthKit-backed hosts), omit `RouteSetMerchantAdmin` and run admin operations through
the in-process client.

**Admin console** (optional, #754): the engine ships zero frontend bytes. The host
builds the SPA (`scripts/build-admin-console.sh` from the module cache into a
gitignored `dist`, wrapped in a 3-line `//go:embed all:dist` package) and passes it
via `embed.WithAdminConsole(sub)`; gate mounting on `admin_console.enabled`. See
[admin-console.md](admin-console.md).

### 7. Calling the engine

```go
client := rt.Client()
ctx = openrails.WithMerchant(ctx, mid) // per-call pin; must agree with the bound merchant
if err := openrails.Verify(ctx, client); err != nil { log.Fatal(err) } // fail fast at boot
```

The `openrails.Client` interface, grouped by job:

| Group | Methods |
|---|---|
| Admission (hot path) | `AdmitBatch`, `Capture`, `Release`, `GetTrustLevel`, `ReportWastedSpend` |
| Usage | `RecordUsage` (metered events outside the hold/capture cycle) |
| Policy | `GetMerchantSettings`, `SetMerchantSettings`, `SetCustomerSpendDelegations`, `SetCustomerSpendDelegation` |
| Funding / reporting | `DepositCredits`, `SetCreditLimit`, `GetCreditLimit`, `UsageRollup`, `ResourceRevenueDaily` |
| Lookups / entitlements | `Balance`, `GetCreditAccount`, `ListActiveEntitlements`, `ListEntitlements`, `HasEntitlement`, `ListCustomersWithEntitlement`, `ListProductAccess`, `HasProductAccess` |

```go
verdicts, err := client.AdmitBatch(ctx, []openrails.AdmitRequest{{
    CustomerID:      customerID,
    Invoker:         userID,
    EstimatedAmount: 50_000,    // micros
    RequestID:       requestID, // idempotency key
}})
err = client.Capture(ctx, requestID, 43_000, &openrails.CaptureUsage{EventType: "chat.completion"})
ents, err := client.ListActiveEntitlements(ctx, []string{userID}, time.Now())
```

Entitlement lookups address subjects by the ids your auth system already holds
(self-service users are keyed under `openrails.SelfIssuer`); a user who never touched
billing is an empty slice, never an error. Deny verdicts are `(Allowed=false, nil
error)`.

Extras: the embedded client always implements `embed.SingleAdmitter` (single `Admit`,
no wire counterpart) — reach it via type assertion. `embed.WithCurrency` /
`embed.WithRemoteOptions` tune the client (in-process calls have no default deadline;
opt one back in with `openrails.WithTimeout`). `rt.Service()` is the escape hatch for
engine-native types (`identity.CustomerID` etc.) instead of wire types.

Every `rt.Service()` method pins its own merchant-scoped connection, so a bare Go
call reads the merchant's rows without ceremony — and one with no merchant on the
context fails loudly instead of answering an empty result. Wrapping a *block* of
calls in `emb.RunInMerchant(ctx, …)` is still worth it (one connection for the
whole block instead of one per call), but it is an optimization, not a
prerequisite.

### 8. Acting on delinquency

For arrears billing, OpenRails decides when a payer's unpaid debt has outlived
the merchant's grace window and refuses their new spend at admission — but only
your app can shut off what your app runs. Transitions land on a durable,
acknowledged feed you drain:

```go
events, err := controlplane.ListPendingHostLifecycleEvents(ctx, app, mid, 100)
for _, ev := range events {
    switch ev.EventType {
    case "delinquency.entered": // shut off their resources
    case "delinquency.cleared": // restore them
    }
    _ = controlplane.AcknowledgeHostLifecycleEvent(ctx, app, mid, ev.ID)
}
```

Ack after your action is durable — an unacked event is redelivered. OpenRails
never revokes an entitlement for an unpaid arrears bill. Full boundary and policy:
[arrears-delinquency.md](arrears-delinquency.md).

### 9. Webhooks and ops

Point each rail's webhook at the webhook routes on **your** server, under your mount
prefix (paths in [api/endpoints.md](api/endpoints.md)). OpenRails verifies rail
signatures and updates subscriptions/entitlements; your app just reads the results.
Local rail sandboxes: [dev/local-webhooks.md](dev/local-webhooks.md).

Further reading: [operations.md](operations.md) (operating modes, safety levers,
dunning, the intents ledger), [billing-policies.md](billing-policies.md)
(named spend/credit-line policies and how you bind them),
[arrears-delinquency.md](arrears-delinquency.md),
[rate-limiting.md](rate-limiting.md),
[auth.md](auth.md) (the full one-credential-per-trust-domain rationale),
[self-hosting-mode1.md](self-hosting-mode1.md).
