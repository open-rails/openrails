# Usage orders

Usage billed in arrears is an order with `origin: usage`. Automatic
[usage billing](usage-billing.md) invoices eligible pending charges when a
customer reaches its credit limit or fixed monthly billing date, per currency.
Amounts below the minimum invoice carry forward, including across periods.
Each order bills the window's pending owed charges once: one line per accrual
source (`source`, e.g. `metered:<meter>`), with no price or product. Usage is
priced and owed when it is recorded ([usage pricing](usage-pricing.md)), so
an order bills what was recorded in its window, including usage dated in an
earlier period. It is due
when billed plus the customer's net terms, and has no expiry. A window with
nothing owed bills no order.

`usage` carries the period (`period_starts_at`, `period_ends_at`), `due_at`,
`amount_paid` and `amount_due`, the collection method and the customer's
invoice profile as it was when billed (`po_number`, `memo`, `tax`,
`billing_contacts`). A funded balance and remittances pay part of it; the
order is numbered and `complete` once fully paid. `delinquent` marks an
overdue order whose customer's grace has run out (see
[arrears and delinquency](arrears-delinquency.md)).

Amounts are the currency registry's native units (`GET /v1/config`'s
`currencies`): USD and EUR six decimals, JPY four. Collection converts what is
due to the provider's minor unit at its boundary.

Provider-managed subscription bills also use orders; see [provider-managed orders](provider-managed-orders.md) for collection ownership and settlement history.

## Reads

Usage orders are orders: `GET /v1/admin/orders?origin=usage` (`ListOrders`),
also filtered by `customer_id`, `status` and `overdue=true` (open, still owed
past `due_at`), and `GET /v1/admin/orders/{id}` (`GetOrder`). An order's
payments are `GET /v1/admin/payments?order_id=`; its collection attempts,
declines included, `GET /v1/admin/payment-attempts?order_id=`. A repayment
from the customer's balance moves no money: it is a ledger transfer, not a
payment. The customer reads their own with `GET /v1/me/orders?origin=usage`.

A customer's invoice profile is their `invoice_profile` setting, read and
written with `GET` / `PATCH /v1/admin/customers/{customer_id}`
([customer settings](api/merchant-settings.md#customer-settings)): net terms,
collection method, PO, tax facts, contacts and memo. Null means net 0, charged
automatically. Orders already billed keep their snapshot. Tax facts calculate
no tax.

## Collection

`charge_automatically` orders are charged to the customer's default card for
their currency (`PUT /v1/me/default-payment-methods/{currency}`) once due;
there is no fallback card. Each charge is an attempt on the order, run as a
durable `order_collection` operation; the order is `processing` while it is
unresolved. A decline leaves it `open` with `payment.last_payment_error`, and
the collection cadence retries it. `send_invoice` orders are never charged:
the customer remits, or pays one with `POST /v1/me/orders/{id}/pay` and a
saved card, in person.

Funding the customer's balance repays owed money first: it pays open and
uncollectible usage orders without a collection in flight, oldest first.

Billing an order queues `order_due`; one still owed past its due date queues
`order_overdue` once, at the next pass; a decline that ends its collection
(the retry schedule exhausted, or a decline no retry fixes) queues
`order_collection_stopped`.

## Staff actions

Each is an admin change: `billing.MerchantCustomerManage` or `billing.RootCustomerManage`.

| Route | Client method | Behavior |
|---|---|---|
| `POST /v1/admin/orders/{id}/waive-balance` | `WaiveOrderBalance` | Voids an open or uncollectible usage order and writes off what it owes. Repeating it changes nothing. |
| `POST /v1/admin/orders/{id}/mark-uncollectible` | `MarkOrderUncollectible` | Stops collecting an open usage order; what it owes stays owed, and collecting it again reopens it. |

A collection in progress refuses every action
(`order_payment_in_progress`). An operation the verifier cannot settle is
resolved with `openrails intents resolve` (see
[provider uncertainty](provider-uncertainty.md)). Another origin, or a state
that does not allow the action, is 409 `order_action_not_allowed`.

Money received outside OpenRails is recorded with `POST /v1/admin/payments`
(`RecordExternalPayment`): `{order_id, amount, transaction_id, paid_at}`, a positive
`amount` up to `amount_due` and a `transaction_id` of 1–255 bytes. It charges
nothing. The same `transaction_id` with the same terms answers the first
payment; with other terms it is 422 `idempotency_key_reused`. More than is due
is 409 `payment_exceeds_due`.

Staff cannot initiate an immediate saved-card charge. The customer pays an outstanding order explicitly; automatic collection and dunning run under the customer's active agreement and configured retry policy. `WaiveOrderBalance` forgives the remaining debt; it does not refund earlier payments. `RecordExternalPayment` records money already received. Marking a balance uncollectible stops scheduled collection while retaining what is owed.

Staff subscription changes also perform no immediate saved-card charge:
engine-billed upgrades and seat increases take effect at renewal, while the
customer's own subscription-change flow retains immediate proration. A staff
retry of an older pending provider operation only reads its status.
