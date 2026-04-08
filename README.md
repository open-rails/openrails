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
- Postgres: `postgres:17-bookworm` (DB `billing_db`, user `admin` / `admin_password`)
- Garnet (Redis-compatible): `ghcr.io/microsoft/garnet` on `6379`
- ClickHouse: `clickhouse/clickhouse-server` (DB `analytics`, user `analytics_user`, pass `analytics_password`)
- Billing service: this server exposing public API on `:2053` and a private/internal port `:8060` (exposed to the compose network only).

#### Quick Start
- Start services: `task docker-up` (or `docker compose -f docker-compose.yaml up -d`)
- Follow logs: `task docker-logs` (Ctrl+C to stop following)
- Stop services: `task docker-down`

- Postgres bootstrap: `migrations/bootstrap/001_postgres_init.sql` is mounted into `/docker-entrypoint-initdb.d` and runs automatically on first initialization.
- ClickHouse bootstrap: `clickhouse-bootstrap` waits for ClickHouse to be healthy and creates the `analytics` database + user/permissions.
- Billing migrations: `billing-migrate` applies Postgres + ClickHouse migrations.
- Note: ClickHouse migrations are tracked/locked via Postgres (`public.migrations` + advisory locks).
- Billing service connects using built-in defaults that match the compose network/service names.

- Postgres: `postgres://admin:admin_password@postgres:5432/billing_db?sslmode=disable`
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
- **Port 8060** (private): Internal service-to-service APIs (credits, entitlements)

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
| HTTP routing | Fixed routes on ports 2053/8060 | You choose which routes to mount |
| Health endpoints | Built-in `/health/*`, `/healthz`, `/readyz` | Host app provides its own |
| Internal ops | HTTP calls to port 8060 | Direct Go API calls |
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

> **Note:** Products cannot be deleted. Set `active: false` to hide from listings. Only `display_name`, `description`, and `is_active` can be updated.

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

> **Note:** Prices are mostly immutable. Each price belongs to exactly one product. Financial fields (`amount`, `currency`, `billing_cycle_days`) cannot be changed after creation to preserve historical payment accuracy. To change pricing, create a new price and deactivate the old one. Only `display_name`, `processors`, and `is_active` can be updated.

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

Admin endpoints are under `/v1/admin/*` and require:

- a valid JWT
- the user to have the `admin` role in AuthKit (`profiles.user_roles`)

Key endpoints:

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

---

### Legacy Endpoints (Deprecated)

These endpoints are kept for backwards compatibility but will be removed:

| Old Endpoint | New Endpoint |
|--------------|--------------|
| `GET /v1/subscriptions/products` | `GET /v1/products` |
| `GET /v1/subscriptions/active` | `GET /v1/me/subscriptions?status=active` |
| `GET /v1/subscriptions/history` | `GET /v1/me/subscriptions?status=all` |
| `GET /v1/subscriptions/purchases` | `GET /v1/me/payments` |
| `POST /v1/subscriptions/cancel` | `POST /v1/me/subscriptions/cancel` |
| `GET /v1/payment-methods` | `GET /v1/me/payment-methods` |
| `POST /v1/payment-methods` | `POST /v1/me/payment-methods` |
| `PUT /v1/payment-methods/:id` | `PUT /v1/me/payment-methods/:id` |
| `DELETE /v1/payment-methods/:id` | `DELETE /v1/me/payment-methods/:id` |
| `PUT /v1/payment-methods/:id/activate` | `PUT /v1/me/payment-methods/:id/activate` |
| `GET /v1/notifications` | `GET /v1/me/notifications` |
| `GET /v1/notifications/unread-count` | `GET /v1/me/notifications/unread-count` |
| `POST /v1/notifications/:id/read` | `POST /v1/me/notifications/:id/read` |
| `POST /v1/solana/generate` | `POST /v1/payment-intents` |
| `POST /v1/solana/qr` | `POST /v1/payment-intents/qr` |
| `GET /v1/solana/check?reference=...` | `GET /v1/payment-intents/:id` |
| `POST /v1/solana/submit` | `POST /v1/payment-intents/:id/confirm` |

---

Networking
- Public: port `2053` is published to the host.
- Private: port `8060` is exposed to the Docker network for intra-service communication (same host, no TLS needed).

Service API access
- Shared secret: private service API routes (port `8060`) are protected by header `X-API-KEY: <token>`.
- Default (dev): `change-me-in-dev`.
- Override via env `BILLING_API_KEY` or config `api_key`.

Private “definition” surface (host-owned catalog + credits)
- OpenRails does **not** seed products/prices/credit types in production. Hosts should define them via:
  - Private service API (port `8060`, `X-API-KEY`)
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
- Runtime configs (`config.yaml`, `config.docker.yaml`, etc.) are not baked into the image. Mount the desired file and point the CLI at it, e.g. `docker run -v $(pwd)/config.docker.yaml:/app/config.docker.yaml:ro openrails/billing:latest -c /app/config.docker.yaml server`.
- The image entrypoint is the billing CLI. To launch workers only, override the command: `docker run ... openrails/billing:latest worker`.

Notes
- This repository manages only the billing service operations. Application-specific integration (e.g., role management in your app DB) is out of scope here.
 - Premium checks in the host app should come from `billing.entitlements` (not from subscription rows). Email addresses should come from `profiles.users` (not denormalized into billing records).
