# Merchant invoice administration

The console's **Invoices** page and the customer **Invoice profile** section use the existing invoice, collection, and ledger services. Invoice issuance remains part of the billing-cycle workflow.

## Reads and permissions

Every invoice route is in the admin group: reads need `Permissions.AdminRead`,
actions `Permissions.AdminUpdate`.

- `GET /v1/admin/invoices`: a staff read. Filters: `customer_id`, `currency`, `status`, `period_starts_after`, and `period_starts_before`. Period filters select `period_starts_at` in the half-open range `[period_starts_after, period_starts_before)`. Results are a cursor page `{data, next_cursor}` (query `limit`, `cursor`), newest first.
- `GET /v1/admin/invoices/{id}`: the issued facts, customer UUID, monetary/collection state, and permitted `available_actions`. The customer's cards for a retry are read from `GET /v1/admin/customers/{customer_id}/payment-methods`.
- An invoice's payments are `GET /v1/admin/payments?invoice_id=`; its collection attempts, declines included, are `GET /v1/admin/payment-attempts?invoice_id=`. A repayment from the customer's balance moves no money: it is a ledger transfer, not a payment.
- A customer's invoice profile is its `invoice_profile` customer setting, read and written with `GET` / `PATCH /v1/admin/customers/{customer_id}` ([customer settings](api/merchant-settings.md#customer-settings)). Null means none: net 0, charged automatically. Profiles contain payment terms, collection method, PO, tax facts, contacts, and memo. Existing issued invoices retain their original snapshots. Tax facts do not calculate tax.

`available_actions` lists only the actions whose routes' permission admits the caller. On the standalone server viewers read invoices; support and owners also act on them.

## Existing support operations

| Endpoint suffix | Client method | Behavior |
|---|---|---|
| `POST /invoices/{id}/void` | `VoidInvoice` | Voids draft/open/past-due invoices and writes off the remaining debt through the existing ledger operation. Repeating an already completed void returns its current state. |
| `POST /invoices/{id}/mark-uncollectible` | `MarkInvoiceUncollectible` | Stops scheduled collection of open/past-due invoices; the debt remains owed. Repeating the same completed transition returns its current state. |
| `POST /invoices/{id}/retry-collection` | `RetryInvoiceCollection` | Starts one durable `invoice_collection` operation bound to an explicit customer-owned `payment_method_id` and `Idempotency-Key` (1–255 bytes). Reusing the key returns that operation's durable state (200 settled/failed, 202 still unresolved) without another provider charge; the same key with a different method is a 409 conflict. |

Money received outside OpenRails is recorded with `POST /v1/admin/payments` (`CreatePayment`): `{invoice_id, amount, transaction_id, paid_at}`, a positive `amount` up to `amount_due` and a `transaction_id` of 1–255 bytes. It charges no provider. The same `transaction_id` with the same terms answers the first payment; with other terms, or on another invoice, it is 422 `idempotency_key_reused`, never a second settlement. More than is due is 409 `payment_exceeds_due`.

Successful local actions return 200; an unresolved collection answers 202. Invalid state, a live collection operation and a new retry key while an operation is unresolved return 409. Invalid input returns 400; foreign or missing invoice/customer IDs return 404; a caller its permission refuses gets 403.

A never-attempted open invoice is not manually retryable. Retry eligibility applies to past-due/uncollectible automatic invoices and open automatic invoices with a prior failure. While `collection_intent_id` names a live operation the invoice accepts no support mutation; an operation the verifier cannot settle is resolved with `openrails intents resolve` (exact provider receipt or provider-confirmed non-execution, see [provider uncertainty](provider-uncertainty.md)). There is no unpark/force-resend operation.

## Period statements

Each period's statement totals its charges. When threshold invoices already bill some of them, `total_amount` includes those charges, `amount_due` is only what the statement bills itself, and `amount_paid` adds what those invoices received for the period: each invoice's payments apply to its oldest charges first. The statement stays `open` until nothing is due on it and those charges are paid, and becomes `paid` with the payment that completes them. One waiting only on other invoices offers only `void`.

## Amount units

Invoice and ledger amounts use the currency registry's native units; the scale of each currency is in `GET /v1/config`'s `currencies`. USD/EUR use six decimal places; JPY uses four. These are not assumed to be catalog/payment micros. Remittance input uses the invoice's same native units. Existing collection converts the unpaid native amount to the provider's minor unit at its established boundary.

The JPY acceptance test proves: 120000 native units = 12 JPY; a 20000-native manual payment leaves 100000; collection dispatches 10 whole-yen units to a fake charger and records 120000 total native units paid. This verifies internal arithmetic and the charger boundary, not live provider certification.

## Invoice notifications

Issuing a positive receivable queues `invoice_issued` in the same transaction as the invoice. An invoice still owed past its due date queues `invoice_overdue` once, at the next invoice pass. Repeating either operation does not repeat its notification. Collection and delinquency discover work from invoices and account policy; hosts own onboarding and any further notification policy.

Invoice collection charges only the customer's default card for that currency (`PUT /v1/me/default-payment-methods/{currency}`); there is no fallback instrument. Funding the customer's balance repays owed money first: it pays open, past-due and uncollectible invoices without a collection in flight, oldest first, and they appear in the payment history without a rail.
