# Credits

A customer's prepaid balance in a currency is a set of credit lots: staff grants, credit packs and deposits a customer buys, and the credit a recurring plan delivers each paid cycle. Usage and running charges spend the lots soonest expiry first; a lot can lapse, be revoked, or be reversed by a refund or chargeback of the payment that bought it. Each lot is a `credit_grants` row; every movement of its units is a transfer in the units book of [Books](books.md) naming it in `credit_grant_id`, and a paid lot's money moves with them in the general ledger.

## Concepts

- **Lot**: a `credit_grants` row: `units` of `unit` (a currency, or one of the merchant's own units: `settings.currencies`), `starts_at`, `expires_at` (NULL never lapses), its `funding_source` (`purchase`, `recurring`, `grant`, `promotion`, `import`) and `account_id`, the customer's `customer_units` account its units sit in. A revocation is a `credit_grant_events` row (`revoke`; `reinstate` clears it), whose trigger sets the lot's `revoked_at`.
- **Remaining**: `units_credited - units_debited`. The trigger `ledger_transfers_apply_lot_counters` keeps both from the legs that name the lot and touch its balance account: deposits, bonuses, restores and reinstatements credit it; spends, repayments, expiries, revocations and refunds debit it. A leg naming the lot on another account (cash, revenue, retired value) moves neither, and a deposit that never posted leaves nothing to spend. `credit_grants_units_posted_check` keeps `units_debited <= units_credited`: a lot never gives more than it holds.
- **Purchased lot**: `paid_amount` and `paid_currency` are set, and `payment_id` and `invoice_line_id` name what bought it. Face is the credit delivered; paid is the cash of the invoice line that bought it, always in a registry currency (`credit_grants_paid_currency_registry_check`): a pack of a merchant's unit is paid in any currency, its rate paid over units, bonus units included. A gift has neither; a lot imported without a payment is a gift unless the import declares a paid amount.
- **Rate and lot money**: a paid lot's `units` are worth its `paid_amount` at the rate paid ÷ units, a pack's bonus included ($50 for 55 credits: $0.909 a credit). `deferred_credited - deferred_debited` is its money still deferred in the general ledger, kept by `ledger_transfers_apply_lot_deferred` from the legs on its customer's `deferred_revenue_credit` in `paid_currency`. n units left are worth `paid - floor(paid × (units - n) ÷ units)` (`books.CreditLot.Worth`), so the last unit carries the remainder.
- **Refunding**: `units_refunding`, what the pending refunds of a purchased lot's payment took off its balance when they were requested (`credit_grant_refunding`): off the balance, their money still deferred until each refund settles.
- **Available**: the balance less its pending usage and running-charge accrual (`GetUnpostedAmounts`; `billing.Balance.AvailableAmount`). A lapsed lot is in the balance until its expiry posts, which its look does at the lapse; spending never draws it.
- **Grant state** (`billing.CreditGrantState`), the first that holds: `revoked`; `pending`, none of its units posted; `refund_pending`, pending refunds of its payment hold units off its balance; `refunded`, settled refunds or chargebacks took all of it, whatever its expiry; `expired`, past its expiry; `spent`, nothing left; `scheduled`, before it starts; else `active`. `refunded_amount` counts settled refunds and chargebacks; `refunding_amount` is what pending refunds hold off the balance until they settle (back on the lot if refused).
- **Balance watch** (`balance_watches`): when to look at a customer's balance
  next, and which low or depleted notice it has sent.

## How it works

### Funding

- **Staff**: `Service.CreateCreditGrants` validates 1 to `billing.MaxBatchItems` items and calls `MoneyService.DepositBatch`: one transaction, balances locked in (customer, currency) order. An item's `funding_source` (`billing.CreditGrantFunding`) is `grant` (the default), `promotion` or `import`; its invoker is at most 512 bytes. `depositTx` takes the customer lock and answers a known `source_id` (`credit_grants_customer_id_source_id_key`) with the same amount, currency, expiry and, when the retry names one, funding source as `Replayed`; other terms are `idempotency_key_reused`. Otherwise `credits.Lots.Issue` records the lot and its `Materialize` posts the `deposit` at (`deposit`, `grant`, lot id); `owed.Repay` pays existing owed from the new lot. Change a usage invoice's card charge leaves beyond the invoice, collected or observed at NMI, is a `purchase` lot paid at face, its invoker the customer.
- **Purchase**: a paid invoice line with a `credit_grant` reaches `purchasedcredits.Service.Fund` in the settlement transaction (`fundCreditLine`). It checks the payment succeeded in the lot's currency and matches its line, keeps one lot per payment (`credit_grants_payment_id_key`) and records it `purchase` or, for a recurring price's line, `recurring`. Only the paid part repays owed. An invoice buys at most one credit product.
- **Deposit**: every lot posts one plain `deposit` of all its units, from `units_sold` for a paid lot and from `units_granted` for a gift. A paid lot's price is deferred in the same transaction (`books.Books.IssueCredit`): the payment's cash, booked when the payment was stored (`books.ReceivePayment`, which the issue replays), is applied at (`credit_issue`, `payment`, payment id), like a bill's line: `paid_amount` moves from `unapplied_cash` to `deferred_revenue_credit` (`cash_applied`), never more than the payment's receipt left unapplied (`GetPaymentCashApplication`); what does not fit raises `books.credit.unapplied`. An import declaring a paid amount without a payment is deferred from `opening_balance_equity` at (`credit_issue`, `credit_grant`, lot id). A usage invoice's change, recorded before its payment, is deferred once the payment is (`credits.Lots.Fund`). The deferred constraint trigger `credit_grants_require_books` refuses a commit that leaves a new paid lot with neither its `credit_issue` leg nor that finding (`credit_books_unposted`).
- **Merchant units**: a `credit_grant` in one of the merchant's units names its amount, and any price of the product buys it (`models.AcceptCreditGrant`); `Fund` takes a payment in another currency for it. Its money is in that currency; nothing is owed in a unit, so it repays no owed.
- **Recurring**: each paid renewal of an engine-owned recurring credit price funds
  the invoice line's frozen promise (`internal/modules/invoices/renewal.go`), with its
  own expiry from delivery ([recurring credits](../client.md#credits)).

### Spending

- A gate decides a request against available and what it has in flight itself;
  it never reports the balance in flight. A lease sync takes its usage into the
  event store, pending at its provisional price until it posts (OR-J22-S01).
- A posting (`postUsageTx`, of usage and running charges together) runs `spendBalanceThenOwedTx` once for what it takes. The balance leg is capped at what the spendable lots have available (`ListSpendableCreditLots`: started, not lapsed, not revoked, with units left, `credit_grants_live_idx`), and `credits.Lots.Spend` draws them soonest expiry first, one `credit_spend` per lot at the posting's coordinate. The rest is `owed_accrual` plus `owed_items`; a usage posting's owed is also usage revenue receivable in the general ledger (`books.Books.AccrueUsage`, `usage_accrued`), and a period whose price fell gives its difference back as a lot priced at face (`books.Books.ReverseUsage`, `usage_reversed`: `revenue_adjustments` to `deferred_revenue_credit`). A posting of usage is never re-gated (`preAuthorized`): beyond the balance it is owed even without a credit line.
- Each lot's leg releases its money at the lot's rate, cumulatively (`books.Books.MoveCredit`): `deferred_revenue_credit` to `usage_revenue` (`credit_released`) for the difference between what the lot had deferred and the worth of its units left, at the leg's coordinate. `credits.Lots.SpendInto` sends it to an invoice line's `deferred_revenue` instead, for a product priced in the lot's unit. A gift in a registry currency grosses usage revenue up at face, `promotional_credit` to `usage_revenue` (`promotional_credit_applied`); a gift in a merchant's own unit posts nothing.
- Owed repaid from a lot (`owed.Repay`) is repaid at face (`books.Books.RepayFromCredit`): as many of the units as the general ledger holds receivable repay it, `usage_receivable` falling by them, the lot's money at its rate paying for them (`usage_repaid`) and `promotional_credit` for the rest, a gift's all promotional. The rest repay owed that accrued before the credit books, which has no receivable, and are revenue as a spend is (a gift's at (`deposit`, `grant:unaccrued`, ...)). The split depends only on the books when it posts.
- A deposit posts nothing: pending usage and running charges post at the end of their accounting day, or first when lots are drawn or retired. Before a lot is expired, revoked or refunded, or an invoice is paid from the balance, the usage and the running charges that accrued while it lived post first, on one receipt (`SettleRunningCharges`, through the usage ledger the runtime binds, `db.UsageLedger`); an expiry posts through the lapse, expires, then posts the usage since, so usage after it never keeps the lot from what came before (OR-J22-S03, OR-J24-S61).
- A merchant's unit is never owed: its owed account is refused (`ErrMerchantUnitOwed`, `ledger_accounts_owed_currency_check`). A gate decides a request in a unit against its balance alone (a billing policy's money limits do not bind it). Usage in a unit arrives on a lease sync, in a unit the merchant declares (`MoneyService.UsageCurrencies`). A posting in a unit credits a period whose price fell first (taking back what it forgave before), then takes what the lots live before its cut (`spendableUnits`) and that credit hold, and forgives the rest, never owed: shared across the periods whose price rose by their amounts and each period's across its positive lines, by largest remainder, as each receipt line's `forgiven` (`usage_receipts.forgiven` sums them). A period whose price fell takes back what its earlier receipts forgave, on its falling lines, before it credits anything. No posting reads the event store, so one runs while it is out of reach (OR-J48-S20). Forgiveness shows in the `usage_charged` measure as `charge: forgiven` by rate card with no invoker, moved out of `charged` at the posting's cut, read from the receipt lines (`MoneyService.UsageForgiveness`).
- An invoice in a merchant's unit draws its lots the same way, one `credit_spend` per lot naming the invoice, into the line's deferred revenue (`invoices`' `payFromBalance`, `SpendInto`): one revenue schedule per currency the lots were paid in, recognised over the line's service, ratably for an access window and at once otherwise.

### Lapse

A lot with `expires_at` is watched (`ledger.WatchLapsingCredit`), and its look is scheduled at the lapse. Spending stops at `expires_at` because the lot query excludes it; the balance holds the lot until its expiry posts (OR-J22-S50, OR-J04-S44). `credits.Lots.ExpireLapsedBy` then posts the remainder as `credit_expire` at (`credit_expire`, `credit_expiry`, lot id), after settling what accrued up to the lapse, and a paid lot's unreleased money as breakage, `deferred_revenue_credit` to `breakage_revenue` (`credit_breakage`). Callers include the balance watch (`MoneyService.WatchBalanceInTx`), the daily posting, an invoice paid from the balance, a reversal and a released refund (`MarkFailed`).

### Revocation

`MoneyService.RevokeCreditGrant` runs inside `destructive.RunLocal` (`destructive.RevokeCredit`) under the customer lock. It posts the customer's usage and accrued running charges (OR-J22-S02), refuses `credit_grant_held` while a refund of the lot's payment is pending (`units_refunding`) and `credit_grant_unavailable` when nothing remains, and `credits.Lots.Revoke` appends a `revoke` event and moves the remainder to `units_revoked` (`credit_revoke`, once per revocation) and a paid lot's unreleased money to `breakage_revenue` (`credit_forfeited`). Revoking a revoked lot replays. A revocation is a destructive command keyed by the grant and the lot's latest event, so a reinstated lot can be revoked again; an `Idempotency-Key` keys it by the caller's key instead, so a retry of an earlier revocation replays even after a reinstatement or a spend, found before the lot's remainder is read.

### Reinstatement

`MoneyService.ReinstateCreditGrant` runs under the customer lock: `credits.Lots.Reinstate` appends a `reinstate` event, which clears `revoked_at`, moves the lot's units still in `units_revoked` back to its balance (`credit_reinstate`, at (`credit_reinstate`, `grant_reinstate`, event id)) and their money from `breakage_revenue` back to deferral (`credit_forfeit_reversed`). Reinstating a live lot replays, as does a retry under its `Idempotency-Key` (`credit_grant_events.request_key`) even after a later revocation; a lapsed lot is refused (`credit_grant_unavailable`), since it would only expire again.

### Refunds and chargebacks

- A refund takes its units off the balance when it is requested. `ReserveRefund` checks with `purchasedcredits.Service.ValidateRefund` that the lot's share of the requested cash (proportional and cumulative with the refunds before it, rounded down) is on the lot and not owed to pending usage, records the pending refund, posts the customer's usage first, then `WithdrawRefund` posts the units to `units_refunded` (`credit_refund` at (`purchased_credit_refund`, `credit`, refund id)). A refund the provider refuses gives them back (`ReleaseRefund` from `MarkFailed`, `credit_refund_restore` at `credit_release`; OR-J22-S51). Their money stays deferred until the refund settles: `units_refunding` (`SyncCreditGrantRefunding`) counts them in the lot's worth (`books.CreditLot.Refunding`). A prune never takes a reversal of a lot's payment (`PaymentHasProtectedDependents`); refunding units that drift anyway are `ledger-audit` lot drift and the `derive.credit_refunding.mismatch` finding, which convergence repairs; it reads only lots with refunding units or whose payment has a pending refund (`credit_grants_refunding_idx`, `payments_pending_credit_refunds_idx`). In the customer's balance history (`ListBalanceTransactions`), a `credit_refund` withdrawal reads `refund_pending` until its refund settles, then `refund`, or `refund_failed` when refused, its units back as `refund_reversed` (`credit_refund_restore`); `revoke` and `reinstate` are staff revocations only.
- `ApplyReversal` decides each settled reversal's share once, the first time it sees it settled, records it in `credit_reversal_shares` (cash and units, zero included: a refund's or chargeback's own row, a recovery's one per chargeback it restored) and posts it at (`purchased_credit_refund`, leg, reversal id); a later pass never revisits it, and a lot's returned cash and a recovery's limits are read from the shares. The pass books each reversal it decides other than the caller's own, which the caller books where its effects put it. The cash share is the reversal up to the credit line's unreturned cash; the face share is proportional. It takes face from what its request withdrew, then from the lot (giving back first what pending refunds withdrew: a settled reversal comes before them, `supersedePending`, OR-J48-S09), then expired or revoked value, then debt the lot repaid (owed again), then units consumed, each straight to `units_refunded` (`credit_refund` from the balance, `credit_refund_funding` from the rest). In the general ledger (`books.Books.ReverseCredit`, in `paid_currency`, against the cash account of the lot's payment), as the units book allocated it: the lot's deferred money comes to the worth of its units left, the cash paying for it (`refund_deferred`, `chargeback_deferred`) and any of it beyond the cash going back against `refunds` or `chargebacks`, so a refund in pieces strands none; repaid owed is receivable again at face, the money at the lot's rate returning in cash and `promotional_credit` the rest (`usage_reopened`); the rest of the cash was recognised and is `refunds` or `chargebacks` (`refund_recognized`, `chargeback_recognized`). A recovery restores only what its own reversal took (one naming none, what the chargebacks took, oldest first by when they occurred), owed only while it is outstanding (what was repaid since returns to the balance), and `books.Books.RecoverCredit` mirrors the reversal (`dispute_won`); money restored to a revoked or lapsed lot is forfeited or broken at once. A lot in a merchant's own unit reverses its units in the unit and its money in `paid_currency`.
- A paid lot the general ledger never booked (its payment brought no cash to apply: `books.credit.unapplied`) has no deferred money: its reversal withdraws its units to `units_refunded` and records its share at once, posts no money of the lot's, and raises `books.credit.unbooked` (once per lot); `books.PostReversal` books the reversal's cash against the payment.
- A pack of a merchant's unit is reversed the same way: its cash share is the general ledger's, in its `paid_currency`, recorded once as each reversal applies, and its units go to `units_refunded` in the unit. The cash for units the lot spent comes, pro rata, from what its money still funds (`ListCreditLotRevenue`): each revenue schedule of a unit-priced line takes a price reduction with catch-up (recognised to `refunds` or `chargebacks`, the rest out of `deferred_revenue`), usage revenue the contra; a line not applied yet gives up its deferred revenue, and its application funds the schedule with what remains. A won dispute brings back, pro rata, what its reversal took (`books.Books.changeSchedule`), at the lot's reversal coordinate like its other legs: its contra part at `money_recognized_restore:<reversal>`, its deferred part at `money_recognized_deferred_restore:<reversal>`. A pack the general ledger never booked is reversed as any unbooked lot (above).

### Balance watch

`WatchChange` queues an `openrails.balance_look` job when a transfer crosses a
line, or on every change while the watch has `look_each_change` (a running charge
or a lapsing lot); `openrails.balance_watch` takes watches due by `next_check_at`.
`WatchBalanceInTx` expires lapsed lots, sends `balance.low` or `balance.depleted`
once per crossing (a funding re-arms them) and sets the next look: at a lot's
lapse or where a running charge's accrual crosses a line. It posts no running
charge: the daily posting does.
A refund, chargeback or revocation that leaves the balance depleted marks the
crossing's notices sent without sending them (`WatchChange`, `SilenceBalanceWatchNotices`).
It is judged against the floor the last look stored, so a chargeback or refund
that reopens debt (`owed_reopened`, the `repaid_owed` leg of
`credit_refund_funding`) is not silenced: when that debt uses up the credit
line, the next look announces it.
Only spending and lapses tell the customer the balance ran out; a look that
expired the lot that emptied it sends `balance_depleted` with reason `expired`.
A balance is low once what the customer may spend is at or under an amount:
its own (`customer_low_balance_lines`), else the merchant's
`balance_notices.low_amounts` for the currency, else none; it is depleted at
zero capacity. Usage and transfers are judged as they happen; time moves a
balance only through a running charge's fixed rate and a lot's lapse, so a look
is scheduled for the exact instant either crosses a line.

## Interfaces

- Go: `Client.CreateCreditGrants`, `ListCreditGrants`, `GetCreditGrant`,
  `RevokeCreditGrant`, `ReinstateCreditGrant`, `GetBalance`, `ListBalanceTransactions`; spending through
  a gate's usage (`SyncLeases`) and running charges.
- HTTP: `POST /v1/admin/credit-grants`, `POST /v1/admin/credit-grants/{id}/revoke`,
  `POST /v1/admin/credit-grants/{id}/reinstate` (`merchant:customer:manage`); `GET /v1/admin/credit-grants`,
  `GET /v1/admin/credit-grants/{id}` (`merchant:customer:read`); the customer's
  `GET /v1/me/balance/transactions` and `PUT /v1/me/low-balance-lines/{currency}`;
  `POST /v1/app/leases/sync` (`merchant:usage:manage`).
- Catalog: a product's `credit_grant` (`catalog.CreditGrantSpec`: `currency`,
  `amount` or `from_payment`, `expires_after_days`); a price's `customer_amount`.
- Events: `balance.low`, `balance.depleted` (`billing.BalanceHostEvent`).
- Error codes: `credit_grant_held`, `credit_grant_not_found`,
  `credit_grant_unavailable`, `currency_unsupported`, `idempotency_key_reused`,
  `invalid_param`.
- Docs: [credits](../client.md#credits), [recurring credits](../client.md#credits),
  [merchant operations](../client.md#credits).

## Guarantees

### 1. Exactly-once provider calls
Does not apply: credits call no provider; checkout charges a pack and refunds return it, and lots move inside their transactions.

### 2. Agreement with providers
A purchased lot exists only for a succeeded payment and in the same transaction
(`Fund` checks `payment.Status`). A provider reversal withdraws credit when it is
recorded (`syncPurchasedCreditRefund`), whichever path reported it. A pending
refund takes its credit off the balance when it is requested; a refused one gives
it back, straight to `units_expired` if the lot lapsed meanwhile.

Checks:
- `Fund` refuses a payment that is not `succeeded`, is a reversal, or differs in
  customer or currency.
- `recordRefund`, `CompleteRefundReservation`, `LinkRefundedPayment` and
  `PaymentService.Create` call `syncPurchasedCreditRefund` in their transaction.
- OR-J22-S07: a pending credit-line refund takes the whole lot off the balance when it is requested; OR-J22-S51: a refused one gives it back; OR-J22-S52: to `units_expired` once the lot lapsed.

### 3. Exact money
Lots and spends are `int64` native units in one currency; a purchased lot is in
its payment's currency, or in a merchant's unit its payment bought. Proportional face uses `big.Int` (`proportional`):
withdrawals take the difference of cumulative rounded shares, so partial
reversals sum to the full one. Deposits and purchases refuse to
overflow the balance; `fundCreditLine` refuses an amount that overflows with
quantity.

Checks:
- `ApplyReversal` takes `faceDelta` as the difference of `proportional` at the
  credit line's cumulative reversed cash after and before this reversal.
- `WithdrawRefund` and `ApplyReversal` take the same cumulative share; what a refund's request withdrew and its settlement disagree by is posted apart (`credit_settled`, `credit_excess`).
- OR-J22-S05: $20 a cycle delivers 25 credits; refunds return only cash.
- A lot's money is released by `books.CreditLot.Worth` of its units left, never by a per-spend rate, so rounding never drifts and the last unit releases the remainder (OR-J40-S10); `ledger-audit` lot drift compares `deferred_credited` and `deferred_debited` with the lot's legs.

### 4. Access control and tenant isolation
Staff routes need `merchant:customer:read` or `manage`, lease syncs `merchant:usage:manage`; a customer reads its own balance and sets its own line (`selfAccountCustomer`). `credit_grants` keys on `(merchant_id, id)` with composite foreign keys to `customers`, its balance account (customer and currency included), `payments`, `products` and `invoice_lines`; `ledger_transfers.credit_grant_id` and `credit_grant_events` reference it.

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
`credit_grants`, `credit_grant_events` and `usage_receipts` are permanent; `usage_totals` are rows deleted once their period's late usage window closed; `balance_watches`, `customer_low_balance_lines` and `usage_pending` are state.

Checks:
- Those classes are in `internal/retention/tables.go` and the table comments.
- `credit_grants` and `credit_grant_events` are in `NeverRollbackableTables`. A lot refuses DELETE and any change but its counters, `revoked_at` and `units_refunding`; only the transfer and event triggers move the counters and `revoked_at` (`guard_credit_grant_facts`), and events refuse UPDATE and DELETE.
- A restore inserts lots with their counters, money included; `check_billing_restore_credit_grants` proves them against the restored legs and events at commit.

### 7. Work scales with activity
Looks are queued by crossings; due watches are read by `balance_watches_next_check_at_idx` and daily usage postings by `usage_pending_due_at_idx`. A balance reads its account, its pending amounts and its running charges' rows. Lot reads are per customer and currency, and read a lot's columns, not its transfers: exhausted and revoked lots leave `credit_grants_live_idx`, and refunding units are read by `credit_grants_refunding_idx`.

Checks:
- `ListDueBalanceWatches` and `ListDueUsagePending` filter on the due instant and page in key order.

### 8. Abuse protection
Customers buy credit only through checkout (`POST /v1/me/invoices`: the `purchase` bucket and card-testing checks). `middleware.AdminOperationGrant` meters each invoker by item: `billing.MaxBatchItems` (100) a minute, one full batch, shared by `UpdateCustomer`, `AdjustCustomerLimit`, `CreateCreditGrants` and `CreateProductAccess`; going over locks the invoker out for an hour. An application acting as itself is not metered. Lease syncs are server-to-server; they, the customer's balance reads and its low-balance line are in no bucket.

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
- `RevokeCreditGrant` builds its command with `destructive.CommandFromContext` (`CommandFromContextExplicit` under an `Idempotency-Key`) and
  runs inside `destructive.RunLocal`.
- OR-J34-S01 to S04 hold and clear revocations at the ceilings.

## Tests

- J22: OR-J22-S01 pending usage posted at the day's end (`TestCredits`); OR-J22-S02
  usage before a revocation (`TestCreditRevocation`); OR-J22-S03, OR-J22-S06,
  OR-J22-S50 usage before a lapse, the lapse and a view after it (`TestCreditExpiry`);
  OR-J22-S04 atomic batches and replay (`TestCreditAtomicity`,
  `TestLedgerMaintenance`); OR-J22-S05 recurring credits (`TestRecurringCredits`);
  OR-J22-S07 to S09 and S51 refunds, chargebacks and won disputes of a mixed
  invoice, S52 a refund refused after the lapse, S55 a refused arrears refund
  still lapses, S56 a settled refund's lapsed excess retires, S57 an arrears
  refund's restored excess still lapses (`TestMixedInvoiceCreditRefunds`);
  OR-J22-S53 a reinstated lapsing lot expires (`TestCreditExpiry`);
  OR-J22-S54 an already-expired or overflowing grant is refused (`TestCredits`).
- J48 (merchant units): OR-J48-S01 packs at their own rates, S02 the same units bought in EUR, S03 a plan allowance and a gift, S04 a unit-priced product paid from the balance, S05 usage by rate card soonest-expiring lot first and never owed, S06 expiry, S07 refund by lot, S08 a gate decides on the unit balance and an overrun is forgiven usage, S09 a pack's reversals in the order they settle, S10 a price fall takes back forgiveness first, S20 a posting forgives with the event store out of reach, S11 a lease sync takes usage in a declared unit only, S12 the first balance in a unit takes the units lock, S13 a unit-priced invoice's revenue scheduled per paid currency, applied once and moved by an archive, S14 a pack's chargeback reducing the schedule its spends funded and its won dispute restoring it, S15 a line paid with given units booking nothing, S16 a later release adding to its schedule's funding and the audit failing a release with no schedule, S17 a refund at the provider, S18 a chargeback split between usage and a schedule, S19 a chargeback of one currency's pack (`TestMerchantUnits`).
- OR-J22-S10 a lot's view equals its ledger counters through spend, expiry, revocation, refund and restore, and `ledger-audit` names lot drift; OR-J22-S11 a refunded pack raises no refunded-source grant finding; OR-J22-S13 two lapsed lots expire only what each still holds; OR-J26-S62 a usage invoice's change is a bought lot; OR-J40-S04 a deposit that did not post is pending and not spendable until convergence posts it (`TestCreditLotCounters`).
- OR-J40-S10 a $50 pack of 55 credits releases exactly $10 for 11 credits and the remainder with its last; OR-J40-S11 expiry, revocation and reinstatement; OR-J40-S12 a gift spend, and owed repaid from a gift and from a bonus pack; OR-J40-S13 refunds of spent and unspent credit and a won dispute (`TestCreditBooks`).
- Also: OR-J24-S22 and OR-J04-S44 (a lapsed lot is never drawn); OR-J23-S14
  (an open ticket holds its windows, not the balance); OR-J24-S71 (usage past
  the balance posts owed); OR-J25-S37, OR-J25-S40 to S42 (balance looks); OR-J25-S47 to S49 (low amounts, exact running-charge crossings);
  OR-J34-S01 to S04 (revocation ceilings).
- No Go package test covers this system; `sqlc vet` prepares its queries.

Gaps:
- No scenario buys a customer-chosen deposit (`customer_amount`, `from_payment`).
- Credit refunds are tested only on Stripe; no NMI-funded lot is reversed.
