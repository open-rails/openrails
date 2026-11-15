# Running charges and balance notices

A running charge bills a customer at a rate per hour from the instant you start it
until you stop it: a rented machine, a reserved seat. Admissions fit work that
ends in seconds; a running charge fits work that runs for hours or days. Nothing is
written while it runs: what it has accrued is computed when the balance is read,
and reaches the ledger when it stops and once a day. OpenRails tells you when a
balance runs low or out; starting and stopping the work is yours.

## Routes

Your backend's, with `billing.MerchantUsageManage`. The durable `reference` owns
replay:

| Route | Go | Answers |
|---|---|---|
| `POST /v1/app/running-charges` | `StartRunningCharge` | 201; 200 for the same reference and terms |
| `POST /v1/app/running-charges/{reference}/stop` | `StopRunningCharge` | 200 with the final `charged_amount` |
| `GET /v1/app/running-charges/{reference}` | `GetRunningCharge` | charged as of now |
| `GET /v1/app/running-charges` | `ListRunningCharges` | newest first, by `customer_id`, `state`, `depleted` |

Embedded, `engine.LocalMerchant(id)` gives `StartRunningChargeTx` and
`StopRunningChargeTx`, which commit with the host's own row so a crash leaves both
or neither.

**Start** (`billing.StartRunningChargeParams`): `reference` (1–255 bytes, unique
per merchant, the identity), `customer_id`, `currency`, `rate_per_hour` (1 to
10^12 native units), `started_at` (default now; at most 60 s ahead, 35 days back),
`minimum_amount` (above 0, refused `402 insufficient_credits` unless capacity
covers it; 0 never refuses), `overdraft_amount` (how far below zero a prepaid
capacity may go; the smallest among a customer's charges applies) and `resource`.
A repeat with the same terms answers the charge; a changed term is
`422 idempotency_key_reused`.

**Cost**: `rate_per_hour` × whole seconds since `started_at` ÷ 3600, rounded half
up, per second with no minimum. **Stop** takes `stopped_at` (default now; at most
60 s ahead, not before the start or last posting); the same instant again answers
the same, another is `422 idempotency_key_reused`.

## The balance

`GET /v1/me`, `GET /v1/admin/customers/{customer_id}/balance` and `GetBalance`
answer `billing.Balance` with `accrued_amount` (charged, not yet in the ledger),
`available_amount` (balance − held − accrued), `accrual_rate_per_hour`, `as_of` and
`depletes_at`. A page shows `available_amount − accrual_rate_per_hour × hours since
as_of` until `depletes_at` without reading again. Capacity is available less owed
plus the overdraft (prepaid), or available plus the credit line left (arrears);
`depletes_at` counts credit lots that lapse on the way, drawn soonest-expiring
first. Every other spend path sees the accrual, as it would a hold.

The ledger is written when a charge stops; at each 00:00 UTC, through the boundary
exactly (one line per charge per day); before a credit lot expires, is revoked or
refunded (what accrued under it draws it first); and before usage is billed. A
posting draws the balance first and records the rest owed, one balance transaction
per lot drawn, source `running_charge`, id `<charge id>/<instant>`. `running_charges`
rows are permanent and travel in merchant archives.

## Balance notices

Watched: every prepaid balance, every balance with a running charge, and every
balance holding a lapsing credit lot. An arrears customer with neither is limited
at admission and told through [delinquency](spend-limits.md#delinquency).

- `balance.low`: what the customer may spend (capacity before any overdraft) is
  under its effective line.
- `balance.depleted`: capacity is at zero. It cannot be turned off.

The effective line is the larger of the amount line and the burn rate × the
runway. The burn rate is the running charges' rate plus an exponentially weighted
usage rate (one-hour time constant). Each part comes from the first that sets it:
the customer's `low_balance_lines` (staff `UpdateCustomer`, or the customer's
`PUT /v1/me/low-balance-lines/{currency}`), the merchant's
`settings.balance_notices`, or the built-in 24-hour runway with no amount line.
0 turns a part off; null inherits; `runway_hours` is 0–8760. `GET /v1/me` reports
each effective line with its `source` (`customer`, `merchant`, `default`).

```yaml
settings:
  balance_notices:
    runway_hours: 12
    low_amounts:
      - { currency: USD, amount: "2500000" }
```

A notice is sent once per crossing and re-armed only when a look after a funding
(deposit, grant, reinstatement, payment of what is owed, changed credit line) finds
the balance back above the line; changing a customer's line re-arms its low notice.
A change that crosses a line queues a look in its own transaction, and each look
queues the next for the instant it names (a lapse, a 00:00 UTC posting, a projected
crossing); a failed look is retried and then taken by the hourly billing pass.
Overshoot is at most the watcher's minute plus your own reaction, times the rate,
and is owed.

Notices arrive as [events](events.md): host events `balance.low` and
`balance.depleted` with `customer_id`, `currency`, `available_amount`,
`owed_amount`, `accrual_rate_per_hour`, `usage_rate_per_hour`,
`low_balance_amount`, `as_of`, `depletes_at` and `runway_ends_at`; and customer
facts `balance_low` / `balance_depleted` (low and depleted in one look are one
`balance_depleted`) for your application to deliver. As state,
`GET /v1/app/running-charges?depleted=true` lists charges of customers past their
run-out instant: confirm with `GetBalance`, stop your work, then stop the charge.
