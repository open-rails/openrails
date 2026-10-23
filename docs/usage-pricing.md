# Usage pricing

Usage is priced when it is recorded. A usage event (`POST /v1/app/usage-events`,
`RecordUsage`) is priced by each matching meter's rate card, in the transaction
that records it. The prepaid balance, admissions, spend windows and threshold
pass see the charge at once. A `dynamic` card charges the event's optional
`amount`; other cards price its quantities and ignore that amount.

## How an event is priced

Each meter whose event type is the event's, and that has a rate card in the
event's currency, takes a quantity from the event: 1 for a `count` meter, the
`value_property` for a `sum` meter (from `dimensions`, then `metadata`). A card's
`filter` can exclude the event; a meter that takes nothing charges nothing.

For each catalog-priced meter (and matrix cell), the customer's quantity so far in the event's
billing period is Q, and the event's quantity q. The event is charged

    R(Q + q) − R(Q)

where R is the card's price for a whole period's quantity. Q then grows by q.
R never falls as Q grows, so no charge is negative, and a period's charges sum
to R of its total exactly: per-event rounding never drifts. 1,000 events of
1,234 tokens at $0.15 per million each cost 185.1 micros, rounded where they
fall, and together cost exactly 185,100.

With [automatic usage billing](usage-billing.md), the customer's fixed monthly
anchor defines both pricing and invoice periods. A threshold payment does not
reset it. Explicit manual billing retains the merchant's
`billing_period_boundary`, anchored on the customer's first activity. The
quantities are kept in `meter_period_quantities`, 100 days per period.

The event records what each meter charged it: `charges`, one
`{meter_key, model, cell, quantity, amount}` per meter, and `amount`, their sum.

## Charge models

| Model | R(Q) |
|---|---|
| `per_unit` | `round(Q × unit_amount / divide_by)`, capped at `maximum_amount` per period; a `matrix` prices each value of one meter dimension in its own cell, with its own quantity |
| `tiered` | `graduated` only: each band's units at the band's rate, its `flat_amount` once the band is reached |
| `package` | `ceil((Q − free_units) / package_size) × amount` |

An `allowance.included` is taken from Q before the price applies, per period
and per cell, in the price's units (units of `divide_by` for `per_unit`).
Volume tiers are refused on a metered card: a later event reaching a cheaper
band would lower the price of earlier units, a negative charge. A metered card
states no `payment_term`: the customer's billing mode decides whether a charge
draws a prepaid balance or is invoiced.

## Dynamic prices

A rate card with `price: {model: dynamic, currency: USD}` has no price block
and requires a `sum` meter. Dynamic cards do not accept an allowance. The event still supplies units in the meter's
`value_property`. With a positive quantity the card charges the event's
`amount`, a nonnegative integer decimal string in the currency's native units.
Missing `amount` means zero; zero or missing units also means zero regardless
of the amount. There is no additional per-event price limit.

A matching dynamic card and a catalog card each charge independently. An event
with 10 units and `amount: "250000"` pays 250,000 micros to each dynamic card
that consumes it, plus any catalog charges. Its `charges` and the usage report
retain quantity and money, so the implied native-unit price is amount divided
by quantity. The supplied value is stored separately as `stated_amount`, even
when a catalog card ignores it. Retrying with a different amount, including
changing an absent amount to explicit zero, is `idempotency_key_reused`.

Failed usage must explicitly state its cost. It is never rated or counted in
meter-period quantities: the existing grace windows decide what is forgiven
and what is charged. Running charges keep their own hourly rate.

## Posting

Each charge draws the prepaid balance first; the rest is owed. Usage is never
refused for money: the work happened, and admission is the gate before it. A
prepaid customer past its balance owes the rest, and the owed refuses its next
admission. Owed charges are billed by the period's usage order (or the
threshold pass) on a line per meter, `metered:<meter>`.

An event dated in an earlier period (up to 35 days back) is priced at that
period's quantity and posted when it is recorded, so the open period's order
bills it, even when the earlier period's order was already billed.

A rate card change applies from the next event; nothing charged is priced
again. A replay of an event answers its first charges, whatever the card says
now.

## What is not priced

An event that no priced meter consumes is recorded and metered, and charges
nothing. A priced meter that cannot price an event refuses it with
`422 usage_not_priced`, and nothing is recorded: a matrix with no cell for the
event's value (`param` names the field, e.g. `metadata.model`), or an event
type whose rate cards are all in other currencies. A catalog gap is never free.

## Recipes

Per call: a `count` meter, `per_unit` with `unit_amount: 10000` ($0.01 per
call).

Per million tokens: one event per request with disjoint dimensions
(`input_tokens`, `cached_input_tokens`, `output_tokens`) and the model in
`metadata`; one `sum` meter per token type, each priced `per_unit` with
`divide_by: 1000000` and a `model` matrix whose cells are micros per million.
Cached tokens are their own meter, never a share of input.

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

A meter is declared in the catalog document or with
`PUT /v1/catalog/meters/{key}` (`SetMeter`, with its rate card).
