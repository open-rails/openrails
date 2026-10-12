# Provider-managed bills are orders

An order records what is owed. Its payments record money that actually moved.
That relationship is the same when OpenRails collects, when an NMI recurring
schedule collects, and when Stripe Billing or CCBill manages billing and retries.
[Usage orders](usage-orders.md) already use this model; provider-managed orders
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

## Import and prelaunch upgrade

[Billing import](batch-import.md) accepts explicit verified schedule and invoice
facts. A live NMI/CCBill import provides its accepted recurring amount, currency,
cadence and first due boundary. Stripe financial history provides the complete
invoice and actual settlement evidence. An amount-only transaction cannot
reconstruct an unknown historical bill from today's catalog.

The order-only payment migration removes `payments.price_id`. It refuses an
existing book containing payments without orders, rather than deleting records
or inventing historical obligations. For the prelaunch cut, use an empty billing
schema or an explicitly prepared order-based book. This is a coordinated
application/schema upgrade: update hosts and the browser SDK together. Restoring
an old archive with orderless payments is explicitly refused. Migration itself
performs no database reset and makes no provider call.
