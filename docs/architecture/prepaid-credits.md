# Prepaid credits

A customer's prepaid balance in a currency is a set of credit lots: staff grants, credit packs and deposits a customer buys, and the credit a recurring plan delivers each paid cycle. Usage, admissions and running charges spend the lots soonest expiry first; a lot can lapse, be revoked, or be reversed by a refund or chargeback of the payment that bought it. Each lot is a `credit_grants` row; every movement of its units is a [ledger](ledger.md) transfer naming it in `credit_grant_id`.

## Concepts

- **Lot**: a `credit_grants` row: `units` of `unit` (a currency, or later a merchant unit), `starts_at`, `expires_at` (NULL never lapses), its `funding_source` (`purchase`, `recurring`, `grant`, `promotion`, `import`) and `account_id`, the customer's `customer_balance` account its units sit in. A revocation is a `credit_grant_events` row (`revoke`; `reinstate` clears it), whose trigger sets the lot's `revoked_at`.
- **Remaining**: `units_credited - units_debited`. The trigger `ledger_transfers_apply_lot_counters` keeps both from the legs that name the lot and touch its balance account: deposits, bonuses, restores and reinstatements credit it; spends, repayments, expiries, revocations and refunds debit it. A leg naming the lot on another account (cash, revenue, retired value) moves neither, and a deposit that never posted leaves nothing to spend. `credit_grants_units_posted_check` keeps `units_debited <= units_credited`: a lot never gives more than it holds.
- **Purchased lot**: `paid_amount` and `paid_currency` are set, and `payment_id` and `order_line_id` name what bought it. Face is the credit delivered; paid is the cash of the order line that bought it. A gift has neither.
- **Reserved**: `units_reserved`, what the pending refunds of a purchased lot's payment will withdraw (`credit_grant_reservation`): their share of its paid amount, up to what no reversal returned, rounded up. **Available** in a lot is remaining less reserved.
- **Held**: the `balance` part of live admission and lease holds (`limit_holds`, `limit_hold_parts`), what pending refunds reserve of a lot (all of a lapsed one not yet expired), unposted usage accruals and running-charge accrual (`GetFinancialHeldAmount`), all outside the ledger. **Available** is balance less held (`billing.Balance.AvailableAmount`).
- **Grant state** (`billing.CreditGrantState`), the first that holds: `revoked`; `pending`, none of its units posted; `refunded`, refunds of its payment took all of it, whatever its expiry; `expired`, past its expiry; `spent`, nothing left (refunds' part in `refunded_amount`); `scheduled`, before it starts; else `active`.
- **Balance watch** (`balance_watches`): when to look at a customer's balance
  next, and which low or depleted notice it has sent.

## How it works

### Funding

- **Staff**: `Service.CreateCreditGrants` validates 1 to `billing.MaxBatchItems` items and calls `MoneyService.DepositBatch`: one transaction, balances locked in (customer, currency) order. An item's `funding_source` (`billing.CreditGrantFunding`) is `grant` (the default), `promotion` or `import`; its invoker is at most 512 bytes. `depositTx` takes the customer lock and answers a known `source_id` (`credit_grants_customer_id_source_id_key`) with the same amount, currency, expiry and, when the retry names one, funding source as `Replayed`; other terms are `idempotency_key_reused`. Otherwise `credits.Lots.Issue` records the lot and its `Materialize` posts the customer's unposted usage first, then the `deposit` at (`deposit`, `grant`, lot id); `owed.Repay` pays existing owed from the new lot. Change a usage order's card charge leaves beyond the order, collected or observed at NMI, is a `purchase` lot paid at face, its invoker the customer.
- **Purchase**: a paid order line with a `credit_grant` reaches `purchasedcredits.Service.Fund` in the settlement transaction (`fundCreditLine`, or `FulfillmentService.grantPurchasedCredits` for a one-line payment). It checks the payment succeeded in the lot's currency and matches its line, keeps one lot per payment (`credit_grants_payment_id_key`), records it `purchase` or, for a recurring price's line, `recurring`, and posts through `Ledger.PurchasedDeposit`: `min(face, paid)` from `processor_clearing`, excess face from `promotional_funding` (`deposit_bonus`), excess cash to `platform_revenue`. Only the paid part repays owed. An order buys at most one credit product.
- **Recurring**: each paid renewal of an engine-owned recurring credit price funds
  the order line's frozen promise (`internal/modules/orders/renewal.go`), with its
  own expiry from delivery ([recurring credits](../client.md#credits)).

### Spending

- An admission holds its estimate against available. A capture
  (`MoneyService.CaptureAdmission`) closes the hold and adds the priced usage to
  the day's `usage_accruals` row, which stays held until it posts; a release
  returns the hold (OR-J22-S01).
- A posting (`postUsageAccrualsTx`, running-charge postings) runs `spendBalanceThenOwedTx`. The balance leg is capped at what the spendable lots have available (`ListSpendableCreditLots`: started, not lapsed, not revoked, with units left, `credit_grants_live_idx`), and `credits.Lots.Spend` draws them soonest expiry first, one `credit_spend` per lot at the posting's coordinate. The rest is `owed_accrual` plus `owed_items`. A posting of admitted usage is never re-gated (`preAuthorized`): beyond the balance it is owed even without a credit line.
- Unposted usage posts before a deposit. Before a lot is expired, revoked or refunded, the running charges and usage that accrued while it lived post first (`SettleRunningCharges`, through the `credits.PostRunningCharges` and `credits.PostUsageAccruals` hooks the money package installs).

### Lapse

A lot with `expires_at` is watched (`ledger.WatchLapsingCredit`). Spending stops at `expires_at` because the lot query excludes it, before any expiry posts. `credits.Lots.ExpireLapsedBy` then posts the remainder as `credit_expire` at (`credit_expire`, `credit_expiry`, lot id), after settling what accrued up to the lapse. Callers include the balance watch (`MoneyService.WatchBalanceInTx`),
admission capacity (`AdmissionCapacityTx`), usage settlement (`PriceUsageTx`), a
reversal, a released refund (`MarkFailed`) and a running charge's stop.

### Revocation

`MoneyService.RevokeCreditGrant` runs inside `destructive.RunLocal` (`destructive.RevokeCredit`) under the customer lock. It posts accrued running charges and usage, refuses `credit_grant_held` when holds or a pending refund (`units_reserved`) need the remainder and `credit_grant_unavailable` when nothing remains, and `credits.Lots.Revoke` appends a `revoke` event and moves the remainder to `revoked_credits` (`credit_revoke`). Revoking a revoked lot replays.

### Refunds and chargebacks

- Before a refund is reserved, `purchasedcredits.Service.ValidateRefund` requires the lot's share of the requested cash (proportional, rounded up) to be unspent, unreserved and unheld. `ReserveRefund`, `MarkFailed` and every settled reversal (`syncPurchasedCreditRefund`) then write the lot's `units_reserved` (`SyncCreditGrantReservation`) in their transaction. A prune never takes a reversal of a lot's payment (`PaymentHasProtectedDependents`); a reservation that drifts anyway is `ledger-audit` lot drift and the `derive.credit_reservation.mismatch` finding, which convergence repairs; it reads only lots that reserve something or whose payment has a pending refund (`credit_grants_reserved_idx`, `payments_pending_credit_refunds_idx`).
- `ApplyReversal` posts each settled reversal once, at
  (`purchased_credit_refund`, leg, reversal id). The cash share is the reversal up
  to the credit line's unreturned cash; the face share is proportional. It takes
  face from the lot first, then expired or revoked value, then debt the lot repaid
  (owed again), and books the rest to `credit_refund_loss`. Every leg routes
  through `credit_refund_clearing`, which nets to zero. A recovery restores only
  what its own reversal took.

### Balance watch

`WatchChange` queues an `openrails.balance_look` job when a transfer crosses a
line, or on every change while the watch has `look_each_change` (a running charge
or a lapsing lot); `openrails.balance_watch` takes watches due by `next_check_at`.
`WatchBalanceInTx` expires lapsed lots, posts running charges through the last
00:00 UTC, sends `balance.low` or `balance.depleted` once per crossing (a funding
re-arms them) and sets the next look.

## Interfaces

- Go: `Client.CreateCreditGrants`, `ListCreditGrants`, `GetCreditGrant`,
  `RevokeCreditGrant`, `GetBalance`, `ListBalanceTransactions`; spending through
  `Admit`, `CaptureAdmissions`, `ReleaseAdmissions`, `ExtendAdmissions`, and
  `LocalClient.AdmitTx`, `CaptureAdmissionsTx`, `ReleaseAdmissionsTx`.
- HTTP: `POST /v1/admin/credit-grants`, `POST /v1/admin/credit-grants/{id}/revoke`
  (`merchant:customer:manage`); `GET /v1/admin/credit-grants`,
  `GET /v1/admin/credit-grants/{id}` (`merchant:customer:read`); the customer's
  `GET /v1/me/balance/transactions` and `PUT /v1/me/low-balance-lines/{currency}`;
  `POST /v1/app/admissions` and its `capture`, `release` and `extend`
  (`merchant:usage:manage`).
- Catalog: a product's `credit_grant` (`catalog.CreditGrantSpec`: `currency`,
  `amount` or `from_payment`, `expires_after_days`); a price's `customer_amount`.
- Events: `balance.low`, `balance.depleted` (`billing.BalanceHostEvent`).
- Error codes: `credit_grant_held`, `credit_grant_not_found`,
  `credit_grant_unavailable`, `currency_unsupported`, `idempotency_key_reused`,
  `invalid_param`.
- Docs: [prepaid credits](../client.md#credits), [recurring credits](../client.md#credits),
  [merchant operations](../client.md#credits).

## Guarantees

### 1. Exactly-once provider calls
Does not apply: credits call no provider; checkout charges a pack and refunds return it, and lots move inside their transactions.

### 2. Agreement with providers
A purchased lot exists only for a succeeded payment and in the same transaction
(`Fund` checks `payment.Status`). A provider reversal withdraws credit when it is
recorded (`syncPurchasedCreditRefund`), whichever path reported it. A pending
refund holds its credit until the provider answers; a refused one releases it and
expires the lot if it lapsed meanwhile.

Checks:
- `Fund` refuses a payment that is not `succeeded`, is a reversal, or differs in
  customer or currency.
- `recordRefund`, `CompleteRefundReservation`, `LinkRefundedPayment` and
  `PaymentService.Create` call `syncPurchasedCreditRefund` in their transaction.
- OR-J22-S07: a pending credit-line refund holds the whole lot until it settles.

### 3. Exact money
Lots and spends are `int64` native units in one currency; a purchased lot is in
its payment's currency. Proportional face uses `big.Int` (`proportional`):
reservations round up, withdrawals take the difference of cumulative rounded
shares, so partial reversals sum to the full one. Deposits and purchases refuse to
overflow the balance; `fundCreditLine` refuses an amount that overflows with
quantity.

Checks:
- `ApplyReversal` takes `faceDelta` as the difference of `proportional` at the
  credit line's cumulative reversed cash after and before this reversal.
- `ListSpendableCreditLots`, `GetFinancialHeldAmount`, `GetAdmissionCapacity` and `ValidateRefund` read the one reservation, `units_reserved`.
- OR-J22-S05: $20 a cycle delivers 25 credits; refunds return only cash.

### 4. Access control and tenant isolation
Staff routes need `merchant:customer:read` or `manage`, admission routes `merchant:usage:manage`; a customer reads its own balance and sets its own line (`selfAccountPayer`). `credit_grants` keys on `(merchant_id, id)` with composite foreign keys to `customers`, its balance account (customer and currency included), `payments`, `products` and `order_lines`; `ledger_transfers.credit_grant_id` and `credit_grant_events` reference it.

Checks:
- Each query in `internal/db/queries/credit_grants.sql` and `purchased_credits.sql` filters `merchant_id`.
- `GetCreditGrant` resolves a grant by merchant and id, so another merchant's id is
  `credit_grant_not_found`.
- OR-J22-S01 reads the customer through another merchant and sees nothing.

### 5. Contracts match the code
The methods, `billing.CreditGrant`, `CreditGrantState`, `CreateCreditGrantParams`,
`Balance` and the routes are in `api/go.txt` and `api/openapi.json`; the tables
are in `api/schema.txt`. The in-process client calls the same routes as the remote
one.

Checks:
- `go run ./scripts/contracts` shows no removed line for these.
- A new grant state, funding source or catalog field adds a value without renaming one.

### 6. Data lifecycle
`credit_grants`, `credit_grant_events` and `usage_accruals` are permanent; `balance_watches` and `customer_low_balance_lines` are state. `limit_holds` and `limit_hold_parts` are partitioned by month, and the held total names a range: `created_at` and `hold_created_at` at or after `limits.HeldSince`.

Checks:
- Those classes are in `internal/retention/tables.go` and the table comments.
- `GetFinancialHeldAmount` and `GetAdmissionCapacity` bound both columns by
  `held_since`; a capture addresses its hold by `created_at`.
- `credit_grants` and `credit_grant_events` are in `NeverRollbackableTables`. A lot refuses DELETE and any change but its counters, `revoked_at` and `units_reserved`; only the transfer and event triggers move the first two (`guard_credit_grant_facts`), and events refuse UPDATE and DELETE.
- A restore inserts lots with their counters; `check_billing_restore_credit_grants` proves them against the restored legs and events at commit.

### 7. Work scales with activity
Looks are queued by crossings; due watches are read by `balance_watches_next_check_at_idx` and daily postings by `usage_accruals_due_idx`. Lot reads are per customer and currency, and read a lot's columns, not its transfers: exhausted and revoked lots leave `credit_grants_live_idx`, and reservations are read by `credit_grants_reserved_idx`.

Checks:
- `ListDueBalanceWatches` and `ListDueUsageAccruals` filter on the due instant and
  page in key order.

### 8. Abuse protection
Customers buy credit only through checkout (`POST /v1/me/orders`: the `purchase`
bucket and card-testing checks). `middleware.AdminOperationGrant` meters each
invoker by item: 10 a minute, shared by `UpdateCustomer`, `AdjustCustomerLimit`,
`CreateCreditGrants` and `CreateProductAccess`; going over locks the invoker out
for an hour. An application acting as itself is not metered. Admission routes
are server-to-server; they, the customer's balance reads and its low-balance line
are in no bucket.

Checks:
- `CreateCreditGrants` carries `Limit: middleware.AdminOperationGrant` in
  `internal/http/routes/credits.go`.
- No customer route funds or revokes a lot.

### 9. Destructive-action safeguards
Revocation is a `destructive.RevokeCredit` command, counted per lot, held by an
incident and recorded with its actor; the `revoke` event names the actor too. A
staff refund that withdraws credit is a `destructive.Refund`. Grants are additive
and spend nothing. Nothing deletes a lot.

Checks:
- `RevokeCreditGrant` builds its command with `destructive.CommandFromContext` and
  runs inside `destructive.RunLocal`.
- OR-J34-S01 to S04 hold and clear revocations at the ceilings.

## Tests

- J22: OR-J22-S01 grant, hold, capture and release (`TestCredits`); OR-J22-S02
  capture against revocation (`TestAdmissionBoundaries`); OR-J22-S03, OR-J22-S06,
  OR-J22-S50 expiry, lapse and admission after a lapse (`TestCreditExpiry`);
  OR-J22-S04 atomic batches and replay (`TestCreditAtomicity`,
  `TestLedgerMaintenance`); OR-J22-S05 recurring credits (`TestRecurringCredits`);
  OR-J22-S07 to S09 refunds, chargebacks and won disputes of a mixed order
  (`TestMixedOrderCreditRefunds`).
- OR-J22-S10 a lot's view equals its ledger counters through spend, expiry, revocation, refund and restore, and `ledger-audit` names lot drift; OR-J22-S11 a refunded pack raises no refunded-source grant finding; OR-J22-S13 two lapsed lots expire only what each still holds; OR-J26-S62 a usage order's change is a bought lot; OR-J40-S04 a deposit that did not post is pending and not spendable until convergence posts it (`TestCreditLotCounters`); OR-J31-S15 the move out of `grants` keeps every lot's old remainder and pending-refund share, and refuses a lot drawn before its deposit (`TestCreditGrantsMigration`).
- Also: OR-J24-S22 and OR-J04-S44 (a lapsed lot is never drawn); OR-J24-S12 (two
  leases within one balance); OR-J23-S02, OR-J23-S14, OR-J23-S26 (holds);
  OR-J25-S37, OR-J25-S40 to S42 (balance looks); OR-J34-S01 to S04 (revocation
  ceilings).
- No Go package test covers this system; `sqlc vet` prepares its queries.

Gaps:
- No scenario buys a customer-chosen deposit (`customer_amount`, `from_payment`).
- Credit refunds are tested only on Stripe; no NMI-funded lot is reversed.
- `credit_reinstate` has no writer; `BalanceTransactionReinstate`, which a won
  dispute's `credit_refund_restore` reads as, is asserted by no test.
- Spend reads grow with a customer's lifetime lots: a spent lot with no expiry
  stays in every `ListSpendableCreditLots` read.
