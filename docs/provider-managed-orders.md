# Provider-managed bills are orders

An order records what is owed. Its payments record money that actually moved.
That relationship is the same when OpenRails collects, when an NMI recurring
schedule collects, and when Stripe Billing or CCBill manages billing and retries.
[Usage orders](usage-billing.md#usage-orders) already use this model; provider-managed orders
add no second usage-pricing engine.

A provider renewal, an initial enrollment, and a separate adjustment are distinct
bills. A declined attempt followed by a successful retry is one bill. Multiple
partial payments can settle that bill. A provider webhook is evidence of an
observation, not an instruction for OpenRails to charge again.

## Reads and collection

Use the ordinary order and payment reads. The order's `collection_owner` is
`engine`, `nmi_schedule`, or `provider`; `collection_psp_id` identifies the account
when known. `provider_bills` contains bounded document references and verified
settlement facts, without provider secrets or raw payloads. Prices and products
belong to order lines. Payments and settlement events name their order instead
of carrying a separate price.

Provider-owned orders do not offer generic saved-card collection. Customer pay,
confirmation, staff remittance, retries and workers enforce that boundary on the
server as well as in the UI. NMI's existing OpenRails recovery coordinator can
still recover a confirmed decline or skipped collection; representing the bill
as an order does not create an additional collector.

The provider's confirmed amount, currency, service dates and line terms are
retained. Later catalog edits do not reprice them. A refund or dispute names the
original payment and order, rather than creating another purchase. Non-cash
settlement, such as provider credit or a verified out-of-band payment, does not
manufacture a gateway charge.

## Replay and changes

The provider account, bill identity and monetary transaction identity have
separate roles. Repeated or reordered webhooks and reconciliation reads reuse
the accepted bill and payment. Reuse of a financial identity with conflicting
amount, currency, customer or order is refused for reconciliation.

NMI and CCBill receipts are matched to explicit accepted schedule terms. Hourly
cadence and calendar-month cadence are distinct. Confirmed schedule amendments
receive an immutable acceptance version, so later receipts use the appropriate
agreement and an old receipt replay retains its original binding. A receipt
timestamp or the subscription's current paid-through date alone cannot prove a
new bill.

A genuine extra capture is retained as money and surfaced for review; it does
not increase the bill total or grant a second period. A late charge does not
revive a canceled subscription. Existing provider uncertainty and cancellation
holds remain in effect.

## Charges before the schedule

A recurring charge pays the cycle it starts. An NMI or CCBill charge made
before the subscription's first accepted schedule cycle, or any such charge
of a canceled subscription without one, is settled history: a complete order on
the subscription's price at the provider's amount, for one cadence from the
charge (provider bill kind `provider_pre_schedule`). It binds no cycle, so it
renews nothing, grants no access and infers no schedule; revenue metrics
count it when it was charged, and renewal cycles never include it. Import,
the NMI pull and verification record it the same way, once per provider
transaction.

## Import and archive restore

Use the [billing import guide](batch-import.md#accepted-bills-and-provider-schedules)
to prepare evidence for NMI and CCBill recurring bills and Stripe invoices.
Use [historical payments](batch-import.md#historical-payments) for a verified
NMI receipt whose original subscription or catalog purchase is unavailable.

Every payment and reversal requires its original order. Merchant archive import
checks that each order belongs to the payment's customer and that reversals
reference the original payment's order. Use
[merchant portability](merchant-portability.md) for a current OpenRails archive.
Use [billing import](batch-import.md#the-migration-playbook) to bring verified
legacy facts into the current schema.
[Payment constraint](../internal/migrate/postgres/0001_schema.up.sql) ·
[Archive validation](../internal/merchantarchive/provider_bill_checks.go)
