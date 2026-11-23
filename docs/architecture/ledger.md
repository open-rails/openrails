# Ledger

The ledger is the double-entry record of a customer's money at a merchant:
prepaid balances, credit lots drawn by usage, usage owed in arrears and how that
debt was settled. Each (merchant, currency) pair is its own ledger, every amount
is an integer at the currency's registered scale, and no transfer crosses
currencies. Prepaid credits, metering, usage orders and purchased-credit
reversals post to it; card charges for products and their refunds are
`payments` rows, not transfers.

## Concepts

- **Native units**: an amount at the currency's scale, `Units.Decimals` in
  `internal/currency/registry.go`: 10^6 per unit for two-decimal fiat (micros for
  USD), 10^4 per yen for JPY and KRW, on-chain precision for SOL and stablecoins.
- **Minor units**: what a card rail settles in (`MinorDecimals`: cents, whole yen).
  `NativeShift` is the difference.
- **Account** (`ledger_accounts`): one role in one ledger. System accounts
  (`customer_id` NULL): `platform_revenue`, `processor_clearing`,
  `expired_credits`, `revoked_credits`, `promotional_funding`,
  `credit_refund_clearing`, `credit_refund_loss`. Per customer: `customer_balance`
  and `arrears_liability`.
- **Transfer** (`ledger_transfers`): an immutable movement of a positive amount
  from a debit account to a credit account; `transfer_type` is a closed vocabulary
  (`ledger.TransferType`).
- **Coordinate** (`ledger.Coord`): operation, source and source id. With customer,
  currency, type and lot it is the transfer's once-only identity.
- **Lot**: a credit grant (`grants`, kind `credit`); a transfer that funds or draws
  it carries its `grant_id`.
- **Balance**: an account's `credits_posted - debits_posted`, kept by trigger.
- **Held / available**: holds, pending refunds, unposted usage and running-charge
  accrual live outside the ledger (`GetFinancialHeldAmount`); available is balance
  less held.
- **Owed**: the negated balance of the customer's `arrears_liability`
  (`Ledger.OutstandingOwed`).

## How it works

Every movement funnels through `Ledger.ApplyIdempotent`
(`internal/modules/money/ledger/ledger.go`):

1. `Coord.Validate` refuses a blank part or one over `MaxCoordBytes` (512).
2. `InsertLedgerTransfer` inserts `ON CONFLICT DO NOTHING` on
   `ledger_transfers_operation_once_key`. No row back means the coordinate
   already committed: the call returns that row with `applied` false and moves
   nothing.
3. The `AFTER INSERT` trigger `trg_ledger_transfers_apply_counters` locks both
   accounts in id order, refuses a cross-currency transfer, enforces the sign flag
   (`debits_must_not_exceed_credits` on `customer_balance`, relaxed per transfer by
   `allow_debit_negative_up_to`) and bumps the counters. `ledger_insufficient_funds`
   surfaces as `ledger.ErrInsufficientFunds`.
4. A transfer that names a customer calls `WatchChange` in the same transaction,
   which queues a balance look when the change crosses a line or the watch looks
   at every change (see [prepaid credits](prepaid-credits.md)).

Facts never change. `immutable_ledger_transfers` refuses UPDATE, DELETE and
TRUNCATE; `guard_ledger_account_facts` lets only the nested counter trigger touch
`credits_posted` and `debits_posted`. Accounts are created on first write
(`EnsureSystemAccount`, `EnsureCustomerBalance`, `EnsureCustomerArrears`); reads
never create one, so an unknown customer reads zero. Writers serialize on the
customer row (`LockCustomerForSpend`, taken by `MoneyService.lockBalance` and
`grants.Ledger.LockCustomer`) before any read-then-write of a balance.

| Movement | Debit → credit | Type | Posted by |
|---|---|---|---|
| Lot funded | `processor_clearing` → `customer_balance` | `deposit` | `grants.Ledger.MaterializeGrant` |
| Purchased lot worth more than paid | `promotional_funding` → `customer_balance` | `deposit_bonus` | `Ledger.PurchasedDeposit` |
| Paid more than the lot's face | `processor_clearing` → `platform_revenue` | `credit_purchase_revenue` | `Ledger.PurchasedDeposit` |
| Spend, one leg per lot | `customer_balance` → `platform_revenue` | `credit_spend` | `grants.Ledger.CreditSpend` |
| Lot lapsed | `customer_balance` → `expired_credits` | `credit_expire` | `grants.Ledger.ExpireLapsedBy` |
| Lot revoked | `customer_balance` → `revoked_credits` | `credit_revoke` | `clawbackRevokedCredit` |
| Spend past the balance | `arrears_liability` → `platform_revenue` | `owed_accrual` | `Ledger.AccrueOwedIdempotent` |
| Owed paid by a rail or remittance | `processor_clearing` → `arrears_liability` | `owed_payment` | `Ledger.PayOwed` |
| Owed paid from a new lot | `customer_balance` → `arrears_liability` | `owed_repayment` | `owed.Repay` |
| Usage order voided | `platform_revenue` → `arrears_liability` | `owed_writeoff` | `Ledger.WriteOffOwed` |
| Purchased credit reversed or restored | through `credit_refund_clearing` | `credit_refund*` | `purchasedcredits.Service.ApplyReversal` |

Usage reaches the ledger as one posting per customer, currency and UTC day
(`usage_accruals`, `MoneyService.postUsageAccrualsTx`) at
(`usage_accrual`, `usage_accrual:<id>`, posting number): balance first, the rest
owed. A payment that settles owed points at its transfer
(`payments.ledger_transfer_id`).

No FX: a transfer's currency must equal both accounts'. Rates
(`internal/integrations/fx`) only price a Solana quote and convert a spend cap at
admission (`fx.ConvertAmount`); no ledger, grants or money code imports them.

Integrity is checked on demand. `ledger.CheckIntegrity` runs conservation (each
currency's balances sum to zero) and counter drift (counters against the transfer
log) for one merchant; `openrails ledger-audit` runs it per merchant and exits
non-zero on a finding. A merchant restore skips the counter trigger
(`billing_restore_active`), and `check_billing_restore_ledger` verifies counters,
conservation, currencies and customer agreement before the restore commits.

## Interfaces

- Go: `Client.GetBalance` (`billing.Balance`: balance, held, accrued, available,
  owed) and `Client.ListBalanceTransactions` (`billing.BalanceTransaction`; a usage
  deduction carries `Usage`). `billing.Currencies` and `billing.LookupCurrency`
  read the registry.
- HTTP: `GET /v1/admin/customers/{customer_id}/balance` and
  `GET /v1/admin/customers/{customer_id}/balance/transactions`
  (`merchant:customer:read`); the customer's `GET /v1/me` and
  `GET /v1/me/balance/transactions`; `GET /v1/config` lists `currencies`.
- Error codes: `currency_unsupported`, `invalid_cursor`, `invalid_customer_id`.
- CLI: `openrails ledger-audit [--merchant=…] [--format=table|json]`.
- Docs: [glossary](../glossary.md), [prepaid credits](../prepaid-credits.md),
  [usage billing](../usage-billing.md).

## Guarantees

### 1. Exactly-once provider calls
Does not apply: the ledger calls no provider; it records what other systems commit, inside their transaction.

### 2. Agreement with providers
Does not apply: the ledger holds no provider state; the systems that talk to providers post here only after they have reconciled.

### 3. Exact money
Amounts are `int64` native units with `ledger_transfers_amount_positive_check`.
The trigger refuses a transfer whose currency differs from either account's.
The registry is code, not a table (`internal/currency`); the `currency` column
CHECK only checks shape, so writers validate the code first. Provider conversions
live in `internal/shared/moneyutil`: `NativeToRailMinor` rounds a charge up,
`NativeToRailMinorExact` refuses a sub-minor remainder, `RailMinorToNative` and
`DecimalToRailMinor` read provider amounts exactly. A deposit refuses to overflow
the balance.

Checks:
- `ledger_transfers_apply_counters` raises when `debit.currency` or
  `credit.currency` differs from `NEW.currency` (`0001_schema.up.sql`).
- Each writer validates the currency before posting: `CurrencyDecimals` in
  `depositTx`, `moneyutil.ValidateCurrency` in `purchasedcredits.Service.Fund`.
- `ListLedgerConservationBreaches` sums `credits_posted - debits_posted` per
  currency; `openrails ledger-audit` fails on a non-zero sum.
- OR-J05-S01 pins 9.99 USD × 3 at 29970000 native and 2997 minor units.

### 4. Access control and tenant isolation
Both tables key on `PRIMARY KEY (merchant_id, id)`, and a transfer's account
foreign keys include `merchant_id`, so a transfer cannot reach another merchant's
account. `ledger.New` binds one merchant and every query passes it. Staff reads
need `merchant:customer:read`; the customer routes read `selfAccountPayer`, never a
path id.

Checks:
- `ledger_transfers_debit_account_id_fkey` and
  `ledger_transfers_credit_account_id_fkey` reference `(merchant_id, id)`.
- Every query in `internal/db/queries/ledger.sql` filters `merchant_id`.
- The counter trigger does not compare `ledger_transfers.customer_id` with the
  accounts' `customer_id` (only `check_billing_restore_ledger` does): confirm each
  writer sets `Transfer.Customer` from the customer whose accounts it names.
- OR-J22-S01 reads the customer through another merchant and gets no transactions.

### 5. Stable contracts
`billing.Balance`, `billing.BalanceTransaction`, `billing.BalanceTransactionType`
and the routes above are in `api/go.txt` and `api/openapi.json`; both tables are
in `api/schema.txt`. Stored names never change: the usage order operations keep
their older names (`invoice_payment`, `manual_invoice_payment`, `invoice_void`).
The in-process `Client` calls the same routes over an in-process transport
(`engine.NewClient` in `embedded.go`), so it answers as the remote one does.

Checks:
- `go run ./scripts/contracts` shows no removed line for these types and routes.
- A new transfer type adds a value to `ledger_transfers_type_check` in a new
  migration, to `ledger.AllTransferTypes`, and a mapping in
  `internal/modules/money/genmap.go`.
- No rename of an `Operation` or `TransferType` value already stored.

### 6. Migrations and retention
Both tables are in the baseline; a change is a new numbered migration. Both are
`Permanent` (`internal/retention/tables.go`, stated in each table comment), are
not partitioned, move with the merchant archive, and are never deleted.

Checks:
- `ledger_accounts` and `ledger_transfers` are `Permanent` in
  `internal/retention/tables.go`.
- Both are in `ownedTables` (`internal/merchantarchive/owned_tables.go`) and in
  the restore guard's table list (`0003_usage_limits.up.sql`).
- No migration after `0001_schema.up.sql` alters either table.

### 7. Work scales with activity
A balance or owed read is one counter row (`LedgerAccountBalance`); an insert
locks two accounts. Lot remainders read by `grant_id`
(`ledger_transfers_grant_id_idx`). No job scans the ledger: daily usage postings
are found by `usage_accruals_due_idx`, balance looks are queued by crossings.
`ledger-audit` reads a merchant's whole log, on demand only.

Checks:
- `LedgerAccountBalance` reads `credits_posted - debits_posted`, not a sum.
- No River worker reads `ledger_transfers` without a customer, lot or coordinate
  predicate.
- `ListLedgerCounterDrifts` is called only from `ledger.CheckCounterDrift`, which
  only `openrails ledger-audit` runs.

### 8. Abuse protection
The ledger has no write route; it moves only through the systems above, which
carry their own limits. Its customer reads (`GET /v1/me`,
`GET /v1/me/balance/transactions`) are in no rate-limit bucket: [limits](../limits.md)
leaves a per-address ceiling to the proxy. Pages are capped by `pagination.Limit`.

Checks:
- No route in `internal/http/routes/credits.go` writes a transfer directly.
- The customer reads take the payer from the signed-in identity
  (`selfAccountPayer`) and a bounded page.

### 9. Destructive-action safeguards
Nothing deletes or rewrites a transfer: a correction is a compensating transfer.
Undo runs refuse both tables (`reconcile.NeverRollbackableTables`) and a merchant
purge leaves them (`purgeMerchantRows` has no case for them). Staff commands that
move money are admitted where they start: `destructive.RevokeCredit`,
`destructive.Refund`, `destructive.WaiveOrder`. Each transfer records its
coordinate and, where known, `invoker_id`, `resource` and `order_id`.

Checks:
- `immutable_ledger_transfers`, `immutable_ledger_transfers_truncate`,
  `guard_ledger_account_facts` and `immutable_ledger_accounts_truncate` exist.
- `NeverRollbackableTables` lists `ledger_transfers` and `ledger_accounts`.
- `WriteOffOwed` is reached only inside `destructive.RunLocal`
  (`internal/modules/money/usage_order.go`).

## Tests

Journeys J05 (money arithmetic) and J22 (credits), plus restore in J31.

- OR-J05-S01, OR-J05-S02: exact price arithmetic; rounding, overflow, wrong
  currency and bad quantity refused (`TestPublicCatalog`).
- OR-J22-S01: grant, hold, capture and release conserve native units; the day's
  deduction posts once; another merchant reads nothing (`TestCredits`).
- OR-J22-S02: concurrent capture and revocation cannot go negative
  (`TestAdmissionBoundaries`).
- OR-J22-S04: a refused batch rolls back ledger and events
  (`TestCreditAtomicity`); concurrent replay leaves one transfer and
  `ledger-audit` catches injected counter corruption (`TestLedgerMaintenance`).
- OR-J31-S01, OR-J31-S02: a restore preserves the ledger, and a corrupt one is
  refused at commit (`TestBillingArchive`, `TestLedgerMaintenance`).
- OR-J23-S05: a host transaction rollback leaves no charge (`TestHostTransactions`).
- No Go package test covers the ledger; `sqlc vet` prepares its queries.

Gaps:
- No test inserts a cross-currency transfer or an UPDATE/DELETE on
  `ledger_transfers`; the triggers are exercised only indirectly.
- Nothing checks `ledger.AllTransferTypes` against `ledger_transfers_type_check`;
  its comment names a test that does not exist.
- No test covers a transfer whose `customer_id` disagrees with its accounts'.
