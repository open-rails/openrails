# Merchant invoice administration

The console's **Invoices** page and the customer **Invoice profile** section use the existing invoice, collection, and ledger services. Invoice issuance remains part of the billing-cycle workflow.

## Reads and permissions

- `GET /v1/merchant/invoices`: requires `merchant:invoices:read`. Filters: `customer_id`, `currency`, `status`, `period_from`, and `period_to`. Period filters select invoice period starts in the half-open range `[period_from, period_to)`. `limit` is 1–100; `offset` is non-negative. Results use `{items,total,limit,offset}` with deterministic period/id ordering.
- `GET /v1/merchant/invoices/{id}`: the issued facts, customer UUID, monetary/collection state, and permitted `available_actions`. Collectors also receive minimal saved-method choices for that customer.
- `GET /v1/merchant/invoices/{id}/payments`: paginated payment/collection history, with the same read permission.
- `GET` / `PUT /v1/merchant/customers/{customer_id}/invoice-profile`: existing customer-settings read/update permissions. Profiles contain payment terms, collection method, PO, tax facts, contacts, and memo. Existing issued invoices retain their original snapshots. Tax facts do not calculate tax.

The fixed viewer role reads invoices. Support also retries collection. Invoice updates (voiding, marking uncollectible, and recording external money) require the separate update permission, held by the owner wildcard by default.

## Existing support operations

| Endpoint suffix | Permission | Behavior |
|---|---|---|
| `POST /invoices/{id}/void` | `merchant:invoices:update` | Voids draft/open/past-due invoices and writes off the remaining debt through the existing ledger operation. Repeating an already completed void returns its current state. |
| `POST /invoices/{id}/uncollectible` | `merchant:invoices:update` | Stops scheduled collection of open/past-due invoices; the debt remains owed. Repeating the same completed transition returns its current state. |
| `POST /invoices/{id}/payments` | `merchant:invoices:update` | Records an external remittance with positive `amount` and a non-empty `reference` (up to 255 bytes). It does not charge a provider. Reusing an applied reference is a 409 conflict, never a second settlement. |
| `POST /invoices/{id}/retry-collection` | `merchant:invoices:collect` | Starts one durable `invoice_collection` operation bound to an explicit customer-owned `payment_method_id` and `Idempotency-Key` (1–255 bytes). Reusing the key returns that operation's durable state (200 settled/failed, 202 still unresolved) without another provider charge; the same key with a different method is a 409 conflict. |

Successful local actions return 200; an unresolved collection answers 202 with its live attempt. Invalid state, conflicting remittance/reference, a live collection operation and a new retry key while an operation is unresolved return 409. Invalid input returns 400; foreign or missing invoice/customer IDs return 404; insufficient permissions return 403. The existing HTTP idempotency cache may return the original successful response verbatim on replay.

A never-attempted open invoice is not manually retryable. Retry eligibility applies to past-due/uncollectible automatic invoices and open automatic invoices with a prior failure. While `collection_intent_id` names a live operation the invoice accepts no support mutation; an operation the verifier cannot settle is resolved with `openrails intents resolve` (exact provider receipt or provider-confirmed non-execution, see [provider uncertainty](provider-uncertainty.md)). There is no unpark/force-resend operation.

## Amount units

Invoice and ledger amounts use the currency registry's native units, exposed as `unit_decimals` on merchant invoices and payment-history entries. USD/EUR use six decimal places; JPY uses four. These are not assumed to be catalog/payment micros. Remittance input uses the invoice's same native units. Existing collection converts the unpaid native amount to the provider's minor unit at its established boundary.

The JPY acceptance test proves: 120000 native units = 12 JPY; a 20000-native manual payment leaves 100000; collection dispatches 10 whole-yen units to a fake charger and records 120000 total native units paid. This verifies internal arithmetic and the charger boundary, not live provider certification.

## Invoice notifications

Issuing a positive receivable queues `invoice_issued` in the same transaction as the invoice. The first overdue transition queues `invoice_overdue` atomically. Repeating either operation does not repeat its notification. These ordinary payer notices require no business profile, onboarding, KYC or terms-acceptance record. Collection and delinquency discover work from invoices and account policy.

The enterprise onboarding routes, business profiles, repeated business reminder ladder, budget-alert thresholds and suspension-recommendation product are unavailable. Hosts own onboarding and any extra notification policy. Core invoice collection, negotiated rates, invoice profiles and delinquency remain available independently.

Invoice collection uses the explicit `collection_payment_method_id` for the payer and currency. There is no fallback to an automatic top-up instrument. Automatic balance refill, its safety policy/panel and self-service auto-top-up settings routes are unavailable. Fiat deposits, manual funding, invoice collection and ordinary recurring subscription payments remain available.
