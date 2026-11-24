# Books

OpenRails keeps two double-entry books in the same tables, `ledger_accounts` and `ledger_transfers`, told apart by `book`. The **units book** is what customers hold and owe, at face: prepaid balances, credit lots drawn by usage, usage owed in arrears and how that debt was settled. The **general ledger** holds money only, in registry currencies; its accounts and transfer types are declared, and its writer is `books.Books.Post`. Each (merchant, book, currency) is its own ledger, every amount is an integer at the currency's registered scale, and no transfer crosses books, currencies or customers. Card charges for products and their refunds are `payments` rows, not transfers.

## Concepts

- **Native units**: an amount at the currency's scale, `Units.Decimals` in `internal/currency/registry.go`: 10^6 per unit for two-decimal fiat (micros for USD), 10^4 per yen for JPY and KRW, on-chain precision for SOL and stablecoins. **Minor units** are what a card rail settles in (`MinorDecimals`); `NativeShift` is the difference.
- **Chart** (`books.Chart`): every account type with its book, class, scope (system, customer or PSP) and sign rules. `ledger_accounts_type_check` and `ledger_accounts_scope_check` hold the same chart.
- **Class**: sets an account's normal side. Units book: `held`, `owed`, `issued`, `retired`, `clearing`. General ledger: `asset`, `contra_asset`, `liability`, `revenue`, `contra_revenue`, `expense`, `equity`. Every stored balance is `credits_posted - debits_posted`.
- **Transfer** (`ledger_transfers`): an immutable movement of a positive amount from a debit account to a credit account in one book. `transfer_type` is a closed vocabulary per book: the units book's `ledger.AllTransferTypes`, the general ledger's `books.GeneralTransferTypes`.
- **Dates**: `effective_at` is when a transfer takes effect. A units-book transfer takes effect when it posts; a general-ledger posting names its own instant. Nothing in the books stores a zone: a reader groups `ledger_daily`'s 15-minute UTC buckets into days and months of the merchant's `accounting.timezone`, passing the zone as a query parameter (every zone's offset is a whole number of quarter hours).
- **Dimensions**: `customer_id`, `order_id`, `order_line_id` and `product_id` attribute a transfer, with no foreign keys; `credit_grant_id` names its lot.
- **Coordinate** (`books.Coord`): operation, source and source id. With customer, currency, type, lot and order line it is the transfer's once-only identity.
- **Lot**: a credit grant (`credit_grants`); a transfer that funds or draws it names it in `credit_grant_id` (a foreign key), and the legs on its balance account keep its `units_credited` and `units_debited`.
- **Rollups**: `ledger_type_totals` sums the counters of every account of a type; `ledger_daily` sums the credits and debits of each 15-minute UTC bucket of `effective_at` per account type, transfer type, PSP and product. A value at a past instant is the total less the flows after it.
- **Held / available**: holds, pending refunds and running-charge accrual live outside the books (`GetFinancialHeldAmount`), unposted usage in the event store (`MoneyService.unpostedUsage`); available is balance less held.
- **Owed**: the negated balance of the customer's `customer_owed` (`Ledger.OutstandingOwed`).

### Units book

| Account | Was | Scope | Class | Sign rule |
|---|---|---|---|---|
| `customer_units` | `customer_balance` | customer | held | `debits_must_not_exceed_credits` |
| `customer_owed` | `arrears_liability` | customer | owed | none |
| `units_sold` | `processor_clearing` | system | issued | none |
| `units_granted` | `promotional_funding` | system | issued | none |
| `units_redeemed` | `platform_revenue` | system | retired | none |
| `units_expired`, `units_revoked` | `expired_credits`, `revoked_credits` | system | retired | none |
| `units_refunded` | (new) | system | retired | none |
| `units_refund_clearing`, `units_refund_loss` | `credit_refund_clearing`, `credit_refund_loss` | system | clearing | none |

The books migration renamed the accounts in place; no transfer moved, and every transfer type keeps its stored name.

| Movement | Debit → credit | Type | Posted by |
|---|---|---|---|
| Lot funded | `units_sold` → `customer_units` | `deposit` | `credits.Lots.Materialize` |
| Purchased lot worth more than paid | `units_granted` → `customer_units` | `deposit_bonus` | `Ledger.PurchasedDeposit` |
| Paid more than the lot's face | `units_sold` → `units_redeemed` | `credit_purchase_revenue` | `Ledger.PurchasedDeposit` |
| Spend, one leg per lot | `customer_units` → `units_redeemed` | `credit_spend` | `credits.Lots.Spend` |
| Lot lapsed | `customer_units` → `units_expired` | `credit_expire` | `credits.Lots.ExpireLapsedBy` |
| Lot revoked | `customer_units` → `units_revoked` | `credit_revoke` | `credits.Lots.Revoke` |
| Spend past the balance | `customer_owed` → `units_redeemed` | `owed_accrual` | `Ledger.AccrueOwedIdempotent` |
| Owed paid by a rail or remittance | `units_sold` → `customer_owed` | `owed_payment` | `Ledger.PayOwed` |
| Owed paid from a new lot | `customer_units` → `customer_owed` | `owed_repayment` | `owed.Repay` |
| Usage order voided | `units_redeemed` → `customer_owed` | `owed_writeoff` | `Ledger.WriteOffOwed` |
| Purchased credit reversed or restored | through `units_refund_clearing` | `credit_refund*` | `purchasedcredits.Service.ApplyReversal` |

Usage reaches the units book through postings (`usage_postings`, `MoneyService.postUsageTx`): at each month's end and before any other movement of that balance, per pricing period the price of the period's usage in the event store less what earlier postings took, at (`usage_posting`, `usage_posting:<id>`, `1`): balance first, the rest owed. A period whose volume price fell is credited back to a new lot (`usage_reversed`, `units_redeemed` → `customer_units`, `Ledger.ReverseUsage`). A payment that settles owed points at its transfer (`payments.ledger_transfer_id`).

### General ledger

| Account | Scope | Class | Sign rule |
|---|---|---|---|
| `cash_clearing` | PSP (`psp_id`) | asset | none |
| `external_receipts` | system | asset | none |
| `usage_receivable` | customer | asset | `credits_must_not_exceed_debits` |
| `allowance_doubtful` | system | contra_asset | `debits_must_not_exceed_credits` |
| `unapplied_cash`, `deferred_revenue` | system | liability | `debits_must_not_exceed_credits` |
| `deferred_revenue_credit` | customer | liability | `debits_must_not_exceed_credits` |
| `tax_payable` | system | liability | none |
| `subscription_revenue`, `one_time_revenue`, `usage_revenue`, `breakage_revenue` | system | revenue | none |
| `refunds`, `chargebacks`, `revenue_adjustments`, `promotional_credit` | system | contra_revenue | none |
| `processor_fees`, `chargeback_fees`, `bad_debt` | system | expense | none |
| `opening_balance_equity` | system | equity | none |

Its transfer types are `cash_received`, `cash_applied`, `cash_returned`, `revenue_adjusted`, `revenue_recognized`, `refund_deferred`, `refund_recognized`, `chargeback_deferred`, `chargeback_recognized`, `dispute_won`, `credit_released`, `credit_breakage`, `credit_forfeited`, `credit_forfeit_reversed`, `promotional_credit_applied`, `usage_accrued`, `usage_collected`, `usage_repaid`, `usage_waived`, `usage_reopened`, `bad_debt`, `bad_debt_recovery`, `processor_fee`, `chargeback_fee` and `opening_balance`. No writer posts to the general ledger yet; nothing reads it but `ledger-audit` and the restore check.

## How it works

The units book's movements funnel through `Ledger.ApplyIdempotent` (`internal/modules/money/ledger/ledger.go`), the general ledger's through `books.Books.Post`; each inserts once (`InsertLedgerTransfer`, `InsertGeneralLedgerTransfer`):

1. `Coord.Validate` refuses a blank part, an operation that is only a prefix, or one over `MaxCoordBytes` (512). `Post` also refuses a type or account outside the general ledger, a customer leg naming another customer, and a currency outside the registry (`books.CheckCurrency`, `books.ErrNotMoney`): a merchant's own unit never enters it.
2. The insert runs `ON CONFLICT DO NOTHING` on `ledger_transfers_operation_once_key`. No row back means the coordinate already committed: the call returns that row with `applied` false and moves nothing. `Post` also compares it with the posting and refuses a different one (`books.ErrCoordinateReused`).
3. `trg_ledger_transfers_book_rules` (before insert) refuses a transfer whose accounts are in another book (`ledger_cross_book`) or currency (`ledger_cross_currency`), or that moves a customer's account in a transfer not attributed to that customer (`ledger_customer_mismatch`).
4. `trg_ledger_transfers_apply_counters` locks both accounts in id order, enforces the sign rules (relaxed for a units-book debit by `allow_debit_negative_up_to`; never for the general ledger) and bumps the counters. `ledger_insufficient_funds` surfaces as `ledger.ErrInsufficientFunds`, a general-ledger breach as `books.ErrSignRule`.
5. `trg_ledger_transfers_rollups` queues the transfer in `ledger_rollup_pending`. At commit, after every account the transaction moves is locked, the deferred `ledger_rollups_settle` folds the queued transfers into `ledger_type_totals` and `ledger_daily` (`settle_ledger_rollups`), locking rollup rows in key order, so no two writers wait on each other's rollups mid-transaction.
6. A units-book transfer that names a customer calls `WatchChange` in the same transaction, which queues a balance look when the change crosses a line or the watch looks at every change (see [prepaid credits](prepaid-credits.md)).

Facts never change. `immutable_ledger_transfers` refuses UPDATE, DELETE and TRUNCATE; `guard_ledger_account_facts` lets only the nested counter trigger touch `credits_posted` and `debits_posted`, and `guard_ledger_daily` and `guard_ledger_type_totals` only the commit-time settlement write the rollups. Accounts are created on first write through the chart (`books.EnsureAccount`; `EnsureSystemAccount`, `EnsureCustomerBalance`, `EnsureCustomerArrears` for the units book); reads never create one, so an unknown customer reads zero. Writers serialize on the customer row (`LockCustomerForSpend`, taken by `MoneyService.lockBalance` and `credits.Lots.LockCustomer`) before any read-then-write of a balance.

The units book's readers read it alone: `ListLedgerTransfersByCustomer` and `ListLedgerTransfersByIDs` filter `book = 'units'`, and the metrics balance family's `outstanding_credit_liability` and `outstanding_owed` read `customer_units` and `customer_owed` of the units book.

No FX: a transfer's currency must equal both accounts'. Rates (`internal/integrations/fx`) only price a Solana quote and convert a spend cap at admission (`fx.ConvertAmount`); no books, grants or money code imports them.

Integrity is checked on demand. `ledger.CheckIntegrity` runs conservation (each book's balances sum to zero per currency), counter drift (account counters against the transfer log), lot drift (each credit lot's `units_credited` and `units_debited` against the legs on its balance account, `CheckLotDrift`) and rollup drift (`ledger_type_totals` and `ledger_daily` recomputed from the transfer log) for one merchant; `openrails ledger-audit` runs it per merchant and exits non-zero on a finding. The books migration filled the rollups from the transfers already posted. A merchant archive carries no rollups: during a restore the counter triggers are skipped (`billing_restore_active`) while the restored transfers queue their rollups, `finish_billing_restore` settles them, and `check_billing_restore_ledger` verifies counters, conservation per book and currency, each transfer's book, currency and customer against its accounts, and both rollups, and `check_billing_restore_credit_grants` the lots' counters, before the restore commits (and at export).

## Interfaces

- Go: `Client.GetBalance` (`billing.Balance`: balance, held, accrued, available, owed) and `Client.ListBalanceTransactions` (`billing.BalanceTransaction`; a usage deduction carries `Usage`) read the units book. `billing.Currencies` and `billing.LookupCurrency` read the registry. The general ledger has no public reader.
- HTTP: `GET /v1/admin/customers/{customer_id}/balance` and `GET /v1/admin/customers/{customer_id}/balance/transactions` (`merchant:customer:read`); the customer's `GET /v1/me` and `GET /v1/me/balance/transactions`; `GET /v1/config` lists `currencies`.
- Configuration: `accounting.timezone` ([merchant configuration](merchant-configuration.md)).
- Error codes: `currency_unsupported`, `invalid_cursor`, `invalid_customer_id`.
- CLI: `openrails ledger-audit [--merchant=…] [--format=table|json]`.
- Docs: [glossary](../guide.md), [prepaid credits](../client.md#credits), [usage billing](../client.md#usage-and-running-charges).

## Guarantees

### 1. Exactly-once provider calls
Does not apply: the books call no provider; they record what other systems commit, inside their transaction.

### 2. Agreement with providers
Does not apply: the books hold no provider state; the systems that talk to providers post here only after they have reconciled.

### 3. Exact money
Amounts are `int64` native units with `ledger_transfers_amount_positive_check`. The trigger refuses a transfer whose currency or book differs from either account's. The registry is code, not a table (`internal/currency`); the `currency` column CHECK only checks shape, so writers validate the code first, and the general ledger takes registry codes only. Provider conversions live in `internal/shared/moneyutil`: `NativeToRailMinor` rounds a charge up, `NativeToRailMinorExact` refuses a sub-minor remainder, `RailMinorToNative` and `DecimalToRailMinor` read provider amounts exactly. A deposit refuses to overflow the balance.

Checks:
- `ledger_transfers_book_rules` raises `ledger_cross_book` and `ledger_cross_currency` (`internal/migrate/postgres`).
- Each writer validates the currency before posting: `CurrencyDecimals` in `depositTx`, `moneyutil.ValidateCurrency` in `purchasedcredits.Service.Fund`, `books.CheckCurrency` in `Post`.
- `ListLedgerConservationBreaches` sums `credits_posted - debits_posted` per book and currency; `openrails ledger-audit` fails on a non-zero sum.
- OR-J05-S01 pins 9.99 USD × 3 at 29970000 native and 2997 minor units; OR-J40-S01 refuses a merchant unit in the general ledger.

### 4. Access control and tenant isolation
Both tables key on `PRIMARY KEY (merchant_id, id)`, and a transfer's account foreign keys include `merchant_id`, so a transfer cannot reach another merchant's account. `ledger.New` and `books.New` bind one merchant and every query passes it. A transfer that moves a customer's account names that customer, enforced by the trigger. Staff reads need `merchant:customer:read`; the customer routes read `selfAccountPayer`, never a path id.

Checks:
- `ledger_transfers_debit_account_id_fkey` and `ledger_transfers_credit_account_id_fkey` reference `(merchant_id, id)`.
- Every query on `ledger_accounts`, `ledger_transfers`, `ledger_daily` or `ledger_type_totals` in `internal/db/queries/` filters `merchant_id`.
- OR-J22-S01 reads the customer through another merchant and gets no transactions; OR-J40-S02 refuses a transfer that moves another customer's account.

### 5. Contracts match the code
`billing.Balance`, `billing.BalanceTransaction`, `billing.BalanceTransactionType` and the routes above are in `api/go.txt` and `api/openapi.json`; the tables are in `api/schema.txt`. Stored transfer names never change: the usage order operations keep their older names (`invoice_payment`, `manual_invoice_payment`, `invoice_void`). The in-process `Client` calls the same routes over an in-process transport (`engine.NewClient` in `embedded.go`), so it answers as the remote one does.

Checks:
- `go run ./scripts/contracts` shows no removed line for these types and routes.
- Both books' vocabularies are declared in `ledger_transfers_type_check` and `ledger_accounts_type_check`; OR-J40-S01 holds `ledger.AllTransferTypes`, `books.GeneralTransferTypes` and `books.Chart` to them.
- A units-book transfer type maps to a `billing.BalanceTransactionType` in `internal/modules/money/genmap.go`.

### 6. Data lifecycle
`ledger_accounts`, `ledger_transfers` and `ledger_daily` are `Permanent`, `ledger_type_totals` `State` (`internal/retention/tables.go`, stated in each table comment). None is partitioned or deleted. Accounts and transfers move with the merchant archive; the rollups are rebuilt from them.

Checks:
- All four, and `ledger_rollup_pending` (`State`, empty at commit), are in `ownedTables` (`internal/merchantarchive/owned_tables.go`) and in the restore guard's table list; the rollups are excluded from the archive as derived and the queue as scratch (`internal/merchantarchive/checks.go`).
- An archive from before the books restores as the units book (`normalizePreBooks`).

### 7. Work scales with activity
A balance or owed read is one counter row (`LedgerAccountBalance`); an account type's total is one `ledger_type_totals` row and a period's flows are its buckets in `ledger_daily`. An insert locks two accounts and, for a lot's leg, its lot; its rollup rows are locked at commit. A lot's remainder is its counters. No job scans the books: the month-end usage close finds its customers by `usage_unposted_due_at_idx`, balance looks are queued by crossings. `ledger-audit` reads a merchant's whole log, on demand only.

Checks:
- `LedgerAccountBalance` reads `credits_posted - debits_posted`, not a sum.
- No River worker reads `ledger_transfers` without a customer, lot or coordinate predicate.
- `ListLedgerCounterDrifts`, `ListLedgerTypeTotalDrifts` and `ListLedgerDailyDrifts` are called only from `ledger.CheckIntegrity`, which only `openrails ledger-audit` runs.

### 8. Abuse protection
The books have no write route; they move only through the systems above, which carry their own limits. The customer reads (`GET /v1/me`, `GET /v1/me/balance/transactions`) are in no rate-limit bucket: [limits](../standalone.md#abuse-limits) leaves a per-address ceiling to the proxy. Pages are capped by `pagination.Limit`.

Checks:
- No package under `internal/http` imports `internal/modules/money/ledger` or `internal/modules/money/books`.
- The customer reads take the payer from the signed-in identity (`selfAccountPayer`) and a bounded page.

### 9. Destructive-action safeguards
Nothing deletes or rewrites a transfer: a correction is a compensating transfer. Undo runs refuse the four tables (`reconcile.NeverRollbackableTables`) and a merchant purge leaves them (`purgeMerchantRows` has no case for them). Staff commands that move money are admitted where they start: `destructive.RevokeCredit`, `destructive.Refund`, `destructive.WaiveOrder`. Each transfer records its coordinate and, where known, `invoker_id`, `resource` and `order_id`.

Checks:
- `immutable_ledger_transfers`, `immutable_ledger_transfers_truncate`, `guard_ledger_account_facts`, `immutable_ledger_accounts_truncate`, `guard_ledger_daily`, `guard_ledger_type_totals` and their truncate triggers exist.
- `NeverRollbackableTables` lists `ledger_transfers`, `ledger_accounts`, `ledger_daily` and `ledger_type_totals`.
- `WriteOffOwed` is reached only inside `destructive.RunLocal` (`internal/modules/money/usage_order.go`).

## Tests

Journeys J05 (money arithmetic), J22 (credits) and J40 (books), plus restore in J31.

- OR-J05-S01 pins price-to-provider arithmetic; OR-J05-S02 refuses a malformed price (`TestPublicCatalog`).
- OR-J22-S01: grant, hold, capture and release conserve native units; the day's deduction posts once; another merchant reads nothing (`TestCredits`).
- OR-J22-S02: concurrent capture and revocation cannot go negative (`TestAdmissionBoundaries`).
- OR-J22-S04: a refused batch rolls back ledger and events (`TestCreditAtomicity`); concurrent replay leaves one transfer and `ledger-audit` catches injected counter corruption (`TestLedgerMaintenance`).
- OR-J40-S01: every account type opens in its book and class with its sign rules, the schema refuses any other pairing, each transfer type belongs to one book, and the general ledger refuses a merchant unit and stays out of the units book's readers (`TestBooks`).
- OR-J40-S02: a transfer across books or currencies, or moving another customer's account, is refused by the database whichever writer inserts it, and nothing moves (`TestBooks`).
- OR-J40-S03: a general-ledger posting late on a New York evening lands in its UTC bucket and on that day when grouped in the merchant's zone, a restore rebuilds the rollups, and `ledger-audit` catches injected rollup, counter and conservation corruption (`TestBooks`).
- OR-J40-S05: a database migrated before the books, with its ledger, migrates to them with rollups that `ledger-audit` and an export accept (`TestBooksMigration`).
- OR-J40-S06: concurrent top-ups, one posting unposted usage first, all commit (`TestBooks`).
- OR-J40-S07: an archive written before the books restores as the units book (`TestBooks`).
- OR-J40-S01 also refuses a coordinate reused for a different posting.
- OR-J31-S01, OR-J31-S02: a restore preserves the books, and a corrupt one is refused at commit (`TestBillingArchive`, `TestLedgerMaintenance`).
- OR-J23-S05: a host transaction rollback leaves no charge (`TestHostTransactions`).
- `sqlc vet` prepares the queries.

Gaps:
- No test inserts an UPDATE or DELETE on `ledger_transfers`; the immutability triggers are exercised only indirectly.
- No writer posts to the general ledger yet, so its sign rules are exercised only by the schema.
