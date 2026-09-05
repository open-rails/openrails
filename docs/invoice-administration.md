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
| `POST /invoices/{id}/retry-collection` | `merchant:invoices:collect` | Uses existing durable collection retry with an explicit customer-owned `payment_method_id` and `Idempotency-Key` (1–255 bytes). Reusing an operation key reuses its recorded attempt; it does not charge again. |

Successful local actions return 200. Invalid state, conflicting remittance/reference, in-progress collection, and unknown collection outcome return 409. Invalid input returns 400; foreign or missing invoice/customer IDs return 404; insufficient permissions return 403. The existing HTTP idempotency cache may return the original successful response verbatim on replay.

A never-attempted open invoice is not manually retryable. Existing retry eligibility applies to past-due/uncollectible automatic invoices and open automatic invoices with a prior failure. Uncertain or in-progress collections are unavailable for support mutation until reconciliation resolves them. No HTTP unpark/force-resend operation is added.

## Amount units

Invoice and ledger amounts use the currency registry's native units, exposed as `unit_decimals` on merchant invoices and payment-history entries. USD/EUR use six decimal places; JPY uses four. These are not assumed to be catalog/payment micros. Remittance input uses the invoice's same native units. Existing collection converts the unpaid native amount to the provider's minor unit at its established boundary.

The JPY acceptance test proves: 120000 native units = 12 JPY; a 20000-native manual payment leaves 100000; collection dispatches 10 whole-yen units to a fake charger and records 120000 total native units paid. This verifies internal arithmetic and the charger boundary, not live provider certification.
