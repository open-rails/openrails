### Open Rails Billing Service — Operations Manual

#### Scope
- Provides a billing-related API server for the frontend to use (signups, cancellations, etc.), and an admin-API server for the backend to use (admin cancellations).
- Handles webhooks from supported payment processors (Stripe, NMI-backed processors including Mobius, CCBill, Solana), and updates corresponding subscriptions / entitlements.
- Runs periodic jobs to update subscriptions / entitlements.

#### Interactions with other services (Intended Contract)
- Entitlements (app reads from): Billing owns the `billing.entitlements` table and writes premium access windows when memberships start/renew and revokes them on cancel/expiry. The host application can read this table to decide if a user is “premium” at a given point in time (current time ∈ [start_at, end_at) and `revoked_at IS NULL`).

- Profiles (billing reads from): When emailing users (e.g., subscription started/renewed/ended, payment failures, one‑off receipts), Billing reads the current email address from `profiles.users`. We treat user IDs as UUIDs; the service performs a direct, schema‑qualified lookup: `SELECT username, email, email_verified, is_active FROM profiles.users WHERE id = $1`.

---

#### Stack
- Postgres: `postgres:18-bookworm` (DB `doujins_db`, user `admin` / `admin_password`)
- Garnet (Redis-compatible): `ghcr.io/microsoft/garnet` on `6379`
- ClickHouse: `clickhouse/clickhouse-server` (DB `analytics`, user `analytics_user`, pass `analytics_password`)
- Billing service: this server exposing public API on `:2053`; optional service-to-service mTLS API on `:2054` is exposed only to the compose network.

#### Quick Start
- Start services: `task docker-up` (or `docker compose -f docker-compose.yaml up -d`)
- Follow logs: `task docker-logs` (Ctrl+C to stop following)
- Stop services: `task docker-down`

- Postgres bootstrap: `migrations/bootstrap/001_postgres_init.sql` is mounted into `/docker-entrypoint-initdb.d` and runs automatically on first initialization.
- ClickHouse bootstrap: `clickhouse-bootstrap` waits for ClickHouse to be healthy and creates the `analytics` database + user/permissions.
- Billing migrations: `billing-migrate` applies Postgres + ClickHouse migrations.
- Note: ClickHouse migrations are tracked/locked via Postgres (`public.migrations` + advisory locks).
- Billing service connects using built-in defaults that match the compose network/service names.

- Postgres: `postgres://admin:admin_password@postgres:5432/doujins_db?sslmode=disable`

> **Postgres 18:** Local and test databases use Postgres 18 so migrations can use native `uuidv7()`. Existing local Postgres 17 Docker volumes must be recreated for disposable dev data, or upgraded with `pg_upgrade` if the data must be preserved.
- Redis (Garnet): `garnet:6379`, DB `0`
- ClickHouse: `http://clickhouse:8123` with `analytics_user/analytics_password` on DB `analytics`

#### Overriding configuration (optional)
- Config file: place `config.yaml` in repo root or `./config/config.yaml`.
- Env vars: follow the standard koanf mapping used across the stack (e.g. `DB_URL` → `db.url`, `CLICKHOUSE_HTTP_ADDR` → `clickhouse.http_addr`, `AUTH_EXPECTED_AUDIENCE` → `auth.expected_audience`).
- If not provided, the service uses the defaults above.

#### Test Mode (Payment Sandboxes)

The `test_mode` setting controls whether payment processors use sandbox/test environments:

```yaml
test_mode: true   # Default - use sandbox endpoints (safe for testing)
test_mode: false  # Production mode - use real payment endpoints
```

**What test_mode controls:**
- **NMI-backed processors**: Use `sandbox.nmi.com` instead of `secure.networkmerchants.com`
- **CCBill**: Uses `sandbox-api.ccbill.com` instead of `api.ccbill.com`
- **Solana**: Uses devnet instead of mainnet
- **Stripe**: Validates key prefix matches mode (`sk_test_*` vs `sk_live_*`)
- **Webhooks**: Incoming webhooks are still signature-verified where supported (recommended for both sandbox and prod)

**Key behaviors:**
- Defaults to `true` for safety (no accidental charges)
- Stripe is disabled with a warning if key prefix doesn't match test_mode
- Orthogonal to `env` - you can run `env=prod` with `test_mode=true` for staging

**Environment variable:** `TEST_MODE=true` or `TEST_MODE=false`

See `config.example.yaml` and `.env.example` for detailed documentation.

#### Feature Flags (Safety Controls)

Feature flags allow you to quickly disable destructive background operations when bugs are suspected, without requiring a code deployment.

```yaml
feature_flags:
  dunning_mode: "on"                      # on | dry_run_only | off
  disable_entitlement_expiration: false   # true | false
```

**Dunning Mode** (`feature_flags.dunning_mode`):

Controls retry charging for failed subscription rebills.

| Value | Behavior |
|-------|----------|
| `on` (default) | Normal dunning - retry charges every 3 days, up to 5 attempts, then cancel |
| `dry_run_only` | Workflow runs but no charges - subscriptions stay in `past_due`, retry counts preserved |
| `off` | No dunning - immediate cancellation on rebill failure, no grace period |

**Use cases:**
- `dry_run_only`: Bug in charge logic causing incorrect amounts - pause charging while you fix and deploy
- `off`: Dunning workflow itself is broken, or business decision to not do recovery

**Example scenario (dry_run_only):**
1. Nov 1: User's rebill fails → subscription goes to `past_due`, `retry_attempts=1`
2. Nov 3: Dunning disabled (`dry_run_only`) → worker logs but skips charge, state unchanged
3. Nov 7: Bug fixed, set `dunning_mode=on` → worker processes as retry #2

**Disable Entitlement Expiration** (`feature_flags.disable_entitlement_expiration`):

When `true`, stops all entitlement/credit expiration:
- CreditExpiryWorker skips (credit batches preserved even if expired)
- HoldExpiryWorker skips (holds stay active even if expired)
- FailMembership still cancels subscriptions but doesn't revoke entitlements
- Users keep premium access even after subscription ends

**Use case:** Bug in expiration logic causing premature credit/entitlement loss

**Example scenario:**
1. Set `disable_entitlement_expiration: true` when bug discovered
2. Fix the expiration bug, deploy
3. Set `disable_entitlement_expiration: false`
4. All accumulated expirations process at once

**Environment variables:**
```bash
FEATURE_FLAGS_DUNNING_MODE=on              # on, dry_run_only, off
FEATURE_FLAGS_DISABLE_ENTITLEMENT_EXPIRATION=false  # true, false
```

---

## Deployment Modes

Open Rails Billing can run in two modes: **standalone** (as its own HTTP server) or **embedded** (inside another Go application).

### Standalone Mode

Run billing as an independent service with its own HTTP server:

```bash
# Build and run
task build
./bin/billing server

# Or run directly
task dev
```

The standalone server exposes:
- **Port 2053** (public): User APIs, admin APIs, webhooks
- **Port 2054** (service mTLS): Internal service-to-service APIs (credits, entitlements), when `service_mtls.enabled=true`

This is the default mode for production deployments where billing runs as a separate microservice.

### Embedded Mode

Embed billing directly inside another Go application. This is useful when:
- You want a single binary deployment
- Your app needs direct Go API access to billing operations
- You want to control which HTTP routes are exposed

```go
import (
    "github.com/gin-gonic/gin"
    "github.com/open-rails/openrails/config"
    "github.com/open-rails/openrails/pkg/embedded"
)

func main() {
    // Load billing config
    cfg, _ := config.Load()

    // Initialize billing
    billing, err := embedded.New(embedded.Options{
        Config:       cfg,
        AuthProvider: myAuthProvider, // your JWT verifier
    })
    if err != nil {
        log.Fatal(err)
    }
    defer billing.Close(ctx)

    // Start background workers (subscription renewals, dunning, etc.)
    go billing.RunWorkers(ctx)

    // Your app's router
    router := gin.Default()

    // Register only the routes you need...
    // (see "Registering HTTP Routes" below)

    router.Run(":8080")
}
```

#### Registering HTTP Routes

Pick and choose which route groups to expose:

```go
// 1. User routes - frontend billing UI
//    Products, prices, checkout, subscriptions, payments, payment methods,
//    notifications, credits
billing.RegisterUserRoutes(router.Group("/billing/v1"), embedded.RouteOptions{})

// 2. Admin routes - admin dashboard
//    Subscription management, payment management, user management, metrics
//    Requires admin role in JWT
billing.RegisterAdminRoutes(router.Group("/billing/v1/admin"), embedded.RouteOptions{})

// 3. Webhook routes - payment processor callbacks
//    Required if using Stripe, CCBill, or NMI webhooks
billing.RegisterWebhookRoutes(router.Group("/billing/v1/webhooks"))
```

The `RouteOptions{}` uses the `AuthProvider` from `embedded.New()` by default. Override per-group if needed:

```go
billing.RegisterUserRoutes(router.Group("/billing/v1"), embedded.RouteOptions{
    AuthProvider: differentAuthProvider,
})
```

#### In-Process Go API

For internal operations, use the Go API directly instead of HTTP:

```go
svc, err := billing.Service()
if err != nil {
    return err
}

// User operations (same as HTTP API)
products, _ := svc.GetProducts(ctx, service.GetProductsOptions{})
status, _ := svc.GetBillingStatus(ctx, userID)
subscriptions, _ := svc.GetSubscriptions(ctx, userID, service.GetSubscriptionsOptions{})

// Admin operations
metrics, _ := svc.AdminGetMetricsSummary(ctx, service.MetricsOptions{...})
_ = svc.AdminRefundPayment(ctx, paymentID, service.RefundPaymentRequest{...})

	// Credits operations (for usage-based billing)
	// HoldCredits returns a durable hold ID (backed by `billing.credit_transactions`).
	// Use that ID to later capture or release the same hold. Retries with the same
	// (CreditType, Source, SourceID) are idempotent and will return the existing hold.
	hold, _ := svc.HoldCredits(ctx, service.HoldCreditsRequest{
	    UserID:     userID,
	    CreditType: "api_credits",
	    Amount:     100,
	    Source:     "api_call",
	    SourceID:   requestID,
	    ExpiresAt:  time.Now().Add(5 * time.Minute),
	})
	tx, _ := svc.CaptureHold(ctx, service.CaptureHoldRequest{
	    HoldID: hold.ID,
	    Amount: 100,
	})
	_ = svc.ReleaseHold(ctx, hold.ID) // if operation failed

	// Direct credit withdrawal (no hold)
	tx, _ := svc.WithdrawCredits(ctx, service.WithdrawCreditsRequest{
	    UserID:     userID,
	    CreditType: "api_credits",
	    Amount:     50,
	    Source:     "image_generation",
	})

// Entitlements
entitlements, _ := svc.ListActiveEntitlements(ctx, userID, time.Now())
records, _ := svc.ListActiveEntitlementRecords(ctx, userID, time.Now())

// Webhook handling (for custom webhook routing)
result, _ := svc.HandleWebhook(ctx, service.HandleWebhookRequest{
    Provider:  "stripe",
    Body:      rawBody,
    Headers:   map[string]string{"Stripe-Signature": sig},
    ClientIP:  clientIP,
})
```

#### Comparison: Standalone vs Embedded

| Aspect | Standalone | Embedded |
|--------|-----------|----------|
| Deployment | Separate container/binary | Single binary with host app |
| HTTP routing | Fixed public routes on port 2053; optional service mTLS routes on 2054 | You choose which routes to mount |
| Health endpoints | Built-in `/health/*`, `/healthz`, `/readyz` | Host app provides its own |
| Internal ops | mTLS HTTP calls to port 2054 | Direct Go API calls |
| Workers | Built-in, always running | Call `billing.RunWorkers(ctx)` |
| Config | Own config file/env vars | Passed via `embedded.Options` |
| Auth | Own JWT verifier | Use host app's auth provider |

---

Developer tasks
- Build: `task build` → outputs `bin/billing`
- Run (binary): `task run` → builds then runs `billing server`
- Dev (no build): `task dev` → `go run ./cmd/billing server`
- Test: `task test`
- Format: `task fmt`
- Clean: `task clean`

Service endpoints
- Health: `GET http://localhost:2053/health` → `{ "status": "ok", "service": "billing-private" }`
- API base: `http://localhost:2053/v1`
- Auth: JWT-based; supply `Authorization: Bearer <token>` where required by routes.

---

## API Reference

All endpoints return JSON. Authenticated endpoints require `Authorization: Bearer <token>` header.

### Response Formats

**List Response** (paginated collections):
```json
{
  "object": "list",
  "data": [...],
  "total_items": 100,
  "page": 1,
  "page_size": 20,
  "total_pages": 5
}
```

**Error Response** (Stripe-style):
```json
{
  "error": {
    "type": "invalid_request_error",
    "code": "resource_not_found",
    "message": "Subscription not found",
    "param": "subscription_id"
  }
}
```

Error types: `invalid_request_error`, `authentication_error`, `authorization_error`, `api_error`, `card_error`, `rate_limit_error`

---

### Public Endpoints (No Auth Required)

#### Products & Prices

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/v1/products` | List all active products with prices |
| GET | `/v1/prices` | List all active prices |

**Product Object:**
```json
{
  "id": "prod_<uuid>",
  "object": "product",
  "active": true,
  "name": "Premium Monthly",
  "description": "...",
  "prices": [...],
  "created": 1704067200,
  "updated": 1704067200
}
```

> **Note:** Products cannot be deleted. Set `status` to `archived` to hide from listings (grandfathers existing subscribers). Only `display_name`, `description`, and `status` can be updated. (The public Stripe-style object exposes a derived `active` boolean: `true` only when `status=active`.)

**Price Object:**
```json
{
  "id": "price_<uuid>",
  "object": "price",
  "active": true,
  "currency": "usd",
  "unit_amount": 999,
  "product": "prod_<uuid>",
  "type": "recurring",
  "recurring": { "interval": "month", "interval_count": 1 },
  "created": 1704067200
}
```

> **Note:** Prices are mostly immutable. Each price belongs to exactly one product. Financial fields (`unit_amount`, `currency`, `billing_cycle_days`) cannot be changed after creation to preserve historical payment accuracy. To change pricing, create a new price and archive the old one. Only `display_name`, `provider_links` (per-provider link maps), and `status` can be updated. Archiving keeps billing existing subscribers indefinitely. (The public object exposes a derived `active` boolean: `true` only when `status=active`.)

---

## Business Time in Tests

OpenRails billing/domain logic uses an injected runtime clock so integration
tests can advance subscription periods, entitlement windows, dunning retries,
checkout/session expiry, and credit expiry without sleeping. Infrastructure
timing such as cache TTLs, rate limits, signature windows, retry backoff, and
metrics durations may still use wall-clock time.

See [docs/business-time.md](docs/business-time.md) for the testing pattern,
processor test-clock notes, and the guardrail for new `time.Now()` / SQL `NOW()`
usage.

---

### Authenticated Endpoints (Auth Required)

#### User Profile & Status

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/v1/me/status` | Get current user's billing status |

---

#### Subscriptions

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/v1/me/subscriptions` | List user's subscriptions |
| POST | `/v1/me/subscriptions/cancel` | Cancel active subscription |

**Query params for GET /v1/me/subscriptions:**
- `status` - Filter: `active`, `all` (default: `active`)
- `page`, `page_size` - Pagination

**Subscription Object:**
```json
{
  "id": "sub_<uuid>",
  "object": "subscription",
  "status": "active",
  "user": "usr_<uuid>",
  "items": [{
    "id": "si_<uuid>",
    "object": "subscription_item",
    "price": {...},
    "quantity": 1
  }],
  "start_date": 1704067200,
  "current_period_start": 1704067200,
  "current_period_end": 1706745600,
  "cancel_at_period_end": false
}
```

**Cancel Request Body:**
```json
{ "feedback": "Optional cancellation reason (max 500 chars)" }
```

---

#### Payments

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/v1/me/payments` | List user's payment history |

**Query params:**
- `page`, `page_size` - Pagination
- `start_date`, `end_date` - Date range (format: `2006-01-02`)
- `processor` - Filter: `stripe`, `ccbill`, `mobius`, `solana`, `admin`, `manual`
- `min_amount`, `max_amount` - Amount range
- `include_stats` - Include summary stats (default: `false`)
- `include_events` - Include payment events (default: `true`)

---

#### Payment Methods

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/v1/me/payment-methods` | List saved payment methods |
| POST | `/v1/me/payment-methods` | Add new payment method |
| PUT | `/v1/me/payment-methods/:id` | Update payment method |
| DELETE | `/v1/me/payment-methods/:id` | Delete payment method |
| PUT | `/v1/me/payment-methods/:id/activate` | Set as default payment method |

**Create Payment Method Body:**
```json
{
  "payment_token": "tok_xxx",
  "first_name": "John",
  "last_name": "Doe",
  "address1": "123 Main St",
  "city": "New York",
  "state": "NY",
  "zip": "10001",
  "country": "US",
  "phone": "555-1234",
  "email": "john@example.com"
}
```

---

#### Notifications

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/v1/me/notifications` | List user's notifications |
| GET | `/v1/me/notifications/unread-count` | Get unread notification count |
| POST | `/v1/me/notifications/:id/read` | Mark notification as read |

**Query params for GET:**
- `page`, `page_size` - Pagination
- `seen` - Filter: `true`, `false`

---

### Subscription Creation

#### NMI-backed Card Payments

| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/v1/subscriptions/mobius` | Subscribe via NMI/Mobius |
| POST | `/v1/subscriptions/solana` | Subscribe via Solana |

**Request Body:**
```json
{
  "price_id": "<uuid>",
  "payment_method_id": "<uuid>"
}
```

#### CCBill (Redirect Flow)

| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/v1/subscriptions/ccbill` | Generate CCBill FlexForm URL |

**Request Body:**
```json
{
  "price_id": "<uuid>",
  "first_name": "John",
  "last_name": "Doe",
  "address1": "123 Main St",
  "city": "New York",
  "state": "NY",
  "zip_code": "10001",
  "country": "US"
}
```

**Response:**
```json
{
  "url": "https://api.ccbill.com/wap-frontflex/flexforms/...",
  "expires_at": 1704070800
}
```

---

### Webhooks

| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/v1/webhooks/:provider` | Receive webhook from processor (stripe, ccbill, mobius, solana) |

---

### Admin Endpoints

Admin endpoints are under `/v1/admin/*` and require a valid JWT plus an
admin-equivalent identity. The check is performed by `policy.OperatorAdminRequired`,
which has two modes selected by the `auth.operator_org_slug` config:

- **Single-org mode** (default — `operator_org_slug` unset). The caller must
  have the global `admin` role in `profiles.user_roles`. This applies to
  embedded deployments where AuthKit is single-org
  (cozy.art), to self-hosted standalone deployments (doujins / hentai0), and to
  any deployment whose AuthKit instance does not use organizations.

- **Multi-org mode** (`operator_org_slug` set, e.g. `"acme"`). The caller's
  AuthKit JWT must carry `Claims.Org == <operator_org_slug>` AND one of
  `auth.operator_org_admin_roles` in `Claims.OrgRoles` (defaults to
  `["admin", "owner"]`). The OpenRails DB is not consulted in this mode —
  revocation is delegated to short JWT TTL. Use this for tensorhub-style
  deployments where OpenRails is embedded in an app whose AuthKit serves
  multiple organizations and only one of them is the operator.

Host apps populate `UserContext.Org` and `UserContext.OrgRoles` from the
AuthKit JWT in their auth-bridge middleware before forwarding requests into
OpenRails handlers. Empty `Org` is fine for single-org mode.

Subscription / payment / user admin endpoints:

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/v1/admin/subscriptions` | List subscriptions |
| GET | `/v1/admin/subscriptions/:id` | Get subscription |
| POST | `/v1/admin/subscriptions/:id/cancel` | Admin-cancel subscription |
| GET | `/v1/admin/payments` | List payments |
| GET | `/v1/admin/payments/:id` | Get payment |
| POST | `/v1/admin/payments/:id/refund` | Record refund |
| POST | `/v1/admin/users/:user_id/payments/off-channel` | Record off-channel/manual purchase (creates Payment and grants entitlements) |
| GET | `/v1/admin/users/:user_id/entitlements` | List active entitlement windows |
| POST | `/v1/admin/users/:user_id/entitlements` | Grant entitlement (creates admin_grants source record) |
| DELETE | `/v1/admin/users/:user_id/entitlements/:id` | Revoke entitlement |

Admin catalog endpoints (issue #205 — symmetric with `pkg/service` embedded API):

| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/v1/admin/catalog/products` | Create product (reconciles to Stripe by default) |
| GET | `/v1/admin/catalog/products` | List products (filters: `active_only`, `tier_group`, `limit`, `offset`) |
| GET | `/v1/admin/catalog/products/:id` | Get product by ID |
| GET | `/v1/admin/catalog/products/by-slug/:slug` | Get product by slug |
| PATCH | `/v1/admin/catalog/products/:id` | Update mutable product fields |
| POST | `/v1/admin/catalog/products/:id/activate` | Activate product |
| POST | `/v1/admin/catalog/products/:id/deactivate` | Deactivate product |
| POST | `/v1/admin/catalog/prices` | Create price (reconciles to Stripe by default) |
| GET | `/v1/admin/catalog/prices` | List prices (filters: `product_id`, `currency`, `type`, `active_only`, `limit`, `offset`) |
| GET | `/v1/admin/catalog/prices/:id` | Get price |
| PATCH | `/v1/admin/catalog/prices/:id` | Update mutable price fields (financials are immutable) |
| POST | `/v1/admin/catalog/prices/:id/activate` | Activate price |
| POST | `/v1/admin/catalog/prices/:id/deactivate` | Deactivate price |
| POST | `/v1/admin/catalog/prices/:id/reconcile` | Reconcile drift between OpenRails and the price's providers (see below) |

> **Reconciliation is a price-level operation.** Products are OpenRails-only
> concepts; their Stripe-side mirror exists only because Stripe requires Prices
> to attach to a Product, and is managed implicitly by price-level operations.
> There is no `providers` field on a product, no `?verify=true` on product GETs,
> and no `POST /admin/catalog/products/:id/reconcile` route.

The admin catalog API is the canonical surface for mutating `billing.products`
and `billing.prices`. Host apps must not write to those tables directly or
reach into `catalog.ProductService` / `catalog.PriceService` — those become
implementation details once this surface ships.

**Status lifecycle (issue #210).** Products and prices carry a `status` enum
instead of a bare `is_active` boolean, so a not-yet-launched item is distinct
from a retired-but-grandfathered one:

| status | in public catalog / purchasable? | existing subscriptions | Stripe |
|---|---|---|---|
| `draft` | no | none yet | not created in Stripe |
| `active` | yes | bill normally | `active: true` |
| `archived` | no | **grandfathered — bill forever** | `active: false` |

- `status` is optional on create (`POST /admin/catalog/products` and `.../prices`),
  defaulting to `active`. A historical plan with existing subscribers can be
  created `archived` in one step — no purchasable gap.
- `activate` sets `status=active`; `deactivate` sets `status=archived`. Set
  `status` explicitly (including `draft`) via `PATCH` to transition between states.
- **Grandfather guarantee:** archiving a price hides it from the public catalog
  and rejects *new* purchases, but **existing subscriptions on an archived price
  keep renewing and billing the stored amount indefinitely.** The renewal/rebill
  path loads the price by ID with no status filter — only the public catalog and
  new-purchase paths gate on `status=active`. Processor lookups used by renewal
  webhooks resolve any non-`draft` price for the same reason.

**Declarative provider attachment (issue #208).** On `POST /admin/catalog/prices`,
admins declare *intent* — which providers a price should exist in — and the
system picks the right mechanism per provider:

```json
{
  "product_id": "...",
  "display_name": "Premium Monthly",
  "unit_amount": 999,
  "currency": "usd",
  "billing_cycle_days": 30,
  "providers": ["stripe", "ccbill", "mobius"],
  "provider_links": {
    "ccbill": {"form_name": "premium", "flex_id": "abc-123"}
  }
}
```

> The Stripe `lookup_key` is **not** a request field. OpenRails assigns it
> internally as `openrails_<price_uuid>` (deterministic, strongly-consistent,
> reconstructable) so callers never construct one.

Behavior matrix:

| Provider | Pre-supplied link in `provider_links`? | No pre-supplied link |
|----------|----------------------------------------|----------------------|
| `stripe` | link + (optionally) verify the IDs exist | find-or-create (auto, idempotent) |
| `ccbill` | link                                   | mark `pending_manual_link`, surface PendingAction, **do not fail** (CCBill is link-only forever — no upstream API to create FlexForms) |
| `mobius` | link                                   | find-or-create the NMI Recurring Plan (auto, idempotent) when an NMI processor is configured; otherwise mark `pending_manual_link` and surface a PendingAction |

Response carries a typed per-provider state plus an aggregated pending-action list:

```json
{
  "id": "...",
  "providers": {
    "stripe": {"status": "linked", "ids": {"price_id": "price_xxx", "product_id": "prod_xxx"}, "lookup_key": "...", "sync_status": "unknown"},
    "ccbill": {"status": "linked", "ids": {"form_name": "premium", "flex_id": "abc-123"}},
    "mobius": {"status": "pending_manual_link", "message": "Create plan in NMI control center, then PATCH ..."}
  },
  "pending_manual_actions": [
    {
      "provider": "mobius",
      "action": "create_recurring_plan",
      "hint": "Create plan in the NMI control center, then PATCH /admin/catalog/prices/{id} with provider_links.mobius.plan_id",
      "patch_required": {"provider_links": {"mobius": {"plan_id": "<plan id>"}}}
    }
  ]
}
```

Provider status values: `linked`, `pending_manual_link`, `sync_disabled`, `error`.
Sync status (per provider, populated only by `?verify=true` reads + reconcile):
`unknown`, `in_sync`, `drifted`, `missing`, `never_synced`, `sync_disabled`.

To resolve a `pending_manual_link`, PATCH the price with the missing provider link:

```json
PATCH /admin/catalog/prices/{id}
{"provider_links": {"mobius": {"plan_id": "premium_monthly"}}}
```

No separate "resolve" endpoint is needed; the partial-PATCH merge on
`provider_links` is the contract.

**Stripe reconciliation model.** OpenRails is authoritative; Stripe is a
downstream projection.

- Create + update calls propagate to Stripe automatically. Pass
  `skip_processor_sync: true` to skip the Stripe round-trip for one operation
  (DB-only edit).
- Stripe objects created by OpenRails carry metadata
  (`openrails_product_id`, `openrails_price_id`) and Prices additionally carry
  a deterministic `lookup_key`. Find-or-attach uses these on Create: if a
  matching Stripe object exists (operator pre-created, or OpenRails recovered
  from a lost-ID state), the existing object is attached rather than
  duplicated.
- `GET /admin/catalog/prices/:id?verify=true` performs a live retrieve against
  every attached provider and populates `providers.<name>.sync_status` ∈
  `{unknown, in_sync, drifted, missing, never_synced, sync_disabled}` plus a
  per-field `drift[]` array. Without `verify`, reads are pure DB and
  `sync_status` is `"unknown"`. CCBill has no read API and always reports
  `sync_disabled`. NMI/Mobius surfaces live drift (plan_name, plan_amount) when
  an NMI processor is configured, and `sync_disabled` when it is not. Product
  GETs do **not** support `?verify=true` — products carry no provider state.
- `POST /admin/catalog/prices/:id/reconcile` walks every attached provider,
  does retrieve + diff + re-apply OpenRails values where the provider supports
  a write surface (Stripe today). Supports `?dry_run=true` (return the diff
  without mutating) and `?recreate=true` (for Stripe prices: when the stored
  Stripe Price ID 404s, mint a new one under the same lookup_key + metadata and
  update the OpenRails row). The response carries a per-provider `actions` map
  and the post-reconcile `providers` map. There is no product-level reconcile
  endpoint: updating a price (or reconciling it with `?recreate=true`)
  implicitly manages the Stripe Product mirror.
- **A background reconciliation loop surfaces drift, but never fixes it
  automatically (issue #209).** In addition to lazy `?verify=true` detection, a
  periodic job pulls the full Stripe catalog **and** the NMI recurring-plan
  catalog and records divergence (see "Catalog reconciliation loop" below). It is
  strictly alert-only: it writes to `billing.catalog_drift_events` and never
  mutates Stripe, NMI, or the catalog rows. Correction stays explicit (per-price
  reconcile), avoiding the silent-mutation scar tissue that hits
  two-source-of-truth systems. Document for your team: editing catalog directly
  in the Stripe Dashboard or the NMI control center will surface as drift and be
  reverted only on an explicit reconcile.

##### Catalog reconciliation loop (issue #209)

A background River job (`billing.catalog_reconciliation_pull`) periodically runs
two passes in a single scheduled run and diffs each against OpenRails:

- **Stripe pass:** enumerates the entire Stripe catalog (Products + Prices via
  the List API) and matches OpenRails rows by their `openrails_*_id` metadata.
- **NMI pass:** enumerates all NMI Recurring Plans via the Query API
  (`recurring_plans`) and matches OpenRails prices by their stored
  `provider_links.mobius.plan_id`.

Each pass is **independently skipped** when that processor is unconfigured (no
Stripe secret key / no `mobius` NMI client), so a Stripe-only or NMI-only
deployment works without configuration.

It records divergence as **open** rows in `billing.catalog_drift_events`. Every
row carries a `provider` column (`stripe` | `nmi`) that disambiguates the shared
`field_drift` kind:

| `kind`              | `provider` | Meaning |
|---------------------|-----------|---------|
| `orphan_in_stripe`  | `stripe`  | A Stripe object with no matching OpenRails row (missing/dangling `openrails_*_id` metadata marker). |
| `missing_in_stripe` | `stripe`  | An OpenRails row stores a Stripe object ID that was absent from the pulled Stripe catalog (deleted/archived in Stripe). |
| `orphan_in_nmi`     | `nmi`     | An NMI plan on the account with no matching OpenRails price. Plans whose `plan_id` carries the deterministic `openrails-<price_uuid>` prefix (the NMI analog of Stripe's metadata marker) are tagged with the extracted OpenRails price id so operators can tell an orphaned-but-ours plan from an operator-hand-made one. |
| `missing_in_nmi`    | `nmi`     | An OpenRails price references a `plan_id` that no longer exists on the NMI account (deleted upstream). |
| `field_drift`       | `stripe`  | An OpenRails row and its Stripe mirror disagree on a mutable field (`name`/`description`/`active` for products; `unit_amount`/`currency`/`active`/`nickname` for prices). |
| `field_drift`       | `nmi`     | An OpenRails price and its NMI plan disagree on `plan_name` or `plan_amount`. NMI's plan model is flat (no separate product, frequency immutable), so name + amount is the entire drift surface. |

**CCBill is intentionally NOT reconciled.** CCBill has no catalog-list API —
FlexForms are write-only redirect URLs, webhooks are inbound, and DataLink only
exports members/subscriptions (not catalog forms) — so enumeration is
structurally impossible. CCBill catalog links stay manual-only forever and never
appear in any drift surface.

The loop is **alert-only** — it never mutates Stripe, NMI, or the catalog rows.
It is idempotent: rerunning produces no new rows for unchanged divergence (dedupe
on `(provider, kind, resource_type, openrails_resource_id, external_resource_id,
field)`), and it auto-resolves (sets `resolved_at`) any open event whose
divergence has disappeared. Each detected event is also emitted as a structured
`WARN` log with `event=catalog_drift` (carrying `provider`) and stable fields for
downstream alerting; open counts per (provider, kind) are available via the
in-process `Service.CountOpenDriftByKind` for a
`openrails_catalog_drift_open_count{provider,kind}` metric.

**Schedule / disabling.** Default interval is **1h**. Override with the
`OPENRAILS_CATALOG_RECONCILIATION_INTERVAL` env var (a Go duration, e.g. `30m`,
`2h`). Set it to `0` (or `0s`) to **disable** the loop entirely.

**Admin surface:**

| Method | Path | Description |
|--------|------|-------------|
| GET | `/v1/admin/catalog/drift` | List open drift events (filters: `provider`, `kind`, `resource_type`, `limit`, `offset`). |
| GET | `/v1/admin/catalog/orphans` | List open orphan events across providers; `?provider=stripe\|nmi` scopes to one. |
| GET | `/v1/admin/catalog/stripe/orphans` | Convenience alias for `/orphans?provider=stripe`. |
| POST | `/v1/admin/catalog/drift/refresh` | Run the pull-and-diff pass (both providers) synchronously and return the resulting open drift set. |
| POST | `/v1/admin/catalog/drift/reconcile-all` | Alias for `drift/refresh` (spec-named on-demand trigger). |

**Operator runbook — "there's drift in production, what do I do?"**

1. `GET /v1/admin/catalog/drift` (or `POST /v1/admin/catalog/drift/refresh` first
   for an immediate fresh scan). Inspect the `provider`, `kind`, and `field` of
   each event. Filter by `?provider=nmi` to triage one processor at a time.
2. For `field_drift` / `missing_in_stripe` / `missing_in_nmi` on a price: resolve
   it with the existing `POST /v1/admin/catalog/prices/:id/reconcile` (add
   `?recreate=true` when the Stripe Price was deleted). OpenRails is
   authoritative, so reconcile re-applies OpenRails values to the provider (it
   re-applies `display_name` to NMI via `edit_plan`; NMI plan amount/frequency
   are immutable). Resolving a price this way **auto-closes** its matching open
   drift events.
3. For `orphan_in_stripe` / `orphan_in_nmi`: decide whether the upstream-only
   object should be imported (attach it via `PATCH /admin/catalog/prices/:id`
   `provider_links`) or deleted upstream (Stripe Dashboard / NMI control center).
   The loop will not touch it for you. An `orphan_in_nmi` row tagged with an
   OpenRails price id (the `openrails-<uuid>` plan_id prefix) is one OpenRails
   created but whose price row was since removed.
4. Re-run `GET /v1/admin/catalog/drift` to confirm the event has cleared.

**NMI / Mobius create-mode (issue #207).** NMI is a first-class create-capable
provider alongside Stripe. Declaring `providers: ["mobius"]` on CreatePrice
auto-creates the matching NMI Recurring Plan via the Direct Post API.

- **Deterministic plan_id.** OpenRails creates the plan under
  `openrails-<openrails_price_uuid>`. NMI plan_ids are operator-chosen and
  stable, so the price UUID is the plan identity. Rerunning CreatePrice (or
  recovering from a lost-ID state) does a find-or-attach against this id rather
  than duplicating the plan.
- **Field mapping.** `display_name` → `plan_name`; `unit_amount` (cents) →
  `plan_amount` (NMI takes dollars, so OpenRails divides by 100);
  `billing_cycle_days` → `day_frequency`. NMI plans bill forever
  (`plan_payments = 0`).
- **`billing_cycle_days` is required.** NMI recurring plans must have a
  frequency, so create-mode rejects a price with a nil `billing_cycle_days`.
- **Immutable fields.** Like Stripe Price financials, an NMI plan's frequency
  and payment count are immutable post-create. Only `plan_name` and
  `plan_amount` can change via `edit_plan`; OpenRails propagates `display_name`
  on UpdatePrice and rejects financial changes (amount/frequency are immutable
  in OpenRails too).
- **Deactivation divergence from Stripe.** On DeactivatePrice OpenRails does
  **not** call NMI `delete_recurring_plan`. NMI plan deletion does not stop
  subscriptions already billing on the plan, so the plan must outlive the
  OpenRails price. (Stripe's `Price active:false` is more lenient and is applied
  there.) The `DeleteRecurringPlan` client method exists for explicit operator
  cleanup paths only.
- **Verify + reconcile.** `?verify=true` does a live `recurring_plans` lookup by
  plan_id and reports `plan_name` / `plan_amount` drift; reconcile re-applies
  the OpenRails `display_name` via `edit_plan`.
- **No NMI processor configured?** Create-mode falls back to
  `pending_manual_link` so an operator can create the plan in the NMI control
  center and PATCH `provider_links.mobius.plan_id`.

Embedded callers can invoke the same methods via the Go facade
(`pkg/service.Service`):

```go
svc, _ := service.New(runtime)
out, err := svc.CreateProduct(ctx, service.CreateProductRequest{...})
out, err := svc.CreatePrice(ctx, service.CreatePriceRequest{
    ProductID:        product.ID,
    DisplayName:      "Premium Monthly",
    UnitAmount:       999,
    Currency:         "usd",
    BillingCycleDays: ptrInt(30),
    Providers:        []string{"stripe", "ccbill"},
    ProviderLinks: map[string]map[string]string{
        "ccbill": {"form_name": "premium", "flex_id": "abc-123"},
    },
})
// out.Providers["stripe"].Status == "linked"   (auto-created)
// out.Providers["ccbill"].Status == "linked"   (linked from supplied IDs)
// out.PendingManualActions is empty (both providers resolved)
out, err = svc.UpdatePrice(ctx, priceID, service.UpdatePriceRequest{
    DisplayName: ptr("New display name"),
    SkipProcessorSync: false, // default — propagate to every attached provider
})
```

---

Networking
- Public: port `2053` is published to the host.
- Service mTLS: port `2054` is exposed only to the Docker network when enabled. The listener requires client certificates signed by the configured HashiCorp Vault PKI CA.

Local mTLS certificates
- The Docker Compose `mtls` profile starts a local HashiCorp Vault dev server, enables the Vault PKI engine, and renders an OpenRails server cert plus service client certs into the shared `openrails_mtls` volume.
- Render local certs with `docker compose -f docker-compose.yaml --profile mtls run --rm vault-mtls-render`.
- Start OpenRails with `SERVICE_MTLS_ENABLED=true` after the certs are rendered. The default service listener is `https://openrails:2054`; sibling compose stacks can use `https://billing:2054` against the same server cert.
- Per-caller certs are rendered under `/run/secrets/mtls/clients/<identity>/`, including `authkit.internal`, `orchestrator.internal`, `doujins.internal`, and `hentai0.internal`.
- Server and client leaf certificate files are reloaded on new TLS handshakes so 7-day Vault renewals can roll forward without reintroducing API keys.
- `SERVICE_MTLS_ALLOWED_CLIENT_SANS` is a local shorthand for full service access. For production, prefer `service_mtls.clients.<identity>.scopes`.
- Deployment notes: [docs/mtls-vault-pki.md](docs/mtls-vault-pki.md).

Private “definition” surface (host-owned catalog + credits)
- OpenRails does **not** seed products/prices/credit types in production. Hosts should define them via:
  - Service mTLS API (port `2054`)
    - Credit types: `GET /v1/credit-types`, `POST /v1/credit-types`, `PATCH /v1/credit-types/{name}`, `POST /v1/credit-types/{name}/activate|deactivate`
    - Catalog: `POST /v1/catalog/products`, `PATCH /v1/catalog/products/{id}`, `POST /v1/catalog/prices`, `PATCH /v1/catalog/prices/{id}`
    - Credits funding: `POST /v1/credits/deposit`
  - Embedded Go API (in-process, no HTTP)
    - Credit types: `Service.ListCreditTypes`, `Service.CreateCreditType`, `Service.UpdateCreditType`, `Service.ActivateCreditType`, `Service.DeactivateCreditType`
    - Catalog: `Service.CreateProduct`, `Service.UpdateProduct`, `Service.CreatePrice`, `Service.UpdatePrice`
    - Credits funding: `Service.DepositCredits`
- Full request/response docs live in `docs/api/endpoints.md`.

JWT verification (Verifier Only)
	- Billing acts as a **JWT verifier**, not an issuer. It verifies tokens issued by your IdP(s).
	- The middleware validates signature and claims, extracting `sub` (user ID), `email`, optional `preferred_username`/`username`/`name`, and `roles` if present.
	- Configuration requirements:
	  - `AUTH_ISSUERS`: JSON array of token issuer URLs (e.g., `["http://localhost:8080"]` for local dev, or `["https://issuer.example.com"]` for production)
	  - `AUTH_EXPECTED_AUDIENCE`: The expected audience claim in JWTs (typically `billing-app`)
	  - Public keys are automatically fetched from each `{issuer}/.well-known/jwks.json` per OIDC spec
- Signature verification:
  - RS256 only (RSA signatures)
  - Public keys fetched via JWKS discovery from each configured issuer (supports automatic key rotation)
  - Keys are cached for 15 minutes and refreshed automatically
- Required JWT claims:
  - `iss` must equal one of the configured issuers
  - `aud` must contain the configured expected audience (`billing-app`)
  - `exp` must be valid (not expired, with 60-second clock skew tolerance)
  - `sub` must be present (user ID)

- Postgres
  - Bootstrap SQL lives under `migrations/bootstrap/` and runs at DB init via `/docker-entrypoint-initdb.d/`.
- ClickHouse
  - Data volumes: `clickhouse_data`, `clickhouse_logs`.
  - Migrations live in `migrations/clickhouse/` and include tables for: `subscription_events`, `payment_events`, `webhook_events`, `acu_events`, `chargeback_events`.
  - To reapply, remove the data volume and re-run `billing-migrate` (ClickHouse migrations are applied by the billing migrator).
- Garnet
  - Data volume: `garnet_data` (optional for persistence). Used for caching/rate limiting.

Common operations
- Start fresh (wipe local analytics/cache data):
  1) `task docker-down`
  2) `docker volume rm <project>_clickhouse_data <project>_clickhouse_logs <project>_garnet_data`
  3) `task docker-up`
  4) (Optional) if you also need a fresh Postgres, reset it from the host backend repository.
- Check health: `curl http://localhost:2053/health`
- Tail logs: `task docker-logs` or `docker-compose logs -f billing`

Troubleshooting
- Billing can’t connect to Postgres/Redis/ClickHouse:
  - Ensure services are healthy: `docker-compose ps` and `docker-compose logs <service>`.
  - Verify defaults weren’t overridden incorrectly (env/config). Remove overrides to return to zero‑config.
- Postgres bootstrap didn’t run:
  - Ensure the Postgres volume is fresh (entrypoint init scripts only run on first init). If needed, remove the `postgres_data` volume and restart compose.
- ClickHouse tables missing:
  - Check `clickhouse-bootstrap` logs and then `billing-migrate` logs. Ensure `migrations/clickhouse/*.sql` exist and the database is `analytics`.

Container usage
- Runtime configs (`config.yaml`, `config.docker.yaml`, etc.) are not baked into the image. Mount the desired file and point the CLI at it, e.g. `docker run -v $(pwd)/config.docker.yaml:/app/config.docker.yaml:ro doujins/openrails:latest -c /app/config.docker.yaml server`.
- The image entrypoint is the billing CLI. To launch workers only, override the command: `docker run ... doujins/openrails:latest worker`.

Notes
- This repository manages only the billing service operations. Application-specific integration (e.g., role management in your app DB) is out of scope here.
 - Premium checks in the host app should come from `billing.entitlements` (not from subscription rows). Email addresses should come from `profiles.users` (not denormalized into billing records).
