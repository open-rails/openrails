# Billing Service API Reference

The billing service exposes a small set of HTTP APIs split across public catalog routes, authenticated
user routes, administrative endpoints, and a private service-to-service surface. All responses are
JSON encoded and use the standard error envelope:

```json
{
  "error": "machine_readable_code",
  "message": "human readable description"
}
```

## Authentication & Security

| Surface | How to authenticate |
|---------|---------------------|
| Public catalog (`/`, `/v1/products`, `/v1/prices`, `/v1/solana/tokens`, health probes) | No auth required |
| User routes (`/v1/solana/pay`, `/v1/me/*`) | `Authorization: Bearer <JWT>` issued by AuthKit |
| Admin routes (`/v1/admin/*`) | Same JWT header, user must have the `admin` role |
| Service API (private port 8060, `/v1/users/:user_id/entitlements`) | `X-API-KEY` header matching `config.api_key` |
| Webhooks (`/v1/webhooks/:provider`) | Provider-specific verification (CCBill IP allow-list, NMI HMAC, Solana reference check, Stripe signature) |

Unless stated otherwise, list endpoints use the Stripe-like envelope defined in `pkg/api.ListResponse`:
`{ object: "list", data: [...], total, limit, offset, has_more }`.

## Health & Service Banner

### GET /
Returns a short JSON banner (`{"service":"billing","status":"ok","endpoints":[...]}`) so load
balancers can confirm the API is reachable.

### GET /health/live, /healthz
Unconditional liveness probes.

### GET /health/ready, /readyz
Runs readiness checks against Postgres, Redis, and the AuthKit verifier. Returns 200 when all checks
pass, or 503 with `{ status: "not_ready", checks: {...} }`.

## Public Catalog Endpoints

### GET /v1/products
Lists products with embedded active prices. Query parameters mirror Stripe's `/v1/products`:
`active` (default `true`, only honoured for admins), `limit` (1-100, default 20), `offset` (>=0).
Response: `ListResponse<Product>`.

### GET /v1/prices
Lists price objects. Query parameters: `active`, `currency`, `product` (product ID), `type`
(`recurring` or `one_time`), plus `limit`/`offset`. Response: `ListResponse<Price>`.

### GET /v1/prices?product=prod_xxx
Same endpoint; explicitly documenting that product filters accept either the `prod_` prefixed ID or a
raw UUID.

### GET /v1/solana/tokens
Returns the currently enabled Solana tokens from configuration so clients can present token choices
(`[{ symbol, name, mint, decimals, enabled }]`).

### POST /v1/webhooks/{provider}
Receives processor webhooks. `provider` is `ccbill`, `mobius`, `solana`, or `stripe`.
- `ccbill`: form-encoded payload, verified via source IP ranges and optional signature.
- `mobius`/`nmi`: JSON body with `X-Signature`/`X-NMI-Signature` header.
- `solana`: JSON envelope emitted by the Solana Pay poller when on-chain confirmation occurs.
- `stripe`: JSON body with `Stripe-Signature` header (if configured).
Returns 200 on success, 401/403 for auth failures, 400 if the provider path is unknown.

## Solana Pay (Authenticated)

### POST /v1/solana/pay
Creates a Solana Pay transfer request for a recurring price.
- Auth: bearer token
- Body: `{ "price_id": "price_...", "token": "USDC" }`
- Response: `{ url, reference, amount, currency, token_amount, token, expires_at }`

### GET /v1/solana/pay/{reference}
Polls the status of a Solana Pay request created above. Returns `{ status: "pending|confirmed|expired", payment_id?, signature? }`.

## Authenticated User API (`/v1/me`)

Every endpoint in this section requires a valid JWT for the current user.

### GET /v1/me/status
Aggregated premium status: active subscription (if any), next renewal date, and active entitlements.
Response mirrors `handlers.GetMyBillingStatus` and includes `is_premium`, `subscription`, and
`entitlements` arrays.

### POST /v1/me/checkout
Unified checkout entry point for subscriptions and one-time purchases.
- Body (fields optional unless noted):
  - `price_id` (required) – target price UUID or prefixed ID
  - `processor` (required) – `mobius`, `ccbill`, or `solana`
  - `payment_method_id` or `payment_token` for Mobius/NMI purchases
  - Billing details: `email`, `first_name`, `last_name`, `address1`, `city`, `state`, `zip`, `country`
- Response: `{ status: success|redirect_required|blocked|pending, message, subscription_id?, payment_id?, transaction_id?, redirect_url?, delayed_start? }`

### GET /v1/me/subscriptions
History of the caller’s subscriptions. Query params: `status` (`active`, `cancelled`, `past_due`,
`all`), `limit`, `offset`. Response: paginated array of subscription summaries (`{ id, status,
processor, current_period_starts_at, current_period_ends_at, canceled_at? }`).

### PUT /v1/me/subscriptions/payment-method
Request body `{ "subscription_id": "sub_uuid", "payment_method_id": "pm_uuid" }`. Updates the card
vault entry used for an NMI-backed subscription (CCBill subscriptions cannot be reassigned). Returns
`{ message: "payment method updated" }`.

### POST /v1/me/subscriptions/cancel
Body `{ "feedback": "optional text" }`. Cancels the user’s active subscription or returns
`{ error, support_url }` when the subscription is managed externally (e.g., CCBill requires portal
cancellation).

### GET /v1/me/payments
Lists one-off payments. Query params: `type` (processor filter), `limit`, `offset`. Response:
`ListResponse<Payment>`.

### GET /v1/me/payment-methods
Query params: `limit`, `offset`, `include_inactive`. Response: list of stored payment methods with
fields `{ id, processor, last_four, brand, exp_month, exp_year, is_active, created_at }`.

### GET /v1/me/credits
Returns all active credit balances for the current user (promo + purchased). Response is an array of
`{ type, display_name, unit, decimal_places, balance, held_balance, permanent_balance, expiring_balance, earliest_expiry? }`.

### GET /v1/me/credits/{type}
Returns the credit balance for a single credit type (e.g. `api_credits`).

### GET /v1/me/credits/{type}/transactions
Lists credit ledger entries for the credit type. Query params: `limit`, `offset`.

## Service API (Private Port 8060)

All endpoints require `X-API-KEY` and run on the private port.

### GET /v1/users/{user_id}/entitlements
Returns active entitlements for the user at the current time.

### GET /v1/users/{user_id}/credits
Returns credit balance summary for a user. Optional query param `type` (defaults to `api_credits`).

### POST /v1/credits/withdraw
Withdraw credits. Body: `{ user_id, credit_type, amount, source, source_id? }`.

### POST /v1/credits/hold
Reserve credits for long-running jobs. Body: `{ user_id, credit_type, amount, source, source_id, expires_at }` (epoch seconds).

### POST /v1/credits/hold/{id}/capture
Capture a hold: `{ amount }` (amount <= hold).

### POST /v1/credits/hold/{id}/release
Release a hold without spending credits.

### POST /v1/me/payment-methods
Body includes `payment_token` (Collect.js token) plus billing details (`first_name`, `last_name`,
`address1`, `city`, `state`, `zip`, `country`, optional `email`, `phone`, `company`, `address2`,
`provider`). Creates and immediately activates an NMI vault record. Response: payment method object.

### PUT /v1/me/payment-methods/{id}
Body accepts a new `payment_token` and optional billing fields (all pointers). Replaces the stored
vault card for the referenced method. Returns updated payment method.

### DELETE /v1/me/payment-methods/{id}
Soft-deletes the stored method. Response `{ success: true }`.

### PUT /v1/me/payment-methods/{id}/activate
Re-verifies and marks the method active. Response `{ success: true }`.

### GET /v1/me/notifications
Query params: `page` (>=1), `page_size` (1-100, default 20), `seen` (`true`/`false`). Response list of
notifications `{ id, event_type, data, seen, created_at }`.

### GET /v1/me/notifications/unread-count
Returns `{ unread_count: <int> }`.

### POST /v1/me/notifications/{id}/read
Marks the notification as read. Empty body, response `{ message: "notification marked as read" }`.

## Admin API (`/v1/admin`, JWT + `admin` role)

### GET /v1/admin/subscriptions
List subscriptions with extensive filtering (limit/offset/status query params). Response:
`ListResponse<SubscriptionObject>`.

### GET /v1/admin/subscriptions/{id}
Detailed subscription record including lifecycle metadata and linked user information.

### PUT /v1/admin/subscriptions/{id}/extend
Body `{ "duration": "72h30m" }` (Go duration string). Extends the current billing period.

### POST /v1/admin/subscriptions/{id}/cancel
Immediate cancellation of the referenced subscription.

### GET /v1/admin/payments
List of payments with filtering by processor, status, date range, etc. Response: list envelope of
`Payment` objects.

### GET /v1/admin/payments/{id}
Full payment detail including refund history.

### POST /v1/admin/payments/{id}/refund
Body `{ "amount": 1234 }` (optional – defaults to full refund). Initiates a refund via the underlying
processor.

### GET /v1/admin/users/{user_id}
Returns the user’s billing profile (current subscription, payment methods, entitlements).

### GET /v1/admin/users/{user_id}/entitlements
Lists all entitlements (subscription-derived or manually granted).

### POST /v1/admin/users/{user_id}/entitlements
Body `{ "entitlement": "premium", "start_at": "2024-01-01T00:00:00Z", "end_at": null }`. Grants a manual
entitlement with audit trail support.

### DELETE /v1/admin/users/{user_id}/entitlements/{id}
Revokes the referenced admin entitlement.

### POST /v1/admin/users/{user_id}/grants
Creates a structured grant record (product-based, includes reason/audit info). Body mirrors the
`AdminGrant` struct.

### GET /v1/admin/users/{user_id}/grants
Lists grants issued to the user.

### GET /v1/admin/grants/{id}
Fetches a single grant record.

### GET /v1/admin/metrics
Returns dashboard/daily/processor metrics depending on `type` query param (defaults to
`dashboard`). For `type=daily`, supply `start`/`end` (YYYY-MM-DD). Response contains counts,
revenue, and processor breakdowns used by the admin UI.

## Private Service-to-Service API (port 8060)

### GET /v1/users/{user_id}/entitlements
Header `X-API-KEY` must match `config.api_key`. Returns active entitlements so other services can
check premium status: `[{ entitlement, start_at, end_at, source_type, source_id }]`.

## Webhook Notes

- **CCBill**: Must originate from the published IP ranges; the handler also validates `formName`/
  `flexId` against the price metadata.
- **Mobius/NMI**: Supply `X-Signature`/`X-NMI-Signature`. When test mode is enabled via config the
  signature check is bypassed.
- **Solana**: Called internally by the Solana Pay poller when an on-chain reference confirms.

Refer to `internal/services/webhook` for processor-specific payload schemas.
