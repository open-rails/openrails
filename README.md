### Doujins Billing Service — Operations Manual

#### Scope
- Provides a billing-related API server for the frontend to use (signups, cancellations, etc.), and an admin-API server for the backend to use (admin cancellations).
- Handles webhooks from supported payment processors (mobius, ccbill, solana), and updates corresponding subscriptions / entitlements.
- Runs periodic jobs to update subscriptions / entitlements.

#### Interactions with other services (Intended Contract)
- Entitlements (app reads from): Billing owns the `billing.entitlements` table and writes premium access windows when memberships start/renew and revokes them on cancel/expiry. The main Doujins app can read this table to decide if a user is “premium” at a given point in time (current time ∈ [start_at, end_at) and `revoked_at IS NULL`).

- Profiles (billing reads from): When emailing users (e.g., subscription started/renewed/ended, payment failures, one‑off receipts), Billing reads the current email address from `profiles.users`. We treat user IDs as UUIDs; the service performs a direct, schema‑qualified lookup: `SELECT username, email, email_verified, is_active FROM profiles.users WHERE id = $1`.

---

#### Stack
- Postgres: shared `postgres:17-bookworm` container from the Doujins backend stack (DB `doujins_db`, user `admin` / `admin_password`)
- Garnet (Redis-compatible): `ghcr.io/microsoft/garnet` on `6379`
- ClickHouse: `clickhouse/clickhouse-server` (DB `analytics`, user `analytics_user`, pass `analytics_password`)
- Billing service: this server exposing public API on `:2053` and a private/internal port `:8060` (exposed to the compose network only). Admin routes require an internal shared secret.

#### Quick Start
- Ensure the shared Postgres container from `doujins-backend` is running and attached to the `local-doujins` network
- Start services: `task docker-up` (or `docker-compose up -d`)
- Follow logs: `task docker-logs` (Ctrl+C to stop following)
- Stop services: `task docker-down`

- Postgres bootstrap: `db-bootstrap` runs `migrations/bootstrap/*.sql` against the shared database to (re)create roles, schemas, and extensions.
- ClickHouse migrations: `clickhouse-init` job waits for ClickHouse to be healthy, then applies `migrations/clickhouse/*.sql` to the `analytics` database.
- Billing service connects using built-in defaults that match the compose network/service names.

- Postgres: `postgres://admin:admin_password@postgres:5432/doujins_db?sslmode=disable`
- Redis (Garnet): `garnet:6379`, DB `0`
- ClickHouse: `http://clickhouse:8123` with `analytics_user/analytics_password` on DB `analytics`

#### Overriding configuration (optional)
- Config file: place `config.yaml` in repo root or `./config/config.yaml`.
- Env vars: common overrides include `DB_URL`, `REDIS_ADDR`, `CLICKHOUSE_HTTP_ADDR`, `CLICKHOUSE_DATABASE`, `CLICKHOUSE_USERNAME`, `CLICKHOUSE_PASSWORD`, `AUTH_ISSUER`, `AUTH_AUDIENCE`.
- If not provided, the service uses the defaults above.

#### Embedding (optional)
You can embed the billing server inside another Go service:

```go
emb, err := embedded.New(embedded.Options{Config: cfg})
if err != nil {
  return err
}
defer emb.Close(ctx)

router.Handle("/billing/", emb.Handler())
router.Handle("/billing-internal/", emb.PrivateHandler())
```

If you want background workers in the same process:
```go
go emb.RunWorkers(ctx)
```

Developer tasks
- Build: `task build` → outputs `bin/billing`
- Run (binary): `task run` → builds then runs `billing server`
- Dev (no build): `task dev` → `go run ./ server`
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
- `processor` - Filter: `ccbill`, `mobius`, `solana`, `system`
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

#### Solana Wallets

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/v1/me/wallets` | List linked Solana wallets |
| GET | `/v1/me/wallets/linked` | Get primary linked wallet |
| POST | `/v1/me/wallets/challenge` | Generate wallet verification challenge |
| POST | `/v1/me/wallets/verify` | Verify wallet signature |
| DELETE | `/v1/me/wallets` | Unlink wallet |

**Challenge Request Body:**
```json
{ "wallet": "<base58_address>" }
```

**Verify Request Body:**
```json
{
  "wallet": "<base58_address>",
  "signature": "<base58_signature>",
  "message": "<challenge_message>"
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

#### Payment Intents (Solana Payments)

| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/v1/payment-intents` | Create payment intent (direct wallet) |
| POST | `/v1/payment-intents/qr` | Create payment intent (QR/Solana Pay) |
| GET | `/v1/payment-intents/:id` | Get payment intent status |
| POST | `/v1/payment-intents/:id/confirm` | Confirm with signed transaction |

**Create Payment Intent Body (direct):**
```json
{
  "price_id": "<uuid>",
  "token": "USDC",
  "wallet": "<base58_address>"
}
```

**Create Payment Intent Body (QR):**
```json
{
  "price_id": "<uuid>",
  "token": "USDC",
  "wallet": "<optional_base58_address>"
}
```

**Confirm Payment Intent Body:**
```json
{
  "signed_transaction": "<base64_encoded_transaction>"
}
```

**Payment Intent Object:**
```json
{
  "id": "pi_<uuid>",
  "object": "payment_intent",
  "status": "pending",
  "amount": 999,
  "currency": "usd",
  "payment_method": {
    "type": "solana",
    "token": "USDC",
    "token_mint": "<mint_address>",
    "token_amount": "9.99",
    "wallet": "<payer_address>"
  },
  "transaction": {
    "data": "<base64_tx>",
    "url": "solana:<recipient>?...",
    "reference": "<reference_key>",
    "signature": "<tx_signature>"
  },
  "expires_at": 1704070800,
  "created": 1704067200
}
```

---

### Subscription Creation

#### NMI / Mobius (Card Payments)

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
| POST | `/v1/webhooks/:provider` | Receive webhook from processor (ccbill, mobius, solana) |

---

### Admin Endpoints (Admin API - Port 8060)

Requires `X-API-KEY: <billing_api_key>` header.

| Method | Endpoint | Description |
|--------|----------|-------------|
| PUT | `/v1/subscriptions/:id/extend` | Extend subscription period |
| POST | `/v1/subscriptions/:id/cancel` | Admin cancel subscription |
| POST | `/v1/users/:user_id/subscription/cancel` | Cancel user's subscription |
| GET | `/v1/subscriptions/:id/details` | Get subscription details |
| GET | `/v1/subscriptions/dashboard-metrics` | Get dashboard metrics |
| GET | `/v1/subscriptions/daily-metrics` | Get daily metrics |
| GET | `/v1/subscriptions/processor-metrics` | Get per-processor metrics |
| GET | `/v1/users/:user_id/entitlements` | Get user's active entitlements |

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
| `GET /v1/wallet/solana` | `GET /v1/me/wallets` |
| `GET /v1/wallet/solana/linked` | `GET /v1/me/wallets/linked` |
| `POST /v1/wallet/solana/challenge` | `POST /v1/me/wallets/challenge` |
| `POST /v1/wallet/solana/verify` | `POST /v1/me/wallets/verify` |
| `DELETE /v1/wallet/solana` | `DELETE /v1/me/wallets` |
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

Admin access
- Shared secret: admin routes are protected by header `X-API-KEY: <token>`.
- Default (dev): `change-me-in-dev`.
- Override via env `BILLING_API_KEY` or config `billing_api_key`.

JWT verification (Verifier Only)
- Billing acts as a **JWT verifier**, not an issuer. It verifies tokens issued by doujins and/or hentai0.
- The middleware validates signature and claims, extracting `sub` (user ID), `email`, optional `preferred_username`/`username`/`name`, and `roles` if present.
- Configuration requirements:
  - `BILLING_AUTH_ISSUERS`: JSON array of token issuer URLs (e.g., `["http://localhost:2052", "http://localhost:4000"]` for local dev, or `["https://doujins.com", "https://hentai0.com"]` for production)
  - `AUTH_EXPECTED_AUDIENCE`: The expected audience claim in JWTs (must be `billing-app`)
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
  - Shared container: provided by `doujins-backend` compose (service name `postgres`).
  - Bootstrap SQL lives under `migrations/bootstrap/` and is applied by the `db-bootstrap` job; rerun by restarting that service.
- ClickHouse
  - Data volumes: `clickhouse_data`, `clickhouse_logs`.
  - Migrations live in `migrations/clickhouse/` and include tables for: `subscription_events`, `payment_events`, `webhook_events`, `acu_events`, `chargeback_events`.
  - To reapply, remove the data volume or re-run the `clickhouse-init` service after clearing state.
- Garnet
  - Data volume: `garnet_data` (optional for persistence). Used for caching/rate limiting.

Common operations
- Start fresh (wipe local analytics/cache data):
  1) `task docker-down`
  2) `docker volume rm <project>_clickhouse_data <project>_clickhouse_logs <project>_garnet_data`
  3) `task docker-up`
  4) (Optional) if you also need a fresh Postgres, reset it from the Doujins backend repository.
- Check health: `curl http://localhost:2053/health`
- Tail logs: `task docker-logs` or `docker-compose logs -f billing`

Troubleshooting
- Billing can’t connect to Postgres/Redis/ClickHouse:
  - Ensure services are healthy: `docker-compose ps` and `docker-compose logs <service>`.
  - Verify defaults weren’t overridden incorrectly (env/config). Remove overrides to return to zero‑config.
- Postgres bootstrap didn’t run:
  - Restart the `db-bootstrap` service. If the shared Postgres container was reset, ensure the `local-doujins` network still exists before bringing billing back up.
- ClickHouse tables missing:
  - Check `clickhouse-init` logs. Ensure `migrations/clickhouse/*.sql` exist and the database is `analytics`.

Container usage
- Runtime configs (`config.yaml`, `config.docker.yaml`, etc.) are not baked into the image. Mount the desired file and point the CLI at it, e.g. `docker run -v $(pwd)/config.docker.yaml:/app/config.docker.yaml:ro doujins/billing:latest -c /app/config.docker.yaml server`.
- The image entrypoint is the billing CLI. To launch workers only, override the command: `docker run ... doujins/billing:latest worker`.

Notes
- This repository manages only the billing service operations. Application-specific integration (e.g., role management in your app DB) is out of scope here.
 - Premium checks in the Doujins app should come from `billing.entitlements` (not from subscription rows). Email addresses should come from `profiles.users` (not denormalized into billing records).
