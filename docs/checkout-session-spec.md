# Checkout Session Spec

This document defines the unified checkout flow for all processors (NMI-backed processors such as Mobius, CCBill, Solana, Stripe).
It replaces processor-specific endpoints with a single checkout session model.

## Goals
- Single entrypoint for all payment processors.
- One processor per checkout.
- Consistent session lifecycle and responses.
- No storage of raw payment tokens.
- Extensible without changing top-level request/response shapes.

## Core Object: Checkout Session

Checkout Session represents a payment flow (not just a charge attempt). It can:
- require user action (redirect, QR, signing),
- be asynchronous,
- expire,
- resolve into a finalized `payment` record.

### Status Lifecycle
`created -> requires_action -> succeeded|failed|expired`

Sessions are single-use. Once terminal, they cannot be reused.

## Endpoints

- `POST /v1/checkout` create a checkout session.
- `GET /v1/checkout/{id}` fetch session status.
- `POST /v1/checkout/{id}/confirm` finalize user-completed flows (e.g., Solana signature).

All endpoints require authentication.

## Create Request

```json
{
  "price_id": "price_...",
  "mode": "one_off|subscription",
  "payment": {
    "processor": "mobius|ccbill|solana|stripe",

    "payment_method_id": "pm_...",
    "payment_token": "tok_...",

    "token_symbol": "USDC|SOL",
    "flow": "transfer_request|transaction_request",
    "wallet": "payer_pubkey",

    "email": "x@y.com",
    "first_name": "A",
    "last_name": "B",
    "address1": "...",
    "city": "...",
    "state": "...",
    "zip": "...",
    "country": "US"
  },
  "metadata": { "source": "web" }
}
```

### Request Rules
- `payment.processor` is required and defines all valid fields.
- `mode` is optional. If omitted, it is inferred from the price. If provided and invalid, return 400.
- NMI-backed processors/Stripe: exactly one of `payment_method_id` or `payment_token`.
- `payment_token` is never stored; it is consumed once to create a vault/payment method.
- If `payment_method_id` is used, it must belong to the authenticated user.
- Solana: `token_symbol` is required; `flow` defaults to `transfer_request`.
- Solana is always treated as `one_off`. If `mode=subscription` is provided with Solana, return 400.
- CCBill/Stripe: billing fields are required.
- NMI-backed processors/Solana: billing fields are ignored.

## Create Response

```json
{
  "object": "checkout_session",
  "id": "cs_...",
  "status": "requires_action|succeeded|failed|expired",
  "mode": "one_off|subscription",
  "price_id": "price_...",
  "payment": {
    "processor": "solana|mobius|ccbill|stripe",
    "reference": "...",
    "transaction_url": "solana:...",
    "transaction_data": "base64...",
    "redirect_url": "https://...",
    "transaction_id": "..."
  },
  "payment_id": "pay_...",
  "subscription_id": "sub_...",
  "expires_at": "2025-01-01T00:00:00Z",
  "next_action": {
    "type": "redirect_to_url|solana_qr|solana_transaction|none",
    "redirect_to_url": { "url": "..." }
  },
  "message": "string",
  "metadata": { "source": "web" }
}
```

### Response Notes
- `payment.*` includes only fields relevant to the processor.
- `payment_id` appears when payment is finalized.
- `subscription_id` appears for subscription flows (non-Solana).
- `next_action` describes the next user action, if any.

## Confirm Request

```json
{
  "payment": {
    "processor": "solana",
    "signature": "base58_signature",
    "wallet": "payer_pubkey"
  }
}
```

### Confirm Notes
- Only required for processors that need client-provided completion signals (Solana).
- Idempotent: if the session is already succeeded, return the current session.

## Processor Flows

### Solana (One-Off Only)
1) Create session with `payment.processor = solana`, `token_symbol`, optional `flow`.
2) Response:
   - `flow=transfer_request` -> `payment.transaction_url`, `payment.reference`, `next_action=solana_qr`.
   - `flow=transaction_request` -> `payment.transaction_data`, `next_action=solana_transaction`.
3) Client signs/broadcasts, then `POST /v1/checkout/{id}/confirm` with signature.
4) System verifies amount, recipient, token mint, and reference before finalizing.
5) On success: create `payment` record and grant entitlements.

Solana ignores subscription semantics. If the price has `billing_cycle_days`,
that duration defines the entitlement window for the one-off purchase.

### NMI-backed Card Payments
1) Create session with `payment_method_id` or `payment_token`.
2) If `payment_token` is provided, create a vault and persist `payment_method_id`.
3) Charge immediately and finalize.
4) Response includes `payment.transaction_id` and `status=succeeded`.

### CCBill
1) Create session with billing fields.
2) Response includes `payment.redirect_url` and `next_action=redirect_to_url`.
3) Webhook finalizes payment and updates the session to `succeeded`.

### Stripe
1) Create session with billing fields + `payment_method_id` or `payment_token` as needed.
2) Response may include `redirect_url` for hosted checkout.
3) Webhook finalizes payment and updates the session.

## Idempotency
- `POST /v1/checkout` honors `Idempotency-Key`.
- Repeated calls with the same key return the same session.
- `confirm` is idempotent and safe to retry.

## Data Model

### checkout_sessions (proposed)
- `id` (public id: `cs_...`)
- `user_id`
- `price_id`
- `mode` (`one_off|subscription`)
- `processor` (`solana|mobius|ccbill|stripe`)
- `status`
- `amount`, `currency`
- `expires_at`
- `reference` (Solana)
- `transaction_id` (processor transaction identifier)
- `payment_id`
- `metadata` (JSON, client-provided metadata)
- `processor_fields` (JSON, sanitized request inputs)
- `processor_state` (JSON, generated outputs)
- `created_at`, `updated_at`

### payments (existing)
Remains the ledger of finalized transactions. Sessions never replace payments.

## Security & Validation
- Do not persist raw `payment_token`.
- Validate payment method ownership.
- Solana confirmations must verify:
  - expected recipient,
  - expected token mint,
  - expected amount,
  - reference inclusion.

## Error Handling
- Use structured error responses.
- Invalid processor or missing required fields: 400.
- Unauthorized access or ownership mismatch: 403.
- Expired session: 410.
