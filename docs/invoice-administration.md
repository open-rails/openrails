# Merchant invoice administration

The console's **Invoices** page and the customer **Invoice profile** section use the existing invoice, collection, and ledger services. Invoice issuance remains part of the billing-cycle workflow.

## Reads and guards

Every invoice route is a staff route in the `openrails.Invoices` resource
group: reads are `openrails.StaffReads`', actions `openrails.StaffWrites`', and
a host may guard one route more strictly (`openrails.VoidInvoice`).

- `GET /v1/merchant/invoices`: a staff read. Filters: `customer_id`, `currency`, `status`, `period_starts_after`, and `period_starts_before`. Period filters select `period_starts_at` in the half-open range `[period_starts_after, period_starts_before)`. Results are a cursor page `{data, next_cursor}` (query `limit`, `cursor`), newest first.
- `GET /v1/merchant/invoices/{id}`: the issued facts, customer UUID, monetary/collection state, and permitted `available_actions`. The customer's cards for a retry are read from `GET /v1/merchant/customers/{customer_id}/payment-methods`.
- `GET /v1/merchant/invoices/{id}/payments`: payment/collection history, a cursor page, a staff read.
- A customer's invoice profile is its `invoice_profile` customer setting, read and written with `GET` / `PATCH /v1/merchant/customers/settings` ([customer settings](api/merchant-settings.md#customer-settings)). Null means none: net 0, charged automatically. Profiles contain payment terms, collection method, PO, tax facts, contacts, and memo. Existing issued invoices retain their original snapshots. Tax facts do not calculate tax.

`available_actions` lists only the actions whose routes' guards admit the caller. On the standalone server viewers read invoices; support and owners also act on them.

## Existing support operations

| Endpoint suffix | Route guard | Behavior |
|---|---|---|
| `POST /invoices/{id}/void` | `openrails.VoidInvoice` | Voids draft/open/past-due invoices and writes off the remaining debt through the existing ledger operation. Repeating an already completed void returns its current state. |
| `POST /invoices/{id}/uncollectible` | `openrails.MarkInvoiceUncollectible` | Stops scheduled collection of open/past-due invoices; the debt remains owed. Repeating the same completed transition returns its current state. |
| `POST /invoices/{id}/payments` | `openrails.CreateInvoicePayment` | Records an external remittance with positive `amount` and a non-empty `reference` (up to 255 bytes). It does not charge a provider. Reusing an applied reference is a 409 conflict, never a second settlement. |
| `POST /invoices/{id}/retry-collection` | `openrails.RetryInvoiceCollection` | Starts one durable `invoice_collection` operation bound to an explicit customer-owned `payment_method_id` and `Idempotency-Key` (1–255 bytes). Reusing the key returns that operation's durable state (200 settled/failed, 202 still unresolved) without another provider charge; the same key with a different method is a 409 conflict. |

Successful local actions return 200; an unresolved collection answers 202 with its live attempt. Invalid state, conflicting remittance/reference, a live collection operation and a new retry key while an operation is unresolved return 409. Invalid input returns 400; foreign or missing invoice/customer IDs return 404; a caller its guard refuses gets 403.

A never-attempted open invoice is not manually retryable. Retry eligibility applies to past-due/uncollectible automatic invoices and open automatic invoices with a prior failure. While `collection_intent_id` names a live operation the invoice accepts no support mutation; an operation the verifier cannot settle is resolved with `openrails intents resolve` (exact provider receipt or provider-confirmed non-execution, see [provider uncertainty](provider-uncertainty.md)). There is no unpark/force-resend operation.

## Period statements

Each period's statement totals its charges. When threshold invoices already bill some of them, `total_amount` includes those charges, `amount_due` is only what the statement bills itself, and `amount_paid` adds what those invoices received for the period: each invoice's payments apply to its oldest charges first. The statement stays `open` until nothing is due on it and those charges are paid, and becomes `paid` with the payment that completes them. One waiting only on other invoices offers only `void`.

## Amount units

Invoice and ledger amounts use the currency registry's native units; the scale of each currency is in `GET /v1/currencies`. USD/EUR use six decimal places; JPY uses four. These are not assumed to be catalog/payment micros. Remittance input uses the invoice's same native units. Existing collection converts the unpaid native amount to the provider's minor unit at its established boundary.

The JPY acceptance test proves: 120000 native units = 12 JPY; a 20000-native manual payment leaves 100000; collection dispatches 10 whole-yen units to a fake charger and records 120000 total native units paid. This verifies internal arithmetic and the charger boundary, not live provider certification.

## Invoice notifications

Issuing a positive receivable queues `invoice_issued` in the same transaction as the invoice. The first overdue transition queues `invoice_overdue` atomically. Repeating either operation does not repeat its notification. Collection and delinquency discover work from invoices and account policy; hosts own onboarding and any further notification policy.

Invoice collection charges only the customer's explicit `collection_payment_method_id` for that currency; there is no fallback instrument. Funding the customer's balance repays owed money first: it pays open, past-due and uncollectible invoices without a collection in flight, oldest first, and they appear in the payment history without a rail.
