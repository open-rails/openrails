# Usage billing

Usage is priced when it is recorded, draws the prepaid balance first and is owed
for the rest. Admissions hold the expected cost before work starts. With
automatic usage billing, owed usage becomes a usage order collected from the
customer's default card. Related: [prepaid credits](prepaid-credits.md),
[spend limits](spend-limits.md), [running charges](running-charges.md) (billing by
the hour).

## Recording usage

`POST /v1/app/usage-events` (`RecordUsage`, `merchant:usage:manage`), up to 1,000
items, one result each. An event is idempotent on `(source, source_id)` for 35
days, and `occurred_at` may be up to 35 days old, never in the future. It is priced
by its meters' rate cards in the transaction that records it, so the balance,
admissions, spend windows and threshold see the charge at once. The event returns
`charges` (one `{meter_key, model, cell, quantity, amount}` per meter) and their
sum, `amount`.

Usage is never refused for money: the work happened, and admission is the gate
before it. A charge draws the prepaid balance, the rest is owed, and what is owed
refuses the next admission. A priced meter that cannot price an event refuses it
`422 usage_not_priced` and records nothing (a matrix with no cell for the event's
value, with `param` naming the field; rate cards only in other currencies). An
event no priced meter consumes is recorded and charges nothing.

`outcome: failed` is work that cost and did not deliver. It states its own cost and
is never rated. The customer's own failures are forgiven up to its policy's
`bad_spend_windows` and charged past them (`amount` charged, `forgiven_amount`
absorbed); a delegated invoker's (`invoker_type: delegated`) are never charged and
count toward `delegated_invoker_wasted_spend_limits`, past which admission refuses
it `failure_rate_limited`.

**Inspecting.** `GetUsageEvent` / `GET /v1/app/usage-events/{id}` reads one event
(`uev_`); `ListUsageEvents` / `GET /v1/app/usage-events` lists them by
`customer_id`, `currency`, `event_type`, `source` (+ `source_id`) and `outcome`,
newest first, over at most 31 days of occurrence time (`from` inclusive, `to`
exclusive; default the latest 31 days). Both need `merchant:usage:read`, never
charge, and see only retained usage. Reports: `POST /v1/metrics/query`
(`usage_units`, `usage_revenue`, `forgiven_usage` by `customer`, `invoker`,
`outcome`, `sku` or `rate_card`).

## Pricing

A meter (`count`, or `sum` of its `value_property` from the event's `dimensions`,
then `metadata`) with a rate card in the event's currency takes a quantity from it;
a card's `filter` can exclude the event. With Q the customer's quantity so far in
the period and q the event's, the event is charged

    R(Q + q) − R(Q)

where R prices a whole period's quantity. R never falls, so no charge is negative,
and a period's charges sum exactly to R of its total: 1,000 events of 1,234 tokens
at $0.15 per million cost 185.1 micros each, rounded where they fall, and exactly
185,100 together. Quantities are kept per period in `meter_period_quantities`.

| Model | R(Q) |
|---|---|
| `per_unit` | `round(Q × unit_amount / divide_by)` (`round: up\|down\|half_up`, default `half_up`), capped by `maximum_amount` per period; a `matrix` prices each value of one dimension in its own cell |
| `tiered` | `graduated` only (volume tiers would make later events refund earlier ones) |
| `package` | `ceil((Q − free_units) / package_size) × amount` |
| `dynamic` | the event's own `amount` (native units) when its `sum` quantity is positive; no allowance |
| `flat` | a fixed fee per period, on a rate card with no meter |

`allowance.included` comes off Q first, per period and cell. A metered card states
no `payment_term`. A `dynamic` card and catalog cards on the same event each
charge; the stated amount is kept as `stated_amount`, and a retry with a different
amount (absent → `"0"` included) is `idempotency_key_reused`. An event dated in an
earlier period is priced at that period's quantity and billed by the open period's
order. A rate card change applies from the next event; a replay answers its first
charges.

With automatic usage billing the customer's monthly anchor defines the pricing
period; otherwise the merchant's `billing_period_boundary`, anchored on the
customer's first activity.

Per million tokens: one event per request with disjoint dimensions
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

## Admissions

`/v1/app/admissions` (`Admit`, `CaptureAdmissions`, `ReleaseAdmissions`,
`ExtendAdmissions`), up to 1,000 items with one result each. The durable
`request_id` (1–255 bytes) owns replay; no `Idempotency-Key`.

- **Admit** with `usage: {event_type, dimensions, metadata}` (an LLM request:
  `{input_tokens: <prompt>, output_tokens: <max_tokens>}`): the rate cards price it
  at the current period quantity without advancing it, and that is the hold and
  `estimated_amount`. The cards are pinned: capture pays the admitted price. A
  host-stated `estimated_amount` is the alternative; with neither nothing is held.
- **Capture** (`{request_id, usage: {…, resource, source, source_id}}`, same event
  type) prices the consumed usage with the pinned cards, records the event and
  posts its charges as the hold leaves, in one transaction. Above the hold the
  whole charge posts and the excess is owed, never clamped: bound it by the
  estimate. The receipt has `estimated_amount`, `amount`, `charges`,
  `usage_event_id`, `balance_transaction_id` and `replayed`.
- **Release** frees the hold; **extend** moves an open, unexpired hold's deadline
  later (at most 30 days past admission).

A request binds immutable terms (customer, unit, estimate, deadline, invoker,
trust, roles, resource, source, prospective rate); a repeat with other terms
conflicts and an exact retry returns the original decision with `replayed: true`
and the current `state` (`open`, `expired`, `released`, `captured`). `allowed` on a
replay is not permission to start new work. Expiry follows the deadline with no
worker. An admission is kept 61 days (`admission_operations`, monthly partitions);
after that it reads as never admitted and a late capture is `admission_not_found`.

Holds and spend windows live in PostgreSQL under the customer's money lock, never
Redis. A spend window counts open estimates and captured amounts in the original
admission window; an expired hold reserves nothing but still counts until released
or captured, and a late capture counts in its original window. One held total
covers live estimates, pending credit refunds and running charges' unposted
accruals.

Embedded, `Engine.LocalMerchant(id)` returns a `*LocalClient` whose `AdmitTx`,
`CaptureAdmissionsTx` and `ReleaseAdmissionsTx` take a transaction from the host's
pool: the host's own row and its hold commit together.

`GetAdmission` / `GET /v1/app/admissions/{request_id}` and `ListAdmissions` (by
`customer_id`, `currency`, `state`, admission time) read holds without replaying
them; a denied request made no reservation and is `admission_not_found`. They need
`merchant:usage:read`.

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
nothing. `usage` carries `period_starts_at`, `period_ends_at`, `due_at` (billed
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
