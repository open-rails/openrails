# Usage billing

Usage is reported on a lease, priced in batches, and accrues to the customer's
day, which posts to the ledger as one deduction: the prepaid balance first, owed
for the rest. Limits cap what a customer may use per window; admissions and the
Go SDK's gate set capacity aside before work starts. With automatic usage
billing, owed usage becomes a usage order collected from the customer's default
card. Related: [prepaid credits](prepaid-credits.md),
[spend limits](spend-limits.md), [running charges](running-charges.md) (billing by
the hour).

## Recording usage

`POST /v1/app/leases/sync` (`SyncLeases`, `merchant:usage:manage`) is the only
usage intake. The [gate](#the-gate) sends it for you, with the `wants`, `returns`
and `committed` that ask for, give back and account for its holds; a batch job
calls it directly with `usage` alone.

- **Lease.** The client picks the `lease_id` (`lse_` and a UUID): a gate when it
  starts, a batch job once for its whole stream. The first sync under a new id
  creates the lease, so a lost first reply repeats harmlessly. A UUIDv7 id more
  than 60 days old that names no lease is `404 lease_not_found`: start a new one.
- **Items.** `usage` holds up to 1,000 items
  `{n, customer_id, limit_set, invoker, invoker_type, currency, source, source_id,
  outcome, event_type, dimensions, metadata, amount, occurred_at}`, numbered once
  per lease and sent in order of `n`. `limit_set` names the hold the item was
  decided against (null without one), `source_id` is your call id and `amount` a
  stated cost. `occurred_at` may be up to 35 days old and at most 5 minutes ahead.
- **Replay.** The lease keeps its highest settled number (`usage.through_n`). A
  resent item answers as settled; a changed item under a settled `n` is
  `422 idempotency_key_reused`; a gap refuses that item and the ones after it
  `409 usage_out_of_order`. Any credential of the merchant may report on its
  leases, live or not, so a restarted process resends from an outbox. An item
  refused alone (`usage.refused`: `{n, status, error}`) posts nothing; the others
  settle.

Usage is never refused for money: the work happened, and admission is the gate
before it. A priced meter that cannot price an item refuses it
`422 usage_not_priced` (a matrix with no cell for the item's value, with `param`
naming the field; rate cards only in other currencies). An item no priced meter
consumes settles and charges nothing.

`outcome: failed` is work that cost and did not deliver. It states its own cost and
is never rated. The customer's own failures are forgiven up to its policy's
`bad_spend_windows` and charged past them, at once and with their own ledger legs;
a delegated invoker's (`invoker_type: delegated`) are never charged and count
toward `delegated_invoker_wasted_spend_limits`, past which admission refuses it
`failure_rate_limited`.

**One deduction per day.** A sync settles its usage in one transaction: it prices
the items, counts them toward the customer's limits, and adds the charges to the
customer's accrual for that currency, UTC day and pricing period
(`usage_accruals`). An accrual posts once its day ends, in the hourly billing
pass, or earlier, before any other movement of that balance (a top-up or staff
grant, a credit lot's expiry, revocation or refund, a usage order): one `spend`
per credit lot it draws and one `owed_accrual` for the rest. Until it posts,
`balance` holds it, so admissions refuse exactly as if it had posted.

**Reading usage.** No usage event is stored. What settles is kept as aggregates
that grow with customers and days, not requests: limit windows,
`meter_period_quantities`, accruals, daily rollups (`usage_daily`, per customer,
invoker, currency, event type, meter, cell and outcome) and per-minute spend
(`usage_minutes`, kept a day). Keep your own log if you need each request.

- `GET /v1/me/usage`: the customer's usage in one `currency` over the UTC days
  overlapping [`from`, `to`) (default the month before now), grouped by
  `event_type` (default), `invoker` or `meter` (per meter and cell, with
  `quantity`).
- `ListBalanceTransactions` and `GET /v1/me/balance/transactions`: each leg of a
  usage deduction carries `usage`, `{day, amount, lines}` with `requests`,
  `quantity` and `amount` per `meter_key` and `cell`: "Nov 22: 1,204 requests,
  3.1M tokens of chat (large), $4.12".
- `POST /v1/metrics/query`: `usage_units`, `usage_revenue` and `forgiven_usage`
  by `customer`, `invoker`, `outcome` or `rate_card`; `active_customers` by
  `rate_card`; `limits_reached` by `limit` and `customer`.

## Pricing

A meter (`count`, or `sum` of its `value_property` from the item's `dimensions`,
then `metadata`) with a rate card in the item's currency takes a quantity from it;
a card's `filter` can exclude the item. A sync prices together the items that
share a customer, currency, pricing period, meter cell and rate card: with Q the
customer's quantity so far in the period and q the group's, the group is charged

    R(Q + q) − R(Q)

and the amount is shared to its items by quantity, exactly. R prices a whole
period's quantity and never falls, so no charge is negative, and a period's
charges sum exactly to R of its total however its usage is batched: 1,000 items
of 1,234 tokens at $0.15 per million accrue exactly 185,100 micros. Quantities
are kept per period in `meter_period_quantities`.

| Model | R(Q) |
|---|---|
| `per_unit` | `round(Q × unit_amount / divide_by)` (`round: up\|down\|half_up`, default `half_up`), capped by `maximum_amount` per period; a `matrix` prices each value of one dimension in its own cell |
| `tiered` | `graduated` only (volume tiers would make later usage refund earlier usage) |
| `package` | `ceil((Q − free_units) / package_size) × amount` |
| `dynamic` | the item's own `amount` (native units) when its `sum` quantity is positive; no allowance |
| `flat` | a fixed fee per period, on a rate card with no meter |

`allowance.included` comes off Q first, per period and cell. A metered card states
no `payment_term`. A `dynamic` card and catalog cards on the same item each
charge. Usage dated in an earlier period is priced at that period's quantity and
billed by the open period's order. Usage decided against a hold is priced by the
cards the hold pinned; other usage by the cards in force when it settles, and
items under two revisions of a card are priced apart.

With automatic usage billing the customer's monthly anchor defines the pricing
period; otherwise the merchant's `billing_period_boundary`, anchored on the
customer's first activity.

Per million tokens: one item per request with disjoint dimensions
(`input_tokens`, `cached_input_tokens`, `output_tokens`), one `sum` meter each,
`per_unit` with `divide_by: 1000000` and a `model` matrix:

```yaml
meters:
  input-tokens:
    event_type: chat
    aggregation: sum
    value_property: input_tokens
    group_by: {model: metadata.model}
products:
  chat:
    display_name: Chat
    rate_cards:
    - meter: input-tokens
      price:
        model: per_unit
        currency: USD
        per_unit:
          divide_by: 1000000
          matrix:
            dimension: model
            cells:
              small: {unit_amount: "150000"}
              large: {unit_amount: "2500000"}
```

Meters are declared in a catalog document or with `PUT /v1/catalog/meters/{key}`
(`SetMeter`, with its rate card).

## Limits

The catalog declares [limits](catalog.md#a-document) and products give them
capacity. A currency limit counts every request and item in its currency at its
money price; a named-unit limit counts, in its unit, the usage its rate cards'
meters read. A host-stated admission (`estimated_amount`, no usage) holds no
named-unit limit, and its capture counts there.

- **The rule.** Each customer, limit and window (`limit_windows`, windows by the
  server clock) keeps its settled units; held is the sum of its live holds. A hold
  is issued or grown only while settled + held + the increase fits the capacity.
  Settled alone may pass it: late usage and overruns settle in full, never
  clamped, and nothing more is held until a refill or reset. A request counting
  toward a limit none of the customer's products gives has capacity 0 and is
  refused.
- **Overrun.** `require_estimate` admits a request only when its whole estimate
  fits. `admit_while_positive` admits while any capacity remains and holds what is
  left, so the crossing request finishes and the limit stays below zero.
- **Refusals.** `limit_reached` (`blocked_by: limit`) names the `limit` and its
  `resets_at` (null when nothing refills it), with `retry_after_seconds` until
  then: the request would not fit even were every gate's hold returned. A
  request that would fit but for capacity gates hold, a gate's want or an
  admission alike, is refused `capacity_held` (`blocked_by: limit`) with
  `retry_after_seconds` of one sync interval, and those holds shrink at their
  next sync. The same rule holds for `balance` and spend windows: their own
  codes mean the request would not fit even then.
- **`balance`.** The prepaid balance and credit line are the built-in limit
  `balance`, per currency, checked first and refused with the money codes
  (`insufficient_balance`, `insufficient_credit`, `outstanding_cap_reached`). A
  billing policy's spend windows are currency limits too
  ([spend limits](spend-limits.md)).
- **Reads.** `ListCustomerLimits` (`GET /v1/app/limits?customer_id=`,
  `merchant:usage:read`) and `GET /v1/me/limits` answer
  `{customer_id, limit, unit, capacity, used, held, remaining, window_starts_at,
  resets_at}` for every declared limit by key, then a `balance` line per currency
  (capacity the balance plus credit line, used what is owed, held the held total;
  no window).
- **Staff.** `limits` on `UpdateCustomer` replaces a customer's capacity
  (`"limits": [{"limit": "weekly", "capacity": "8000"}]`; `inherit: true` restores
  its products'). `AdjustCustomerLimit`
  (`POST /v1/admin/customers/{customer_id}/limit-adjustments`,
  `{limit, amount, source_id}`, `merchant:customer:manage`) adds to the current
  window only; a retry with its `source_id` adds nothing again. A rate limit takes
  no adjustment.
- **Reached.** A window records `reached_at` the first time settled + held take it
  to capacity; `limits_reached` in `/v1/metrics/query` counts them.

## Admissions

`/v1/app/admissions` (`Admit`, `CaptureAdmissions`, `ReleaseAdmissions`,
`ExtendAdmissions`), up to 1,000 items with one result each. An admission is a
hold owned by one request. The durable `request_id` (1–255 bytes) owns replay; no
`Idempotency-Key`.

- **Admit** with `usage: {event_type, dimensions, metadata}` (an LLM request:
  `{input_tokens: <prompt>, output_tokens: <max_tokens>}`): the rate cards price it
  at the current period quantity without advancing it, and that is the money hold
  and `estimated_amount`. The cards are pinned: capture pays the admitted price. A
  host-stated `estimated_amount` is the alternative. The request is checked
  against `balance`, then its spend windows, then the declared limits it counts
  toward by key; the first that would not fit even were gates' holds returned
  refuses it, else one that fits only then makes it `capacity_held`, and
  nothing is held. An allowed request holds its estimate in every one of them.
- **Capture** (`{request_id, usage: {…, resource, source, source_id}}`, same event
  type) prices the consumed usage with the pinned cards and settles it in the
  hold's windows as the hold leaves, in one transaction. Above the hold the whole
  charge settles and the excess is owed, never clamped: bound it by the estimate.
  The receipt has `estimated_amount`, `amount`, `charges`,
  `balance_transaction_id` (the first leg of the day's posting, null until it
  posts) and `replayed`.
- **Release** frees the hold; **extend** moves an open, unexpired hold's deadline
  later (at most 30 days past admission).

A request binds immutable terms (customer, unit, estimate, deadline, invoker,
trust, roles, resource, source, prospective rate); a repeat with other terms
conflicts and an exact retry returns the original decision with `replayed: true`
and the current `state` (`open`, `expired`, `released`, `captured`). `allowed` on a
replay is not permission to start new work. A hold that sets anything aside needs
`expires_at`. Past it, the hold reserves no money and nothing in a lifetime or
rate limit, but still counts in windows that end until it is captured or
released, and a late capture counts in its original windows. Expiry needs no
worker. A hold is kept 61 days (`limit_holds`, monthly partitions); after that
it reads as never admitted and a late capture is `admission_not_found`.

OpenRails keeps holds and windows in PostgreSQL, under the customer's money
lock. One held total covers live estimates, pending credit refunds, running
charges' unposted accruals and unposted usage.

Embedded, `Engine.LocalMerchant(id)` returns a `*LocalClient` whose `AdmitTx`,
`CaptureAdmissionsTx` and `ReleaseAdmissionsTx` take a transaction from the host's
pool: the host's own row and its hold commit together.

`GetAdmission` / `GET /v1/app/admissions/{request_id}` and `ListAdmissions` (by
`customer_id`, `currency`, `state`, admission time) read request holds without
replaying them; a denied request made no reservation and is
`admission_not_found`. They need `merchant:usage:read`. A gate's holds have no
read route.

## The gate

`openrails.NewGate(client, openrails.GateConfig{})` admits in your process, over
a remote or embedded `*Client` whose credential holds `merchant:usage:manage`
and `merchant:usage:read`. It keeps one lease and holds of each served customer's
capacity, sized from traffic: a warm decision takes the request's cost from
every limit of its hold, or from none, with no I/O, so it holds while OpenRails
is unreachable. A cold customer, or a hold short of its demand, waits for one
`SyncLeases` round trip. The gate syncs on a fixed cadence while it lives,
carrying the usage its tickets captured, and the server prices that usage
exactly.

```go
gate, err := openrails.NewGate(client, openrails.GateConfig{})
ticket, err := gate.Admit(ctx, billing.AdmitParams{RequestID: id, CustomerID: customer,
    Invoker: user, Currency: "USD", Usage: &billing.UsageEstimate{EventType: "chat", Dimensions: estimate}})
if r := ticket.Refusal(); r != nil { /* 429: r.DenyCode, r.Limit, r.ResetsAt, r.RetryAfterSeconds */ }
// … do the work, then:
err = ticket.Capture(ctx, billing.CaptureUsage{Dimensions: used}) // or ticket.Release(ctx)
```

- `Admit` takes `AdmitParams` with a `request_id`; `expires_at` is not needed,
  since a ticket lives while the gate syncs. A request that declares `accrual_rate_delta_per_hour` is
  admitted by the server as a request hold. `Consume` admits a known cost and
  captures it at once; `Record` sends usage no admission held. `Limits` shows a
  customer's lines less the usage not yet sent, for display. `Flush` syncs now
  and waits for settlement; `Close` sends pending usage and returns the holds.
- A refusal is a verdict on the ticket, never an error; `capacity_held` is
  waited out within the request's context first. The error is bad input or
  `ErrUnavailable`: OpenRails unreachable and no hold covering the request.
- Safe spending is off unless the merchant sets `usage_gate_safe_spend: true`.
  Each hold then names a `safe` amount, its money size, that the gate may spend
  unreserved while OpenRails is unreachable, once the hold's own money is spent
  or its lease has died. It covers money only: units are never taken past a
  live hold, so a request that counts units waits for OpenRails. What safe
  spending admits settles as late usage, owed past the balance.
- Without Redis, state is in process memory: usage captured but not yet sent is
  lost if the process crashes, and never billed. `GateConfig.Outbox`
  (`UsageOutbox`, not with Redis) stores it before `Capture` returns; a restarted
  process resends it against its original leases.
- With `GateConfig.Redis`, your processes sharing that Redis are one gate: one
  lease and one hold per customer and limit set, decided by one Redis script, so a
  customer one process served is warm on all. Usage waits in a Redis stream and
  one process at a time syncs. A process that cannot reach Redis admits from its
  own lease meanwhile. Losing Redis loses its unsent usage, never billed, and the
  lease's unused capacity comes back when the lease expires.

Nothing in `GateConfig` tunes sizes, timing or process counts: holds follow
traffic, and a merchant declares only limits, products and prices. A gate that
stops syncing loses its lease within seconds and its unused capacity is released;
usage it reports later posts as late usage. [`examples/gate`](../examples/gate)
runs one.

## Automatic usage billing

Off unless the host sets `usage_billing_enabled: true`
(`Config.UsageBillingEnabled`); subscription-only hosts need none of it. Each
merchant declares defaults:

```yaml
usage_billing:
  billing_day: 13
  currencies:
    - {currency: USD, credit_limit: 100000000, minimum_invoice_amount: 5000000}  # $100, $5
```

Spending on credit needs a valid default card for the currency and its collection
agreement. The ledger limits debt, unpaid orders included, and a failed payment
does not reopen credit. When debt reaches the limit, or the monthly date arrives,
pending charges become one usage order and collection charges the default card.
An admission past the remaining credit asks for collection and is refused until
payment frees capacity. A legacy `collection_threshold` can lower the invoice
threshold, never raise the limit. Amounts below the minimum carry forward; a
threshold payment never moves the monthly date. Dates are UTC; days 29–31 clamp to
a shorter month.

Per customer (`PATCH /v1/admin/customers/{customer_id}`):

```json
{
  "credit_limits": [{"currency": "USD", "amount": "75000000"}],
  "usage_billing": [{"currency": "USD", "billing_anchor": "2026-01-13T00:00:00Z", "minimum_invoice_amount": "1000000"}]
}
```

Null `billing_anchor` or minimum restores the merchant default; `"0"` disables
credit and `{"currency": "USD", "inherit": true}` restores the default limit.
Changing the merchant's `billing_day` moves no existing anchor. Reads include
`next_bill_at`.

Each account stores when it is next due: its bill date, or at once when spending
crosses its threshold, an admission asks to collect, or its terms change. A
threshold crossing queues the pass in the same transaction; otherwise the hourly
billing pass finds due accounts, then due orders, and runs each charge as an
`order_collection` operation. Deploy to every worker before enabling it.

## Usage orders

A usage order (`origin: usage`) bills its window's owed charges once, one line per
source (`metered:<meter>`), no price or product; a window with nothing owed bills
nothing. Unposted usage posts before its order is billed. `usage` carries
`period_starts_at`, `period_ends_at`, `due_at` (billed
plus net terms; no expiry), `amount_paid`, `amount_due`, the collection method and
the customer's `invoice_profile` as billed (`po_number`, `memo`, `tax`,
`billing_contacts`; tax facts calculate no tax). Null profile means net 0, charged
automatically. The order is numbered and `complete` when paid; `delinquent` marks
one overdue past the customer's grace ([spend limits](spend-limits.md)).

- **Reads:** `GET /v1/admin/orders?origin=usage` (`customer_id`, `status`,
  `overdue=true`), its payments `GET /v1/admin/payments?order_id=`, its attempts
  `GET /v1/admin/payment-attempts?order_id=`; customers `GET /v1/me/orders?origin=usage`.
- **Collection:** `charge_automatically` orders are charged to the default card for
  the currency once due (no fallback card), each attempt an `order_collection`
  operation; the order is `processing` meanwhile, and a decline leaves it `open`
  for the retry schedule. `send_invoice` orders are never charged: the customer
  remits, or pays with `POST /v1/me/orders/{id}/pay`. Funding the balance repays
  owed orders first, oldest first (a ledger transfer, not a payment).
- **Events:** `order_due` when billed, `order_overdue` once when still owed past
  due, `order_collection_stopped` when retries end.
- **Staff** (`merchant:customer:manage`): `WaiveOrderBalance`
  (`POST /v1/admin/orders/{id}/waive-balance`) voids and writes off what it owes;
  `MarkOrderUncollectible` stops collection and keeps the debt (collecting again
  reopens it); `RecordExternalPayment` (`POST /v1/admin/payments`,
  `{order_id, amount, transaction_id, paid_at}`) records money received elsewhere,
  idempotent on `transaction_id`, at most `amount_due`
  (`409 payment_exceeds_due`). A collection in flight refuses them
  (`order_payment_in_progress`); another origin or state is
  `409 order_action_not_allowed`. Staff never start a card charge; an unsettled
  operation is resolved with `openrails intents resolve`.

Provider-run subscription bills are orders too:
[provider-managed orders](provider-managed-orders.md).
