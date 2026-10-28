# Balance notices

OpenRails tells you, and your customer, when a balance runs low and when it
runs out: once per crossing, re-armed only by a funding.

| | Owner |
| --- | --- |
| Watching every prepaid balance and every balance with a running charge | **OpenRails** |
| Host events `balance.low` and `balance.depleted` | **OpenRails** |
| Customer facts `balance_low` and `balance_depleted` | **OpenRails** |
| Delivering them to the customer (inbox, email) | **your application** |
| Stopping work when a balance runs out | **you** |

## Who is watched

Every prepaid balance (customer and currency), every balance with a
[running charge](running-charges.md) and every balance holding a credit lot
that lapses. An arrears customer with neither is not watched: its limit is the
credit line, refused at admission and covered by the
[delinquency notices](arrears-delinquency.md).

A watch costs nothing while nothing changes. A deposit, a credit grant, a
spend, a capture, a hold, an expiry or a refund marks it due, and every
minute one job looks at the due watches. A burst of spends is one look.
Time alone moves a balance in two ways, and each look schedules both: a
running charge's accrual, and a credit lot's lapse. The look at a lapse
expires the lot; a balance read leaves it out of `available_amount` from its
lapse on.

## The lines

What the customer may spend is its capacity before any overdraft: prepaid,
what is available less what it owes; arrears with a running charge, what is
available plus the credit line left.

- `balance.low`: what it may spend is under the effective line.
- `balance.depleted`: capacity is at zero. It cannot be turned off.

The **effective line** is the larger of the amount line and the burn rate
times the runway line. The **burn rate** is the running charges' rate plus the
usage rate: an exponentially weighted rate of usage spend with a one-hour time
constant, sampled at each look.

Each part of the line comes from the first that sets it:

1. The customer's own (`low_balance_lines`), set by staff with
   `Client.UpdateCustomer` or by the customer with
   `PUT /v1/me/low-balance-lines/{currency}` (billing-ui
   `setLowBalanceLine`).
2. The merchant's `settings.balance_notices`:

   ```yaml
   settings:
     balance_notices:
       runway_hours: 12
       low_amounts:
         - currency: USD
           amount: "2500000"
   ```

3. Built in: a 24-hour runway and no amount line.

0 turns a part off; null inherits. `runway_hours` is 0 to 8760.
`GET /v1/me` answers each balance's effective line in `low_balance_lines`, with
its `source` (`customer`, `merchant` or `default`).

## One notice per crossing

A notice is sent once and stays sent until a look finds the balance back above
its line after a funding: a deposit, a credit grant, a reinstated grant, a
payment of what is owed, or a changed credit line. A released hold or a falling
rate does not re-arm it, so a customer hovering at the line is told once.
Changing a customer's line re-arms its low notice.

## Host events

[`GET /v1/app/events`](event-log.md) carries the balance payload in `data`:

| Field | |
| --- | --- |
| `customer_id`, `currency` | Whose balance |
| `available_amount`, `owed_amount` | As the crossing was seen, at `as_of` |
| `accrual_rate_per_hour` | The running charges' rates, summed |
| `usage_rate_per_hour` | The usage rate |
| `low_balance_amount` | The effective line the crossing was judged at |
| `depletes_at` | When capacity reaches zero from running charges and lapsing lots, exact; null when neither moves it |
| `runway_ends_at` | When capacity reaches zero at the burn rate; null at rate zero |

```json
{"customer_id":"…","currency":"USD","available_amount":"2400000","owed_amount":"0",
 "accrual_rate_per_hour":"0","usage_rate_per_hour":"800000","low_balance_amount":"2500000",
 "as_of":"2026-10-11T14:00:00Z","depletes_at":null,"runway_ends_at":"2026-10-11T17:00:00Z"}
```

## Customer facts

Each crossing is also a [notification event](customer-notifications.md),
`balance_low` or `balance_depleted`, with `action_requested`. Its `data`
carries `currency`, `available_amount`, `owed_amount`, `low_balance_amount`
and `runway_ends_at`. Low and depleted crossed in one look are one
`balance_depleted`. Your application renders and delivers it: "You have 2.40
USD left, about 3 hours at your current rate", linking to your billing page.
