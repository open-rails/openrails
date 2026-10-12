# Running charges

A running charge bills a customer at a rate per hour, from the instant you
start it until you stop it: a rented machine, a reserved seat, anything whose
cost is rate × time. Admissions fit a request that ends in seconds; a running
charge fits work that runs for hours or days.

Nothing is written while a charge runs. What it has accrued is computed when
the balance is read, and reaches the ledger when the charge stops and once a
day. One stored instant per customer says when its balance is next looked at.

| | Owner |
|---|---|
| The balance, what has accrued and when it runs out | **OpenRails** |
| Refusing other spend the accrual has already used | **OpenRails** |
| Telling you a balance is low or has run out, durably | **OpenRails** |
| Starting and stopping the work, and its instants | **you** |

## Routes and methods

Your backend's routes, behind `billing.MerchantUsageManage` or
`billing.RootUsageManage` ([routes](api/routes.md)). The durable `reference`
owns replay; no `Idempotency-Key` header is needed:

| Route | Go method | Answers |
| --- | --- | --- |
| `POST /v1/app/running-charges` | `Client.StartRunningCharge` | 201; 200 for the same reference and terms |
| `POST /v1/app/running-charges/{reference}/stop` | `Client.StopRunningCharge` | 200 with the final `charged_amount` |
| `GET /v1/app/running-charges/{reference}` | `GetRunningCharge` | 200, charged as of now |
| `GET /v1/app/running-charges` | `ListRunningCharges` | 200, newest first, by `customer_id`, `state` and `depleted` |

An embedded host obtains `local, err := engine.LocalMerchant(merchantID)` and
uses `local.StartRunningChargeTx` and `local.StopRunningChargeTx` inside its transaction
on the same database: commit the start with the row that says the work is
ready, and the stop with the row that says it was released, so a crash leaves
both or neither. The shared `Client` has no transaction methods; remote callers
use ordinary `StartRunningCharge` and `StopRunningCharge` requests.

## Start

`billing.StartRunningChargeParams`:

| Field | |
| --- | --- |
| `reference` | Your name for what you charge for: 1 to 255 bytes, unique per merchant. It is the charge's identity |
| `customer_id`, `currency` | Who pays, in which currency |
| `rate_per_hour` | Native units per hour, 1 to 1,000,000,000,000 |
| `started_at` | Your instant, now when absent: at most 60 seconds ahead and 35 days back |
| `minimum_amount` | Above 0, the start is refused `402 insufficient_credits` unless the customer's capacity covers it, and nothing is written. 0 never refuses: use it when the work is already delivered |
| `overdraft_amount` | How far below zero a prepaid customer's capacity may reach before it counts as run out |
| `low_balance_amount` | The balance under which the customer counts as low; 0 is no line |
| `resource` | Attribution, carried on the ledger entries |

Starting a reference again with the same terms answers its charge, running or
stopped; a changed term is `422 idempotency_key_reused`, with `param` naming
it. `minimum_amount` is a gate for the call, not a term: a repeat is not gated
again. A customer never funded gets its balance account.

Among a customer's running charges in one currency the smallest
`overdraft_amount` and the largest `low_balance_amount` apply.

## What a charge costs

`charged_amount` is `rate_per_hour` × whole seconds since `started_at` ÷ 3600,
rounded half up to the native unit. It is charged per second, with no minimum.
The instants are yours, so a retry never moves the amount.

A stop takes `stopped_at` (now when absent): at most 60 seconds ahead, and not
before the charge started or was last posted. It answers the charge with its
final `charged_amount`. Stopping again at the same instant, or with none,
answers the same; another instant is `422 idempotency_key_reused`.

## The balance while a charge runs

`GET /v1/me`, `GET /v1/admin/customers/{customer_id}/balance` and `GetBalance`
answer `billing.Balance`:

| Field | |
| --- | --- |
| `accrued_amount` | What the customer's running charges have charged and the ledger does not hold yet |
| `available_amount` | `balance_amount` − `held_amount` − `accrued_amount` |
| `accrual_rate_per_hour` | The rates of its running charges, summed |
| `as_of` | The instant the answer was computed |
| `depletes_at` | When its capacity reaches zero at that rate; null with no running charge |

A page shows `available_amount − accrual_rate_per_hour × hours since as_of`
until `depletes_at`: fresh to the second, without reading again.

Capacity is what is available less what is owed, plus the overdraft (prepaid),
or what is available plus the credit line still left (arrears). `depletes_at`
counts credit that expires on the way: lots are drawn soonest-expiring first
and what is left of one is gone when it lapses.

Every other spend path sees the accrual: an admission, a spend, a withdrawal
or a refund that the accrued amount no longer leaves room for is refused, as a
hold would refuse it.

## When the ledger is written

- **The stop** posts the rest of the charge.
- **Each 00:00 UTC**, every running charge is posted through the boundary
  exactly, whenever the watcher gets to it: one line per charge per day, and
  no posting straddles a day or a month.
- **Before a credit lot is expired, revoked or refunded**, what accrued while
  the lot lived is posted first and draws that lot, so credit never expires
  under a charge it should have paid.

A posting draws the balance first and records the rest as owed, as a capture
past the balance does; the customer's next funding repays what is owed first.
Each posting is one balance transaction per credit lot drawn, with source
`running_charge` and source id `<charge id>/<instant posted through>`. Any
number of postings sums to `charged_amount`.

## Running out

OpenRails does not stop your work; it tells you when to. Each customer with a
running charge has one watch: the earliest of the instant its balance goes
under the low line, the instant its capacity reaches zero, and the next 00:00
UTC. A deposit, a capture, a spend, a refund, an expiry or a new hold marks the
watch due; every 30 seconds one job looks at the watches that are due, and at
nothing else.

It reaches you two ways:

- **Events** ([host events](api/endpoints.md#host-events)), once per crossing
  and armed again when the balance is back above the line: `balance.low`
  (what is available less what is owed is under the low line) and
  `balance.depleted` (capacity is at zero). Each carries `balance`:
  `customer_id`, `currency`, `available_amount`, `owed_amount`,
  `accrual_rate_per_hour`, `as_of` and `depletes_at`.
- **State**: `GET /v1/app/running-charges?depleted=true`
  (`billing.RunningChargeListParams` with `Depleted`) lists the running charges
  of customers whose stored run-out instant has passed, whether or not the
  watcher has looked yet. Confirm with `GetBalance`, stop what you run, then
  stop the charge.

Overshoot past the line is at most the watcher's 30 seconds plus your own
pass, times the rate. It is real spend: it is posted as owed, and the next
funding repays it.

## Retention

`running_charges` rows are permanent. A merchant archive carries them, running
ones included; `balance_watches` is derived, and a restore watches every
running charge again.
