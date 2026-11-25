# Books

OpenRails keeps two double-entry books in the same tables, `ledger_accounts` and `ledger_transfers`, told apart by `book`. The **units book** is what customers hold and owe, at face: prepaid balances, credit lots drawn by usage, usage owed in arrears and how that debt was settled. The **general ledger** holds money only, in registry currencies; its accounts and transfer types are declared, and its writer is `books.Books.Post`. Each (merchant, book, currency) is its own ledger, every amount is an integer at the currency's registered scale, and no transfer crosses books, currencies or customers. Card charges for products and their refunds are `payments` rows, not transfers.

## Concepts

- **Native units**: an amount at the currency's scale, `Units.Decimals` in `internal/currency/registry.go`: 10^6 per unit for two-decimal fiat (micros for USD), 10^4 per yen for JPY and KRW, on-chain precision for SOL and stablecoins. **Minor units** are what a card rail settles in (`MinorDecimals`); `NativeShift` is the difference.
- **Chart** (`books.Chart`): every account type with its book, class, scope (system, customer or PSP) and sign rules. A merchant-wide general-ledger account (system or PSP scope) keeps no counters: its balance is its `ledger_type_totals` row, checked against its sign rule at commit. `ledger_accounts_type_check` and `ledger_accounts_scope_check` hold the same chart.
- **Class**: sets an account's normal side. Units book: `held`, `owed`, `issued`, `retired`, `clearing`. General ledger: `asset`, `contra_asset`, `liability`, `revenue`, `contra_revenue`, `expense`, `equity`. Every stored balance is `credits_posted - debits_posted`.
- **Transfer** (`ledger_transfers`): an immutable movement of a positive amount from a debit account to a credit account in one book. `transfer_type` is a closed vocabulary per book: the units book's `ledger.AllTransferTypes`, the general ledger's `books.GeneralTransferTypes`. `usage_reversed` alone is in both.
- **Dates**: `effective_at` is when a transfer takes effect. A units-book transfer takes effect when it posts; a general-ledger posting names its own instant. Nothing in the books stores a zone: a reader groups `ledger_daily`'s 15-minute UTC buckets into days and months of the merchant's `accounting.timezone`, passing the zone as a query parameter (every zone's offset is a whole number of quarter hours).
- **Dimensions**: `customer_id`, `invoice_id`, `invoice_line_id`, `product_id` and `revenue_schedule_id` attribute a transfer, with no foreign keys; `credit_grant_id` names its lot.
- **Revenue schedule** (`revenue_schedules`): one per paid invoice line whose revenue is deferred, and per currency its lots were paid in for a line priced in a merchant's own unit: what was funded into `deferred_revenue`, what has been recognised, over what service, and when it is next due.
- **Coordinate** (`books.Coord`): operation, source and source id. With customer, currency, type, lot, invoice line and book it is the transfer's once-only identity.
- **Lot**: a credit grant (`credit_grants`); a transfer that funds or draws it names it in `credit_grant_id` (a foreign key), and the legs on its balance account keep its `units_credited` and `units_debited`.
- **Rollups**: `ledger_type_totals` sums what every account of a type was credited and debited; `ledger_daily` sums the credits and debits of each 15-minute UTC bucket of `effective_at` per account type, transfer type, PSP and product. A value at a past instant is the total less the flows after it.
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
| Lot bought | `units_sold` → `customer_units` | `deposit` | `credits.Lots.Materialize` |
| Lot given | `units_granted` → `customer_units` | `deposit` | `credits.Lots.Materialize` |
| Spend, one leg per lot | `customer_units` → `units_redeemed` | `credit_spend` | `credits.Lots.Spend` |
| Lot lapsed | `customer_units` → `units_expired` | `credit_expire` | `credits.Lots.ExpireLapsedBy` |
| Lot revoked | `customer_units` → `units_revoked` | `credit_revoke` | `credits.Lots.Revoke` |
| Revocation reversed | `units_revoked` → `customer_units` | `credit_reinstate` | `credits.Lots.Reinstate` |
| Spend past the balance | `customer_owed` → `units_redeemed` | `owed_accrual` | `Ledger.AccrueOwedIdempotent` |
| Owed paid by a rail or remittance | `units_sold` → `customer_owed` | `owed_payment` | `Ledger.PayOwed` |
| Owed paid from a new lot | `customer_units` → `customer_owed` | `owed_repayment` | `owed.Repay` |
| Usage invoice voided | `units_redeemed` → `customer_owed` | `owed_writeoff` | `Ledger.WriteOffOwed` |
| Purchased credit reversed | `customer_units`, `units_expired`, `units_revoked`, `customer_owed`, `units_redeemed` → `units_refunded` | `credit_refund`, `credit_refund_funding` | `purchasedcredits.Service.ApplyReversal` |
| Purchased credit restored | `units_refunded` → where it was taken from | `credit_refund_restore`, `credit_refund_funding` | `purchasedcredits.Service.ApplyReversal` |

`deposit_bonus`, `credit_purchase_revenue`, `credit_refund_cash(_restore)` and the promotion and revenue `credit_refund_funding` legs through `units_refund_clearing` are history: a pack's bonus is part of its rate and a refund's cash is the general ledger's. Reversals of a paid lot from before the credit books still post them until the books are rebuilt.

Usage reaches the units book through postings (`usage_postings`, `MoneyService.postUsageTx`): at each month's end and before any other movement of that balance, per pricing period the price of the period's usage in the event store less what earlier postings took, at (`usage_posting`, `usage_posting:<id>`, `1`): balance first, the rest owed. A period whose volume price fell is credited back to a new lot (`usage_reversed`, `units_redeemed` → `customer_units`, `Ledger.ReverseUsage`). A payment that settles owed points at its transfer (`payments.ledger_transfer_id`).

`usage_reversed` is the one type in both books: the units book's leg is `Ledger.ReverseUsage`'s, the general ledger's `books.Books.ReverseUsage`'s (`revenue_adjustments` → `deferred_revenue_credit`, the new lot priced at face).

### General ledger

| Account | Scope | Class | Sign rule |
|---|---|---|---|
| `cash_clearing` | PSP (`psp_id`) | asset | none |
| `external_receipts` | system | asset | none |
| `usage_receivable` | customer | asset | `credits_must_not_exceed_debits`, checked as it posts |
| `allowance_doubtful` | system | contra_asset | `debits_must_not_exceed_credits`, checked at commit |
| `unapplied_cash`, `deferred_revenue` | system | liability | `debits_must_not_exceed_credits`, checked at commit |
| `deferred_revenue_credit` | customer | liability | `debits_must_not_exceed_credits`, checked as it posts |
| `tax_payable` | system | liability | none |
| `subscription_revenue`, `one_time_revenue`, `usage_revenue`, `breakage_revenue` | system | revenue | none |
| `refunds`, `chargebacks`, `revenue_adjustments`, `promotional_credit` | system | contra_revenue | none |
| `processor_fees`, `chargeback_fees`, `bad_debt` | system | expense | none |
| `opening_balance_equity` | system | equity | none |

Its transfer types are `cash_received`, `cash_applied`, `cash_returned`, `revenue_adjusted`, `revenue_recognized`, `refund_deferred`, `refund_recognized`, `chargeback_deferred`, `chargeback_recognized`, `dispute_won`, `credit_released`, `credit_breakage`, `credit_forfeited`, `credit_forfeit_reversed`, `promotional_credit_applied`, `usage_accrued`, `usage_collected`, `usage_repaid`, `usage_waived`, `usage_reopened`, `usage_reversed`, `bad_debt`, `bad_debt_recovery`, `processor_fee`, `chargeback_fee` and `opening_balance`. Its writers are the cash, revenue and credit postings below and processor fees; nothing reads it but `ledger-audit` and the restore check.

**Processor fees.** Stripe reports each fee on a balance transaction of the charge, refund or dispute it belongs to. A Stripe payment that moved money or a Stripe refund (`re_` or `pyr_`) queues its read in `stripe_fee_reads`, by trigger, in the transaction that records it; a chargeback's payments and a dispute's prevention refund, booked under the dispute's id, read through the dispute. Each notice of a Stripe dispute queues its read unless one waits, and one that changes its status reads it at once, its failures forgotten. The billing pass's `openrails.stripe_fees` scan (`fees.StripeReader`) reads the object's balance transactions through the read-only Stripe client and posts each non-zero fee once, at the balance transaction (coordinate source `stripe_balance_transaction`, source id its id): `processor_fee`, DR `processor_fees` / CR `cash_clearing` of the PSP, or `chargeback_fee` to `chargeback_fees` for a dispute, the other way round for a negative fee. A fee posts in the balance transaction's currency, the account's settlement currency, which can differ from the charge's; nothing converts. A read that fails waits, doubling from a minute to a day; after eight failures it raises `books.fee.unread` and waits for it: fixing the finding (approving it) reads the object again, ignoring it leaves the read waiting. A fee never holds up its charge or refund. The fees covered are those on these balance transactions: Stripe's processing fee on a charge, any part of it a refund returns, a dispute's fee and its return when the dispute is won, and the VAT or other tax Stripe charges on each. Stripe's other fees are not read, each charged on balance transactions of its own: Billing, Radar and Tax fees, payout fees, and a separate dispute countering fee. NMI and CCBill fees are left to the bank feed.

| Movement | Debit → credit | Type (operation) | Posted by |
|---|---|---|---|
| Money taken at a rail | `cash_clearing` of its PSP → `unapplied_cash` | `cash_received` (`cash`) | `books.ReceivePayment` as `PaymentService` stores a charge or completes an attempt; `Books.Receive` for a usage invoice's payment |
| Money received outside OpenRails | `external_receipts` → `unapplied_cash` | `cash_received` (`cash`) | the same |
| A completed bill's cash, per line and payment | `unapplied_cash` → `deferred_revenue` | `cash_applied` (`application`) | `books.ApplyInvoice` |
| A bill settled without cash (a provider's customer balance) | `revenue_adjustments` → `deferred_revenue` | `revenue_adjusted` (`application`) | `books.ApplyInvoice` |
| Revenue earned | `deferred_revenue` → `subscription_revenue` or `one_time_revenue` | `revenue_recognized` (`recognition`) | `books.ApplyInvoice`, `books.Recognize` |
| What an early end leaves | `deferred_revenue` → the same | `revenue_recognized` (`termination`) | `books.Recognize` |
| A forfeit taken back when its end is undone | the revenue account → `deferred_revenue` | `revenue_recognized` (`reinstatement`) | `books.Recognize` |
| A refund or chargeback of unapplied money | `unapplied_cash` → the payment's cash account | `cash_returned` (`refund`, `chargeback`) | `books.PostReversal` |
| A refund's or chargeback's share of a line, still deferred | `deferred_revenue` → the cash account | `refund_deferred`, `chargeback_deferred` | `books.PostReversal` |
| The same share, already recognised | `refunds` or `chargebacks` → the cash account | `refund_recognized`, `chargeback_recognized` | `books.PostReversal` |
| A won dispute's recovery | the cash account → `unapplied_cash`, `chargebacks`, `deferred_revenue` | `dispute_won` (`dispute_won`, `dispute_won_deferred`) | `books.PostReversal` |
| A paid credit lot's price deferred, within its payment's unapplied receipt | `unapplied_cash` (an import without a payment: `opening_balance_equity`) → `deferred_revenue_credit` | `cash_applied` (`opening_balance`) (`credit_issue`, at the payment) | `books.Books.IssueCredit` |
| Credit spent, at the lot's rate | `deferred_revenue_credit` → `usage_revenue` (or an invoice line's `deferred_revenue`) | `credit_released` | `books.Books.MoveCredit` |
| A gift spent, in a registry currency | `promotional_credit` → `usage_revenue` | `promotional_credit_applied` | `books.Books.MoveCredit` |
| A lot lapsed or revoked | `deferred_revenue_credit` → `breakage_revenue` | `credit_breakage`, `credit_forfeited` | `books.Books.MoveCredit` |
| A revocation reversed | `breakage_revenue` → `deferred_revenue_credit` | `credit_forfeit_reversed` | `books.Books.MoveCredit` |
| Owed beyond the balance (a usage posting, a running charge, failed usage past its grace, arrears) | `usage_receivable` → `usage_revenue` | `usage_accrued` (at the owed accrual's coordinate) | `books.Books.AccrueUsage` from `spendBalanceThenOwedTx` and `MoneyService.AccrueOwed` |
| A usage invoice paid by card, remittance or an observed provider payment | `unapplied_cash` → `usage_receivable`, beyond the receivable `usage_revenue` | `usage_collected` (`usage_collection`, `usage_collection_revenue`, at the payment) | `books.Books.CollectUsage` |
| A voided usage invoice's due | `allowance_doubtful` (its allowance), then `revenue_adjustments` → `usage_receivable` | `usage_waived` (`allowance`, `usage_waiver`) | `books.Books.WaiveUsage` |
| An uncollectible usage invoice's due, and its return as it is paid or reopened | `bad_debt` → `allowance_doubtful`, and back | `bad_debt`, `bad_debt_recovery` (`allowance`, at `invoice:<id>`) | `books.Books.SettleAllowance` |
| A usage invoice payment charged back, and its won dispute | `usage_receivable` → the payment's cash account (the rest from `unapplied_cash`, then `chargebacks`), and back (owed from before the books from `usage_revenue`, the loss from `chargebacks`, the rest to `unapplied_cash` and a credit lot) | `usage_reopened`, `cash_returned`, `chargeback_recognized` (`chargeback`); `dispute_won` (`dispute_won_reopened`, `dispute_won_revenue`, `dispute_won_loss`, `dispute_won`) | `books.Books.ReopenUsage`, `books.Books.ResettleUsage` |
| A period whose price fell, given back as a lot | `revenue_adjustments` → `deferred_revenue_credit` | `usage_reversed` (`usage_posting`) | `books.Books.ReverseUsage` |
| Owed repaid from a lot, at face | `deferred_revenue_credit`, `promotional_credit` → `usage_receivable` | `usage_repaid`, `promotional_credit_applied` | `books.Books.RepayFromCredit` |
| A lot's payment reversed | `deferred_revenue_credit`, `refunds` or `chargebacks`, `usage_receivable` → the payment's cash account | `refund_*`, `chargeback_*`, `usage_reopened` (`purchased_credit_refund`) | `books.Books.ReverseCredit` |
| A won dispute | the cash account, `promotional_credit` → `deferred_revenue_credit`, `usage_receivable` (owed still outstanding), `refunds` or `chargebacks` | `dispute_won` (`purchased_credit_refund`) | `books.Books.RecoverCredit` |

A credit lot's money is described in [credits](credits.md).

A spend releases what the lot holds less what its units left are worth (`CreditLot.Worth`), so a fully deferred lot releases in proportion to the units spent, rounded cumulatively. A partly deferred lot, whose payment applied less than its price (`books.credit.unapplied`), takes its shortfall from its first spends: they release nothing until its units left are worth less than it holds, and the spend that crosses that point releases only the difference.

**The usage receivable.** A customer's `usage_receivable` is shared by every source of owed. Each owed accrual in the units book posts it, so units owed (`customer_owed`) equals it; owed that accrued before the books has none. Every draw caps at what it holds, read under a lock on the account (`books.Books.Receivable`): credit repays it at face, a collection settles it and counts the rest as usage revenue, a waiver and an allowance stop at it. A usage invoice's allowance follows its state on every change (`usage.changed`): an uncollectible invoice reserves its due, within the customer's receivable beyond its other invoices' allowances, keeps no more than it still has due, and gives the rest back once it is paid or collected again; a void writes it off. A chargeback of a usage invoice payment owes again what the payment paid, less what its refunds gave back: `owed_reopened` in the units book (DR `customer_owed` / CR `units_sold`) with a pending owed item, the account flagged due so a usage invoice bills it at once whatever its minimum, and even once the currency's usage terms are withdrawn (nothing else would bill that currency again, so the invoice bills all its pending owed), and the receivable again here; the rest of the cash leaves what the payment still holds in unapplied cash, and beyond that (a card's rounding change, which a credit lot took) is a loss to `chargebacks`. Its won dispute settles that owed as funding does, the invoices that claim the customer's debt first; owed from before the books it settles is usage revenue; it then gives back what its chargebacks lost to `chargebacks`, and what it brings beyond is the customer's, a credit lot at face funded from the unapplied cash it returned to, with a `books.dispute.customer_credit` finding so the merchant can refund it instead (`owed.SyncUsageReversals`, run with every reversal sync; it replays the payment's reversals in order, reading one already booked as it posted, so a refund settling later changes no earlier chargeback). A merchant's refund of the payment reopens nothing: it is the usage's contra revenue. `ledger-audit` warns of each customer whose owed and receivable differ.

### Cash and revenue recognition

A payment's money is booked when the payment row is stored with it: a positive, succeeded charge that moved money at a rail, or a manual payment (`books.ReceiptOf`), in the payment's transaction and dated `purchased_at`, at (`cash`, `payment`, payment id). It stays in `unapplied_cash` until the bill it paid completes; a payment stored before the books is booked when its bill is applied. A payment on a closed invoice, a second payment of a paid one, or money beyond a bill's total is never applied; its refund comes from `unapplied_cash`. A usage invoice's payment settles its receivable, and a credit line's money waits for its lot.

When an invoice completes (`invoices.Service.Paid` and `RecordRemittance` after fulfilment, `invoices.RenewalPaid`, `observed.Record`), `books.ApplyInvoice` allocates it in integers, rounding cumulatively so the parts sum exactly: a provider's negative adjustment lines over the positive lines in proportion; the bill's non-cash settlement (the first settled `provider_bills` row's `non_cash_amount`) over the net lines; then each payment, the invoice's own first, over what the lines still lack. An application takes the invoice's lock and allocates only what earlier ones left: lines already scheduled take nothing, and each payment counts less what earlier applications and its reversals took of it, and the non-cash settlement less what was applied of it (`ListInvoiceApplications`), so no payment is applied beyond what it brought however its receipts changed in between. Each line gets a schedule (`CreateRevenueSchedule`, built by `newSchedule`, once per line and currency, so a replay applies nothing again) funded with exactly what it was applied, and its legs at (`application`, `payment` or `invoice`, id) with the invoice line, cash before non-cash. The application is dated when the last payment it used was taken, or the completion if that is earlier. A provider's positive adjustment line (its tax and shipping, which Stripe nets against discounts in one line) is no revenue: its money stays in `unapplied_cash` with a `books.adjustment.unapplied` finding, raised by the first application that leaves it so and never reopened by a later one. A purchase's timed line waits for its access: `observed.Record` applies a provider bill with `books.ApplyRecorded`, and the caller that grants the access (`RegisterPurchase`, the import) applies the invoice again with `books.ApplyInvoice`, over the window granted; a timed line that application still finds without access raises `books.line.unapplied`, closed when its schedule is made. `ledger-audit` checks that no payment is applied beyond what it received, to its bill's lines at (`application`, `payment`, id) and to the credit lot it bought at (`credit_issue`, `payment`, id) (`books.CheckApplications`) and that every completed revenue line has its schedule (`books.CheckSchedules`). Two kinds of line are warnings, never failures: one whose invoice was made and completed before the merchant's books started (`books_starts`, recorded by the payment writer that first booked a receipt, by its clock), which the books' rebuild schedules, and a provider's tax and shipping line; a line in a merchant's own unit is checked by its lots' releases, failing one whose money has no schedule.

A schedule's service is a subscription's period (`subscription_revenue`, ratable; by the month for a calendar price, each month an equal share and linear within it, so an annual price recognises a twelfth a month), a purchase's access window (`one_time_revenue`, ratable over the `product_access` row, else access hours × seats from the application), or delivery at once (`one_time_revenue`, point in time). What the service earned by t is ⌊funded × elapsed ÷ service⌋, exactly `funded` at its end. Recognition starts at payment: the application posts what the service earned by then, dated then. After that the schedule is due at each midnight on the 1st in the merchant's `accounting.timezone` (`books.MonthStart`, by `time.LoadLocation`, so DST and leap years are the zone database's; the 1st begins as PostgreSQL resolves its midnight, `calendarutil.DayStart`: at the transition where clocks spring forward over it, at the later one where it happens twice) and at its end. Each boundary posts what was earned by it less what was recognised, dated the last microsecond before the boundary so it lands in the month it recognises, at (`recognition`, `revenue_schedule`, `<schedule>@<boundary>`): a replayed boundary posts nothing. The zone is read when a schedule is due, so a change moves only the boundaries not yet posted.

An early end ends a schedule: `subscriptions_end_revenue_schedules` (a subscription's `ended_at` set before the period's end, other than by an upgrade) and `product_access_end_revenue_schedules` (access revoked, deleted or cut short) set `ended_at` and make it due then, rather than recognising inline, so a refund in the same transaction still meets the unrecognised revenue first. The recognition job then posts what the service earned by the end and forfeits the rest (`termination`). An end undone or moved later (a reactivation, a destructive run's undo) makes the schedule due at the old end, or earlier if a boundary is; the job takes back what was forfeited (`reinstatement`) and recognises on. Each termination and reinstatement is at `<schedule>@<instant>#<generation>`, the generation the schedule's `reinstatements`, so an end that flaps at one instant posts each time. An end reached before the schedule exists (a charge after a cancellation, a subscription imported canceled) is read from the subscription at the application; a bill paid after it is forfeited at payment. A replaced subscription's periods (an upgrade) keep running. A prune that soft-deletes a booked payment raises `books.payment.pruned`, and one that soft-deletes a subscription whose schedules keep recognising raises `books.subscription.pruned`; its undo (`RollbackDestructiveRun`) closes both.

`openrails.revenue_recognition`, a scan of the hourly billing pass (`RevenueRecognitionWorker`), reads the schedules due by `revenue_schedules_due_idx` across merchants in keyset pages of 200 and recognises each in its merchant's scope and its own transaction (`books.Recognize`, `LockDueRevenueSchedule`). An idle pass reads no row; at a month's end the work is the live schedules.

`books.Replay` books one invoice's money again through the same posters, for the books' rebuild: what posted live posts nothing again, and a schedule it makes starts at its application, the job then posting each boundary since in its own month.

### Reversals

A settled reversal of a payment posts once, at (`refund`, `chargeback` or `dispute_won`, `payment`, the reversal's id) and the line it moves (`books.PostReversal`), where its row settles: at the end of `recordRefund`, after `ApplyRefundAccessPolicy` (provider refunds, chargebacks, imports), in `CompleteRefundReservation` (staff and engine refunds, after the access they revoke), and where a won dispute's recovery is stored or linked (`Create`, `LinkRefundedPayment`); the credit lot's pass (`syncPurchasedCreditRefund`) books any other reversal of the payment it decides. A replay posts nothing, and so does a reversal of a payment that brought no money (`books.ReceiptOf`). Its legs are dated when it settled (`books.SettledAt`, the row's `purchased_at`): when the rail confirmed it, or the provider's occurrence time when that is what is known; a staff refund the rail confirms later settles at its confirmation (`CompleteRefundReservation` sets it). The credit lot's legs are dated the same way.

The credit lot a payment bought comes first: its poster (`books.Books.ReverseCredit`, `books.Books.RecoverCredit`) moves its share at (`purchased_credit_refund`, …, the reversal's id) before `PostReversal`, which moves only the rest (`GetLotReversalCash`): all of it for a pack of a merchant's unit the general ledger never booked. The share is decided once, when the reversal first settles, and kept in `credit_reversal_shares` (cash and units, zero included): a refund that found the lot already returned keeps none of what a later won dispute restores to it. A refund or chargeback then takes the payment's money from what it left unapplied (`cash_returned`), so a late or duplicate payment's refund comes from `unapplied_cash`, never beyond what the payment brought; then from each schedule the payment funded, pro rata to what the schedule still holds of it, rounding cumulatively. A schedule still serving first recognises what its service earned by the reversal, then takes a price reduction: its funding falls by the share, recognised revenue falls to what the lower price earned by then, that difference is the contra (`refunds` or `chargebacks`) and the rest leaves `deferred_revenue`. A schedule whose service has ended (an early end set in this or an earlier transaction, or its period over) first recognises what its service earned up to its end, then gives up its unrecognised revenue before any contra; what remains is still forfeited by the job. A schedule records what an early end forfeited (`forfeited`), a contra comes out of that first, and an end undone takes back no more than is left: revenue a reversal took is never reinstated. A won dispute's recovery brings back what its chargeback (the row its `reverses_payment_id` names; a recovery naming none restores the payment's chargebacks oldest first by when they occurred, its lot's share first) took and earlier recoveries have not brought back, pro rata. Earlier recoveries count in the order they were stored: one naming a chargeback against it, one naming none against the chargebacks stored before it, so a chargeback reported late never takes back what an earlier recovery restored. It returns to `unapplied_cash`, and per schedule as a price restored while it serves, or, once its service has ended, its contra to `chargebacks`, the rest to `deferred_revenue`, forfeited at once when no early end's forfeit is still to come. A reversal of money the books never received (a payment stored before them) is drawn from `unapplied_cash` whole: never refused, the breach of its sign rule is a `books.sign_breach` finding until the books' rebuild books the receipt. A usage invoice's payment is the exception: its cash settles the receivable as it is collected (`usage_collected`, applied in the payment's books), so a merchant's refund of delivered usage reopens nothing owed and is contra beyond whatever the payment left unapplied (`refund_recognized` to `refunds`), while its chargeback and won dispute are `owed.SyncUsageReversals`' (see the usage receivable above), which books them before `PostReversal` would.

## How it works

The units book enforces business rules and refuses (no spending credits a customer does not have); the general ledger records what happened and never refuses a posting for a sign or balance rule: a breach is the finding `books.sign_breach`. B4 and B5 bound their draws by the lot's own counters, never by a general-ledger refusal.

The units book's movements funnel through `Ledger.ApplyIdempotent` (`internal/modules/money/ledger/ledger.go`), the general ledger's through `books.Books.Post`; each inserts once (`InsertLedgerTransfer`, `InsertGeneralLedgerTransfer`):

1. `Coord.Validate` refuses a blank part, an operation that is only a prefix, or one over `MaxCoordBytes` (512). `Post` also refuses a type or account outside the general ledger, a customer leg naming another customer, and a currency outside the registry (`books.CheckCurrency`, `books.ErrNotMoney`): a merchant's own unit never enters it.
2. The insert runs `ON CONFLICT DO NOTHING` on `ledger_transfers_operation_once_key`. No row back means the coordinate already committed: the call returns that row with `applied` false and moves nothing. `Post` also compares it with the posting and refuses a different one (`books.ErrCoordinateReused`).
3. `trg_ledger_transfers_book_rules` (before insert) refuses a transfer whose accounts are in another book (`ledger_cross_book`) or currency (`ledger_cross_currency`), or that moves a customer's account in a transfer not attributed to that customer (`ledger_customer_mismatch`).
4. `trg_ledger_transfers_apply_counters` locks the accounts that keep counters (every units-book account and the general ledger's customer accounts) in id order, checks their sign rules and bumps the counters. A units-book breach is refused (relaxed for a debit by `allow_debit_negative_up_to`; `ledger_insufficient_funds` surfaces as `ledger.ErrInsufficientFunds`); a general-ledger customer account's raises `books.sign_breach` for that customer and commits. It never locks a merchant-wide general-ledger account, so no posting holds one until it commits. `LedgerAccountBalance` reads no row for a merchant-wide general-ledger account.
5. `trg_ledger_transfers_rollups` queues the transfer in `ledger_rollup_pending`. At commit, the deferred `ledger_rollups_settle` folds the queued transfers into `ledger_type_totals` and `ledger_daily` (`settle_ledger_rollups`), so no two writers wait on each other's rollups mid-transaction. A merchant-wide account's type total, locked there, is checked against the account's sign rule. A breach never refuses the commit (before the books are rebuilt from history, a refund of money received earlier can draw `deferred_revenue` or `unapplied_cash` below zero): it raises `books.sign_breach`, one per type, currency and (for a customer's account) customer, reopened by a later breach once resolved, and `ledger-audit` fails on it. A writer cannot read such a balance mid-transaction, so the general ledger's writers bound a draw by their own facts (a draw on `unapplied_cash` by the per-payment cash it applies), never by a balance read.
6. A units-book transfer that names a customer calls `WatchChange` in the same transaction, which queues a balance look when the change crosses a line or the watch looks at every change (see [credits](credits.md)).

Merchant-wide general-ledger accounts are opened where a currency or PSP becomes sellable, outside any business transaction, so no first posting in it waits on another's uncommitted account: `books.Open` makes the merchant's general ledger whole in a short transaction of its own (`db.Pool.CommittedMerchantTx`, refused inside a caller's transaction), giving every currency its catalog prices in, every currency named and every currency already open each merchant-wide account and each PSP's cash clearing, and only reads once whole. `books.Prepare` runs it, at public entry points only and never inside a caller's transaction (where it does nothing), and never fails the operation: after a catalog apply, a price or provider price created, a configuration sync that declares or arms a PSP (at start-up too), before a billing import, once the batch is validated, with its transactions' currencies, and before a PSP import's apply with its records'. Bookkeeping never blocks a money movement: a posting that meets an account nothing opened creates it in its own transaction, as any first writer would, and raises the finding `books.account.opened_late` (`books.OpenedLateFindingType`, low severity) once per account so the gap is closed.

Locks are taken in one order. During its statements a writer locks the customer row (`LockCustomerForSpend`), then the accounts that keep counters in id order, and a lot's row. Everything a transaction keeps at commit is kept by one sequence, `billing.settle_commit`, after every statement-time lock:

1. the ledger rollups of every merchant the transaction posted for, merchants in id order: `ledger_type_totals`, then `ledger_daily`, each in key order (`settle_ledger_rollups`);
2. MRR of every merchant with a changed subscription, in id order (`settle_mrr`: `customer_mrr`, the movements, the flow rollups and `mrr_totals`; see [subscriptions](subscriptions.md));
3. the event streams (`flush_event_log`: each `eventlog_streams` row in (source, scope) order, then the events; see [events](events.md)).

Steps 1 and 2 repeat until neither queue holds work, then step 3 runs. Ledger or MRR work queued once step 3 has begun, in that run or a later one in the transaction (`openrails.event_log_flushed`), would take its rows out of order and is refused.

Every commit-time queue fires the same trigger function, `settle_at_commit`: `ledger_rollups_settle` on `ledger_rollup_pending`, `subscription_mrr_settle` on `subscription_mrr` and `eventlog_flush_at_commit` on `eventlog_pending`. The first event whose row is still unsettled runs the whole sequence, whichever was queued first; each later one finds its row settled and returns. A new commit-time rollup is a step in this sequence and its queue fires `settle_at_commit` with a probe of its own row: MRR's at-risk rollups (`mrr_risk_totals`, `mrr_risk_flows`) are step 2's last part, after `mrr_totals` and before the event streams.

Facts never change. `immutable_ledger_transfers` refuses UPDATE, DELETE and TRUNCATE; `immutable_revenue_schedule_facts` lets a schedule change only its funding, progress, end, due instant and generation; `guard_ledger_account_facts` lets only the nested counter trigger touch `credits_posted` and `debits_posted`, and `guard_ledger_daily` and `guard_ledger_type_totals` only the commit-time settlement write the rollups. Accounts are created on first write through the chart (`books.EnsureAccount`; `EnsureSystemAccount`, `EnsureCustomerBalance`, `EnsureCustomerArrears` for the units book), a merchant-wide general-ledger account by `books.Open` beforehand; reads never create one, so an unknown customer reads zero. Writers serialize on the customer row (`LockCustomerForSpend`, taken by `MoneyService.lockBalance` and `credits.Lots.LockCustomer`) before any read-then-write of a balance.

The units book's readers read it alone: `ListLedgerTransfersByCustomer` and `ListLedgerTransfersByIDs` filter `book = 'units'`, and the metrics balance family's `outstanding_credit_liability` and `outstanding_owed` read `customer_units` and `customer_owed` of the units book.

No FX: a transfer's currency must equal both accounts'. Rates (`internal/integrations/fx`) only price a Solana quote and convert a spend cap at admission (`fx.ConvertAmount`); no books, grants or money code imports them.

Integrity is checked on demand. `ledger.CheckIntegrity` runs conservation (each book's balances sum to zero per currency, a merchant-wide general-ledger account's balance being its type total), counter drift (account counters against the transfer log; a merchant-wide general-ledger account's stay 0), lot drift (each credit lot's `units_credited` and `units_debited` against the legs on its balance account, `CheckLotDrift`) rollup drift (`ledger_type_totals` and `ledger_daily` recomputed from the transfer log) sign breaches (a merchant-wide account's type total across its sign rule, `CheckSignBreaches`) postings (every receipt, and every settled reversal of one, stored since the payment the books first booked, has a posting, `books.CheckPostings`) and reversal cash (each such reversal's cash legs, its own and its lot's, sum to its amount, `books.CheckReversalCash`) for one merchant, and warns, without failing, of a merchant-wide account with no sign rule whose type total sits opposite its class's normal side (`CheckSides`, the `SIDES` column); `openrails ledger-audit` runs it per merchant, raises `books.entry.missing` for each payment stored without its posting and `books.reversal.misbalanced` for each reversal posted for other cash (closing each once it no longer holds), and exits non-zero on a finding. The books' start is `books_starts`: recorded once, when a writer first books a receipt (a payment's or a usage invoice's), by its clock; payments and invoices stored before it wait for the books' rebuild, and a completed line whose invoice's money was all returned is no longer owed a schedule. The books migration filled the rollups from the transfers already posted. A merchant archive carries no rollups: during a restore the counter triggers are skipped (`billing_restore_active`) while the restored transfers queue their rollups, `finish_billing_restore` settles them, and `check_billing_restore_ledger` verifies counters, conservation per book and currency as `ledger-audit` reads them, each transfer's book, currency and customer against its accounts, and both rollups, and `check_billing_restore_credit_grants` the lots' counters, before the restore commits (and at export).

## Interfaces

- Go: `Client.GetBalance` (`billing.Balance`: balance, held, accrued, available, owed) and `Client.ListBalanceTransactions` (`billing.BalanceTransaction`; a usage deduction carries `Usage`) read the units book. `catalog.Currencies` and `catalog.ParseCurrency` read the registry. The general ledger and its schedules have no public reader.
- Jobs: the `openrails.revenue_recognition` scan of `openrails.billing_pass` ([River jobs](../river-jobs.md)).
- HTTP: `GET /v1/admin/customers/{customer_id}/balance` and `GET /v1/admin/customers/{customer_id}/balance/transactions` (`merchant:customer:read`); the customer's `GET /v1/me` and `GET /v1/me/balance/transactions`; `GET /v1/config` lists `currencies`.
- Configuration: `accounting.timezone` ([merchant configuration](merchant-configuration.md)).
- Error codes: `currency_unsupported`, `invalid_cursor`, `invalid_customer_id`.
- CLI: `openrails ledger-audit [--merchant=…] [--format=table|json]`.
- Docs: [glossary](../guide.md), [credits](../client.md#credits), [usage billing](../client.md#usage-and-running-charges).

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
- `ListLedgerConservationBreaches` sums every balance per book and currency (an account's counters, or a merchant-wide general-ledger account's type total); `openrails ledger-audit` fails on a non-zero sum.
- OR-J05-S01 pins 9.99 USD × 3 at 29970000 native and 2997 minor units; OR-J40-S01 refuses a merchant unit in the general ledger.

### 4. Access control and tenant isolation
Both tables key on `PRIMARY KEY (merchant_id, id)`, and a transfer's account foreign keys include `merchant_id`, so a transfer cannot reach another merchant's account. `ledger.New` and `books.New` bind one merchant and every query passes it. A transfer that moves a customer's account names that customer, enforced by the trigger. Staff reads need `merchant:customer:read`; the customer routes read `selfAccountPayer`, never a path id.

Checks:
- `ledger_transfers_debit_account_id_fkey` and `ledger_transfers_credit_account_id_fkey` reference `(merchant_id, id)`.
- Every query on `ledger_accounts`, `ledger_transfers`, `ledger_daily` or `ledger_type_totals` in `internal/db/queries/` filters `merchant_id`.
- OR-J22-S01 reads the customer through another merchant and gets no transactions; OR-J40-S02 refuses a transfer that moves another customer's account.

### 5. Contracts match the code
`billing.Balance`, `billing.BalanceTransaction`, `billing.BalanceTransactionType` and the routes above are in `api/go.txt` and `api/openapi.json`; the tables are in `api/schema.txt`. Stored transfer names never change: the usage invoice operations keep their older names (`invoice_payment`, `manual_invoice_payment`, `invoice_void`). The in-process `Client` calls the same routes over an in-process transport (`engine.NewClient` in `embedded.go`), so it answers as the remote one does.

Checks:
- `go run ./scripts/contracts` shows no removed line for these types and routes.
- Both books' vocabularies are declared in `ledger_transfers_type_check` and `ledger_accounts_type_check`; OR-J40-S01 holds `ledger.AllTransferTypes`, `books.GeneralTransferTypes` and `books.Chart` to them.
- A units-book transfer type maps to a `billing.BalanceTransactionType` in `internal/modules/money/genmap.go`.

### 6. Data lifecycle
`ledger_accounts`, `ledger_transfers`, `ledger_daily` and `revenue_schedules` are `Permanent`, `ledger_type_totals` `State` (`internal/retention/tables.go`, stated in each table comment). None is partitioned or deleted. Accounts, transfers and schedules move with the merchant archive; the rollups are rebuilt from them.

Checks:
- All five, and `ledger_rollup_pending` (`State`, empty at commit), are in `ownedTables` (`internal/merchantarchive/owned_tables.go`) and in the restore guard's table list; the rollups are excluded from the archive as derived and the queue as scratch (`internal/merchantarchive/checks.go`).
- An archive from before the books restores as the units book, and one from before revenue schedules with none (`normalizePreBooks`).

### 7. Work scales with activity
A balance or owed read is one counter row (`LedgerAccountBalance`); an account type's total is one `ledger_type_totals` row and a period's flows are its buckets in `ledger_daily`. An insert locks at most two accounts, never a merchant-wide general-ledger account, and, for a lot's leg, its lot; its rollup rows are locked at commit, and a merchant's settlement reads only the transfers its transaction queued. A lot's remainder is its counters. No job scans the books: the month-end usage close finds its customers by `usage_unposted_due_at_idx`, balance looks are queued by crossings. `ledger-audit` reads a merchant's whole log, on demand only. Revenue schedules are found by `revenue_schedules_due_idx`, and an early end finds its subscription's or access's schedules by `revenue_schedules_subscription_id_idx` and `revenue_schedules_product_access_id_idx`.

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
Nothing deletes or rewrites a transfer: a correction is a compensating transfer. Undo runs refuse the five tables (`reconcile.NeverRollbackableTables`) and a merchant purge leaves them (`purgeMerchantRows` has no case for them). Staff commands that move money are admitted where they start: `destructive.RevokeCredit`, `destructive.Refund`, `destructive.WaiveInvoice`. Each transfer records its coordinate and, where known, `invoker_id`, `resource` and `invoice_id`.

Checks:
- `immutable_ledger_transfers`, `immutable_ledger_transfers_truncate`, `guard_ledger_account_facts`, `immutable_ledger_accounts_truncate`, `guard_ledger_daily`, `guard_ledger_type_totals`, `immutable_revenue_schedule_facts` and their truncate triggers exist.
- `NeverRollbackableTables` lists `ledger_transfers`, `ledger_accounts`, `ledger_daily`, `ledger_type_totals` and `revenue_schedules`.
- `WriteOffOwed` is reached only inside `destructive.RunLocal` (`internal/modules/money/usage_invoice.go`).

## Tests

Journeys J05 (money arithmetic), J22 (credits), J40 (books) and J41 (revenue recognition), plus restore in J31.

- OR-J05-S01 pins price-to-provider arithmetic; OR-J05-S02 refuses a malformed price (`TestPublicCatalog`).
- OR-J22-S01: grant, hold, capture and release conserve native units; the day's deduction posts once; another merchant reads nothing (`TestCredits`).
- OR-J22-S02: concurrent capture and revocation cannot go negative (`TestAdmissionBoundaries`).
- OR-J22-S04: a refused batch rolls back ledger and events (`TestCreditAtomicity`); concurrent replay leaves one transfer and `ledger-audit` catches injected counter corruption (`TestLedgerMaintenance`).
- OR-J40-S01: every account type opens in its book and class with its sign rules, a merchant-wide general-ledger account reads no balance, the schema refuses any other pairing, each transfer type belongs to one book but `usage_reversed`, which posts in both at one coordinate and reads as a spend given back or usage owed taken back, and the general ledger refuses a merchant unit and stays out of the units book's readers (`TestBooks`).
- OR-J40-S02: a transfer across books or currencies, or moving another customer's account, is refused by the database whichever writer inserts it, and `books.Open` inside a business transaction is refused; nothing moves. A draw on unapplied cash beyond what was received commits, raises `books.sign_breach` and fails `ledger-audit`; resolved, a later breach reopens it; a customer's usage receivable credited beyond what was accrued commits and raises it for that customer (`TestBooks`).
- OR-J40-S03: a general-ledger posting late on a New York evening lands in its UTC bucket and on that day when grouped in the merchant's zone, a restore rebuilds the rollups, and `ledger-audit` catches injected rollup, counter, conservation and sign corruption, a merchant-wide general-ledger account's counters included, and warns of a revenue total on the debit side while the books stay OK (`TestBooks`).
- OR-J40-S05: a database migrated before the books, with its ledger, migrates to them with rollups that `ledger-audit` and an export accept (`TestBooksMigration`).
- OR-J40-S06: concurrent top-ups, one posting unposted usage first, all commit (`TestBooks`).
- OR-J40-S07: an archive written before the books restores as the units book (`TestBooks`).
- OR-J40-S08: two transactions, one posting and then changing a subscription's MRR, the other the reverse, the first held at commit inside the ledger step: both commit (`TestBooks`).
- OR-J40-S09: the first two checkouts in a currency a catalog apply made sellable overlap without waiting; checkouts in a currency nothing opened still post and raise `books.account.opened_late` once per account; two checkouts' cash postings and a usage collection that locks the customer first, in opposite lock orders, all commit, and the merchant-wide accounts' counters stay 0 (`TestBooks`).
- OR-J40-S23: two transactions, one posting and then queueing an event, the other the reverse, the first held at commit: both commit and both events are numbered; ledger work queued after the event streams flushed is refused (`TestBooks`).
- OR-J40-S24: two transactions posting for two merchants in opposite invoices, the first held at commit inside the lower merchant's ledger step: both commit (`TestBooks`).
- OR-J40-S10 to S19: a credit lot's money in the general ledger through spend, expiry, revocation, reinstatement, owed repayment, refunds, chargebacks, recoveries and restores, in pinned micros (`TestCreditBooks`, `TestCreditBooksEdges`); OR-J40-S25: a lot applied beyond its payment's cash fails `ledger-audit`; OR-J40-S26: a lot whose payment brought no cash defers nothing and raises `books.credit.unapplied` (`TestCreditBooksEdges`).
- OR-J40-S20 to S22, S27 to S29: usage owed beyond the balance is receivable and its card and remittance collections settle it; a waiver, an uncollectible invoice collected or waived (the allowance); units owed equal the receivable after a posting, a running charge, a remittance, a gift, an allowance, a waiver and a chargeback under the minimum invoice billed at once and collected; owed from before the books is collected as revenue beside a running charge's receivable (the audit warns until then); a chargeback of a payment with rounding change reopens owed once, the change a loss, and the dispute won after the next invoice collected gives the loss back and is the customer's credit for the rest, nothing left unapplied; reopened owed is billed even once usage terms are withdrawn; a refund leaves owed settled, a chargeback after it reopens only what it left and keeps it when a reserved refund settles later, and its won dispute settles it and gives the loss back (`TestUsageReceivable`).
- OR-J40-S40: a Stripe charge's fee and a dispute fee post once, exactly (pinned micros); a balance transaction read again posts nothing new; a dispute is read again when its status changes, and a notice that changes none keeps a failing read's count; a won dispute's and a refund's returned fees post back; its fees sit beside the charge's cash in `cash_clearing` with no side warning; a euro charge settled in pounds posts its fee in pounds, the one side warning (`SIDES`) on cash clearing; a read Stripe fails waits without failing the pass, after eight failures waits for `books.fee.unread`, and posts once it is fixed (`TestProcessorFees`).
- OR-J40-S41: a prevention refund booked after its dispute was read queues no refund read; the dispute's later change posts its returned fee (`TestProcessorFees`).
- OR-J40-S01 also refuses a coordinate reused for a different posting.
- OR-J41-S01: a monthly plan bought mid-month posts its cash to its PSP's clearing account, defers it, and recognises pinned micros at the month boundary and the period end (`TestRevenueRecognition`).
- OR-J41-S02: an annual calendar plan recognises a twelfth a month across a leap February; in New York the boundary moves with DST (`TestRevenueRecognition`).
- OR-J41-S03: lifetime access at once, a time-limited pass ratable over its window, a per-seat line funded per seat (`TestRevenueRecognition`).
- OR-J41-S04: a provider bill settled partly by a customer balance, rebills on NMI and CCBill PSPs and a pull in USDC each book cash to their PSP, the non-cash part to `revenue_adjustments`, and a negative adjustment line reduces the others (`TestRevenueRecognition`).
- OR-J41-S05: a seat increase defers its adjustment over the rest of the period (`TestRevenueRecognition`).
- OR-J41-S06: an idle pass locks no schedule; concurrent passes and a replay post each boundary once (`TestRevenueRecognition`).
- OR-J41-S07: an external payment posts to `external_receipts`; a payment on a closed invoice stays unapplied (`TestRevenueRecognition`).
- OR-J41-S08: an immediate cancellation without a refund forfeits the remainder at its end (`TestRevenueRecognition`).
- OR-J41-S09: a usage invoice's card and remitted payments book their cash and apply it to the usage receivable (`TestRevenueRecognition`).
- OR-J41-S10: revoked access ends its schedule; the revocation undone takes the forfeit back; an end that flaps at one instant posts each generation; an undone end keeps an earlier boundary due (`TestRevenueRecognition`).
- OR-J41-S11: a late payment on an expired invoice whose product another invoice holds stays unapplied (`TestRevenueRecognition`); OR-J38-S43 does the same for a second payment of a paid invoice.
- OR-J41-S12: a purchase recorded with its tax raises the tax's finding at once; applied with no access grant it raises a finding and fails `ledger-audit`; applied after its grant it covers the stacked window, a replay applies an invoice with no schedule, and revocation before the window forfeits it (`TestRevenueRecognition`).
- OR-J41-S13: a prune that hides booked payments or a recognising subscription raises findings, and its undo closes them (`TestRevenueRecognition`).
- OR-J41-S14: a charge after a cancellation, on a subscription imported canceled, is forfeited at payment (`TestRevenueRecognition`).
- OR-J41-S15: `ledger-audit` warns of a line completed before the books started and of a provider's tax line, skips an invoice in a merchant's own unit, and fails a line completed since (`TestRevenueRecognition`).
- OR-J41-S16: an invoice applied again after a payment sorting before another arrives applies no payment beyond what it brought (`TestRevenueRecognition`).
- OR-J41-S17: an imported purchase is applied over the access its import grants, from when it was paid (`TestRevenueRecognition`).
- OR-J41-S18: a partial refund while the service runs is a price reduction with catch-up (`TestRevenueReversals`).
- OR-J41-S19: a full refund that revokes access takes unrecognised revenue first (`TestRevenueReversals`).
- OR-J41-S20: a chargeback reduces its schedule and the won dispute restores it, its deferral forfeited at once (`TestRevenueReversals`).
- OR-J41-S21: a refund row written past the poster commits; `ledger-audit` names it and raises `books.entry.missing` (`TestRevenueReversals`).
- OR-J41-S22: a duplicate capture's refund returns it from `unapplied_cash` (`TestRevenueReversals`).
- OR-J41-S23: a refund of a payment from before the books posts and raises `books.sign_breach` (`TestRevenueReversals`).
- OR-J41-S24: a usage invoice's refund is contra revenue and its chargeback reopens the owed, receivable again, with no contra (`TestRevenueReversals`).
- OR-J41-S25: refunds in pieces each reduce the price from where the last left it (`TestRevenueReversals`).
- OR-J41-S26: a refund of a two-line bill takes each line pro rata (`TestRevenueReversals`).
- OR-J41-S27: a mixed credit invoice's refund, chargeback and won dispute move each dollar once, the lot's share first (`TestRevenueReversals`).
- OR-J41-S28: an end undone after a refund reinstates no forfeit it never made (`TestRevenueReversals`).
- OR-J41-S29: a refund the rail confirms later is dated when it settled, its credit lot's legs with its own (`TestRevenueReversals`).
- OR-J41-S30: a timed line refunded before its access is granted is not unscheduled (`TestRevenueReversals`).
- OR-J41-S31: a refund that found the credit lot charged back keeps none of what the won dispute restores, in card credits and in a merchant's own unit (`TestRevenueReversals`).
- OR-J41-S32: a reversal posted for other cash than its amount fails `ledger-audit` and raises `books.reversal.misbalanced` (`TestRevenueReversals`).
- OR-J41-S33: a usage invoice's payment, the merchant's first receipt, starts the books (`TestRevenueReversals`).
- OR-J41-S34: a refund after an early end takes the forfeit first, and the end undone reinstates only what is left (`TestRevenueReversals`).
- OR-J41-S35: a recovery naming no chargeback commits and restores the chargeback that occurred first (`TestRevenueReversals`).
- OR-J41-S36: a provider's discount on a bill with a credit line reduces the other lines alone (`TestRevenueReversals`).
- OR-J41-S37: a refund of an unbooked pack of a merchant's unit withdraws its units, books its cash against the payment and raises `books.credit.unbooked_unit_pack` (`TestRevenueReversals`).
- `TestMonthStartWhereMidnightIsSkipped`, `TestMonthStartWhereMidnightRepeats`, `TestScheduleEnds` and `TestRecognitionBoundaries` pin the boundaries (a midnight that never happens or happens twice, DST, leap years, a zone change), a schedule ended before its start, an end undone, and the allocation; `TestApplicationsStayWithinReceipts` a second application's; `TestReversalAdjustments`, `TestReversalDrawsFromItsPayment` and `TestPaymentBooksFromLegs` a reversal's.
- OR-J31-S01, OR-J31-S02: a restore preserves the books, and a corrupt one is refused at commit (`TestBillingArchive`, `TestLedgerMaintenance`).
- OR-J23-S05: a host transaction rollback leaves no charge (`TestHostTransactions`).
- `sqlc vet` prepares the queries.

Gaps:
- No test inserts an UPDATE or DELETE on `ledger_transfers`; the immutability triggers are exercised only indirectly.
- No check proves a schedule's `recognized_net` against its recognitions.
- A fee settled in another currency than its charge leaves the PSP's cash clearing in that currency on its credit side until settlements are booked (`ledger-audit` warns, `SIDES`); the charge's cash sits in the charge's currency.
