// Package ledger is the units book's writer: the double-entry, append-only
// record over ledger_accounts / ledger_transfers of what customers hold and
// owe, at face. A ledger is a (merchant, currency) pair; transfers move an
// amount debit->credit within one ledger and are immutable. Account counters
// hold balances; transfers are the truth. Every transfer is posted: holds
// live outside the ledger. The chart and the general ledger are package books.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/open-rails/openrails/billing"
	unit "github.com/open-rails/openrails/internal/currency"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/money/books"
)

// AccountType identifies an account's role in its book (books.Chart).
type AccountType = books.AccountType

// The units book's accounts.
const (
	CustomerUnits = books.CustomerUnits
	UnitsRedeemed = books.UnitsRedeemed
	UnitsSold     = books.UnitsSold
	CustomerOwed  = books.CustomerOwed
	UnitsExpired  = books.UnitsExpired
	// UnitsRevoked holds the unspent remainder clawed back when a credit grant
	// is revoked (UnitsExpired is time-lapse): frozen and reversible, not
	// refunded; a refund moves it out to UnitsSold.
	UnitsRevoked = books.UnitsRevoked
	UnitsGranted = books.UnitsGranted
	// UnitsRefunded is where a refunded lot's units go.
	UnitsRefunded       = books.UnitsRefunded
	UnitsRefundClearing = books.UnitsRefundClearing
	UnitsRefundLoss     = books.UnitsRefundLoss
)

// TransferType is ledger_transfers.transfer_type; these are the units book's,
// mirrored by ledger_transfers_type_check. The lot-once index
// ledger_transfers_credit_grant_id_transfer_type_key is partial on these literals, so
// a typo would escape it and post a duplicate.
type TransferType = books.TransferType

const (
	// DepositBonus and CreditPurchaseRevenue are history: a pack's bonus is
	// part of its rate now, its money the general ledger's.
	DepositBonus          TransferType = "deposit_bonus"
	CreditPurchaseRevenue TransferType = "credit_purchase_revenue"
	// CreditRefund takes a refunded lot's units left to units_refunded, and
	// CreditRefundFunding its units already retired, owed or consumed;
	// CreditRefundRestore and CreditRefundFunding return them.
	CreditRefund        TransferType = "credit_refund"
	CreditRefundRestore TransferType = "credit_refund_restore"
	CreditRefundFunding TransferType = "credit_refund_funding"
	// CreditRefundCash and CreditRefundCashRestore are history: a refund's
	// cash is the general ledger's.
	CreditRefundCash        TransferType = "credit_refund_cash"
	CreditRefundCashRestore TransferType = "credit_refund_cash_restore"
	Deposit                 TransferType = "deposit"       // units_sold -> customer_units
	CreditSpend             TransferType = "credit_spend"  // customer_units -> units_redeemed
	CreditExpire            TransferType = "credit_expire" // unspent lot remainder, time-lapsed
	CreditRevoke            TransferType = "credit_revoke" // unspent lot remainder, clawed back
	// CreditReinstate reverses a clawback (units_revoked -> customer_units).
	CreditReinstate TransferType = "credit_reinstate"
	OwedAccrual     TransferType = "owed_accrual" // postpaid usage -> customer_owed
	OwedPayment     TransferType = "owed_payment" // arrears settled by an external charge
	// OwedWriteoff cancels accrued debt without money moving when a usage invoice
	// is voided: the inverse of OwedAccrual. OwedPayment means a rail collected.
	OwedWriteoff TransferType = "owed_writeoff"
	// OwedRepayment pays debt from a newly funded credit lot (customer_units
	// -> customer_owed), naming that lot.
	OwedRepayment TransferType = "owed_repayment"
	// OwedReopened is a chargeback of a usage invoice payment owing again what
	// it paid (customer_owed -> units_sold): the inverse of OwedPayment.
	OwedReopened TransferType = "owed_reopened"
	// UsageReversed credits a lot with what a period's usage posted beyond
	// its price, once a volume tier's price fell (units_redeemed ->
	// customer_units), naming the lot; a leg that reduces owed names none.
	// The general ledger shares the type.
	UsageReversed = books.UsageReversed
)

// AllTransferTypes is the units book's whole vocabulary (OR-J40-S01 checks it
// against ledger_transfers_type_check).
var AllTransferTypes = []TransferType{Deposit, DepositBonus, CreditPurchaseRevenue, CreditRefund, CreditRefundRestore, CreditRefundCash, CreditRefundCashRestore, CreditRefundFunding, CreditSpend, CreditExpire, CreditRevoke, CreditReinstate, OwedAccrual, OwedPayment, OwedWriteoff, OwedRepayment, OwedReopened, UsageReversed}

// fundingTransfers re-arm a customer's balance notices: money added to its
// balance, and debt it paid.
var fundingTransfers = map[TransferType]bool{Deposit: true, DepositBonus: true, CreditReinstate: true, OwedPayment: true}

// LotOnceTransferTypes are the at-most-once-per-lot movements enforced by
// ledger_transfers_credit_grant_id_transfer_type_key. A reinstated lot can be
// revoked again.
var LotOnceTransferTypes = []TransferType{Deposit, DepositBonus, CreditPurchaseRevenue, CreditExpire}

// Operation is the kind of money write that posted a transfer, part of the
// idempotency coordinate. The engine composes it and a caller supplies only
// (source, source_id), so two operations (an overage charge and the capture of
// the same request) never alias on one caller key.
type Operation = books.Operation

const (
	OpSpend    Operation = "spend"    // history only
	OpCapture  Operation = "capture"  // elapsed running-charge spend
	OpWithdraw Operation = "withdraw" // history only
	OpDeposit  Operation = "deposit"  // credit grant / top-up

	// OpInvoice is an invoice line in a merchant's unit, paid from the balance,
	// under the name its earlier transfers carry.
	OpInvoice Operation = "order"

	OpCreditExpire    Operation = "credit_expire"
	OpCreditRevoke    Operation = "credit_revoke"
	OpCreditReinstate Operation = "credit_reinstate"

	OpArrearsAccrual Operation = "arrears_accrual" // MoneyService.AccrueOwed
	OpUsagePosting   Operation = "usage_posting"   // one posting of a customer's usage in a period
	// A usage invoice's owed payment and write-off.
	OpInvoicePayment       Operation = "invoice_payment" // arrears settled by a rail charge
	OpManualInvoicePayment Operation = "manual_invoice_payment"
	OpInvoiceVoid          Operation = "invoice_void"

	usageOpPrefix = "usage:"
)

// UsageOperation is the operation kind of a charged failed usage item's legs,
// at its lease and number.
func UsageOperation(eventType string) Operation {
	return Operation(usageOpPrefix + strings.TrimSpace(eventType))
}

// Coord is the idempotency coordinate a durable money write posts at, within
// (merchant, customer, currency). All three parts are required;
// money.NewIdempotencyKey builds one for callers.
type Coord = books.Coord

// MaxCoordBytes bounds each part of a coordinate; ledger_transfers checks it.
const MaxCoordBytes = books.MaxCoordBytes

// ErrMerchantUnitOwed refuses debt in a merchant's own unit: its units are
// prepaid, never owed.
var ErrMerchantUnitOwed = errors.New("ledger: nothing is owed in a merchant's unit")

// ErrUndeclaredUnit refuses an account in a unit the merchant does not
// declare.
var ErrUndeclaredUnit = errors.New("ledger: the merchant does not declare the unit")

// UnitsLock is the transaction lock key the units book takes when it first
// counts a merchant's unit, and a catalog write or settings change that could
// remove or rescale one takes, so neither sees the other half done.
func UnitsLock(merchant uuid.UUID) string { return "merchant-units:" + merchant.String() }

// ErrInsufficientFunds is returned when a posting transfer would push the debit
// account below its sign-constraint floor.
var ErrInsufficientFunds = errors.New("ledger: transfer breaches the debit account's sign constraint")

// Ledger applies transfers and derives balances for one merchant. It operates
// over a gen.Queries bound to a (merchant-scoped) connection or transaction;
// compose it inside a pgx tx for atomic multi-transfer operations.
type Ledger struct {
	q        *gen.Queries
	merchant uuid.UUID
	now      func() time.Time
}

// New binds a Ledger to a query handle and the merchant whose ledgers it serves.
func New(q *gen.Queries, merchant uuid.UUID) *Ledger {
	return &Ledger{q: q, merchant: merchant, now: func() time.Time { return time.Now().UTC() }}
}

// SetClock sets the time a balance watch reads holds at; nil keeps the real
// clock.
func (l *Ledger) SetClock(now func() time.Time) *Ledger {
	if now != nil {
		l.now = now
	}
	return l
}

// EnsureSystemAccount get-or-creates the merchant's system account of the given
// type + currency (customer_id NULL).
func (l *Ledger) EnsureSystemAccount(ctx context.Context, t AccountType, currency string) (uuid.UUID, error) {
	id, _, err := l.ensureAccount(ctx, t, currency, nil)
	return id, err
}

// EnsureCustomerBalance get-or-creates a customer's balance account, flagged
// debits_must_not_exceed_credits so it cannot be overdrawn beyond an
// applier-supplied arrears floor. A new account of a customer not billed in
// arrears is watched.
func (l *Ledger) EnsureCustomerBalance(ctx context.Context, customer uuid.UUID, currency string) (uuid.UUID, error) {
	c := customer
	id, created, err := l.ensureAccount(ctx, CustomerUnits, currency, &c)
	if err != nil || !created {
		return id, err
	}
	if err := l.q.InsertPrepaidBalanceWatch(ctx, gen.InsertPrepaidBalanceWatchParams{MerchantID: l.merchant, CustomerID: customer, Currency: currency}); err != nil {
		return uuid.Nil, fmt.Errorf("ledger: watch the balance: %w", err)
	}
	return id, nil
}

// EnsureCustomerArrears get-or-creates the customer's own owed account, so
// outstanding owed is one counter read, not a sum over history. Not
// debits_must_not_exceed_credits: its negative balance IS the debt.
func (l *Ledger) EnsureCustomerArrears(ctx context.Context, customer uuid.UUID, currency string) (uuid.UUID, error) {
	if unit.IsUnitCode(currency) {
		return uuid.Nil, fmt.Errorf("%w: %s", ErrMerchantUnitOwed, currency)
	}
	c := customer
	id, _, err := l.ensureAccount(ctx, CustomerOwed, currency, &c)
	return id, err
}

// CustomerArrearsAccountID returns the customer's arrears account id, and false
// when it does not exist. Read-only: an exposure read never creates an account.
func (l *Ledger) CustomerArrearsAccountID(ctx context.Context, customer uuid.UUID, currency string) (uuid.UUID, bool, error) {
	c := customer
	acc, err := l.q.GetLedgerAccount(ctx, gen.GetLedgerAccountParams{
		MerchantID: l.merchant, AccountType: string(CustomerOwed), Currency: currency, CustomerID: &c,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, fmt.Errorf("ledger: get arrears account: %w", err)
	}
	return acc.ID, true, nil
}

// OutstandingOwed is the customer's unpaid arrears in this currency as a
// positive amount, read O(1) from the arrears account's counters (debt makes
// that balance negative). Zero without an arrears account. It is the only
// exposure source: usage invoices lag the ledger and only mirror its accruals.
func (l *Ledger) OutstandingOwed(ctx context.Context, customer uuid.UUID, currency string) (int64, error) {
	acc, found, err := l.CustomerArrearsAccountID(ctx, customer, currency)
	if err != nil || !found {
		return 0, err
	}
	bal, err := l.Balance(ctx, acc)
	if err != nil {
		return 0, err
	}
	if bal >= 0 {
		return 0, nil
	}
	return -bal, nil
}

// CustomerBalanceAccountID returns the customer's balance account id, and false
// when it does not exist yet. Read-only: unlike EnsureCustomerBalance it never
// creates the account, so reading an unknown customer gives a clean zero.
func (l *Ledger) CustomerBalanceAccountID(ctx context.Context, customer uuid.UUID, currency string) (uuid.UUID, bool, error) {
	c := customer
	acc, err := l.q.GetLedgerAccount(ctx, gen.GetLedgerAccountParams{
		MerchantID: l.merchant, AccountType: string(CustomerUnits), Currency: currency, CustomerID: &c,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, fmt.Errorf("ledger: get %s account: %w", CustomerUnits, err)
	}
	return acc.ID, true, nil
}

// ensureAccount get-or-creates a units-book account with the chart's sign
// rules; created reports this call made it.
func (l *Ledger) ensureAccount(ctx context.Context, t AccountType, currency string, customer *uuid.UUID) (id uuid.UUID, created bool, err error) {
	if acct, ok := books.Lookup(t); !ok || acct.Book != books.Units {
		return uuid.Nil, false, fmt.Errorf("ledger: %q is not a units-book account", t)
	}
	if err := l.CountUnits(ctx, currency); err != nil {
		return uuid.Nil, false, err
	}
	return books.EnsureAccount(ctx, l.q, l.merchant, t, currency, customer, nil)
}

// CountUnits records each of codes that is one of the merchant's units and
// the book has not counted yet (ledger_units), at the scale the merchant's
// document in its file or Vault declares now, read past any replica's cache,
// under UnitsLock: a settings change that would remove or rescale it either
// commits first, and this reads its result, or sees the record. A path that
// may count a unit first calls it before it locks any customer, so the units
// lock always comes first.
func (l *Ledger) CountUnits(ctx context.Context, codes ...string) error {
	var missing []string
	for _, code := range codes {
		if !unit.IsUnitCode(code) || slices.Contains(missing, code) {
			continue
		}
		if _, err := l.q.GetLedgerUnitScale(ctx, gen.GetLedgerUnitScaleParams{MerchantID: l.merchant, Unit: code}); errors.Is(err, pgx.ErrNoRows) {
			missing = append(missing, code)
		} else if err != nil {
			return err
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if err := l.q.LockCatalogKey(ctx, UnitsLock(l.merchant)); err != nil {
		return err
	}
	config := db.MerchantConfigOf(l.q)
	if config == nil {
		return fmt.Errorf("%w: %s (no merchant configuration is bound)", ErrUndeclaredUnit, strings.Join(missing, ", "))
	}
	declared, err := config.StoredCurrencies(ctx, billing.MerchantID(l.merchant))
	if err != nil {
		return fmt.Errorf("ledger: read the merchant's units: %w", err)
	}
	for _, code := range missing {
		i := slices.IndexFunc(declared, func(u billing.MerchantCurrency) bool { return strings.EqualFold(string(u.Code), code) })
		if i < 0 || declared[i].Scale < 0 || declared[i].Scale > unit.MaxUnitScale {
			return fmt.Errorf("%w: %s", ErrUndeclaredUnit, code)
		}
		if err := l.q.InsertLedgerUnit(ctx, gen.InsertLedgerUnitParams{MerchantID: l.merchant, Unit: code, Scale: int32(declared[i].Scale)}); err != nil {
			return err
		}
	}
	return nil
}

// Transfer is one double-entry movement to append.
type Transfer struct {
	Debit, Credit uuid.UUID
	Amount        int64
	Currency      string
	Type          TransferType
	// Coord is the operation coordinate this leg is idempotent on. Required.
	Coord Coord
	// CreditGrantID names the credit lot the leg moves units of or
	// attributes money to, independently of Coord.
	CreditGrantID     *uuid.UUID
	Customer          *uuid.UUID
	Invoker, Resource *string
	Invoice           *uuid.UUID
}

// Apply is ApplyIdempotent for callers that don't need the applied flag: it
// appends one posted transfer, enforcing the debit account's sign constraint.
func (l *Ledger) Apply(ctx context.Context, t Transfer) (gen.BillingLedgerTransfer, error) {
	tr, _, err := l.ApplyIdempotent(ctx, t)
	return tr, err
}

// ApplyIdempotent is the durable money write every ledger movement funnels
// through. The database enforces once-only: ON CONFLICT DO NOTHING on
// ledger_transfers_operation_once_key, whatever the caller's lock order.
//
// applied is true when this call posted the transfer and moved the counters;
// false when the coordinate was already committed: nothing moved and the
// returned row is the transfer that landed. A replay never rechecks or moves
// money, even if the original depleted the balance.
func (l *Ledger) ApplyIdempotent(ctx context.Context, t Transfer) (tr gen.BillingLedgerTransfer, applied bool, err error) {
	// The coordinate is validated HERE, at the one insert every money movement
	// funnels through, so no new spend path can post an unkeyed or ambiguous leg.
	if err := t.Coord.Validate(); err != nil {
		return gen.BillingLedgerTransfer{}, false, err
	}
	tr, err = l.q.InsertLedgerTransfer(ctx, gen.InsertLedgerTransferParams{
		MerchantID:      l.merchant,
		DebitAccountID:  t.Debit,
		CreditAccountID: t.Credit,
		Amount:          t.Amount,
		Currency:        t.Currency,
		TransferType:    string(t.Type),
		Operation:       string(t.Coord.Operation),
		Source:          t.Coord.Source,
		SourceID:        t.Coord.SourceID,
		CreditGrantID:   t.CreditGrantID,
		CustomerID:      t.Customer,
		InvokerID:       t.Invoker,
		Resource:        t.Resource,
		InvoiceID:       t.Invoice,
	})
	if err == nil {
		// A transfer that names a customer changes what it may spend: one
		// that crosses a line of its balance watch queues a look, and a
		// funding re-arms its notices.
		if t.Customer != nil {
			if err := WatchChange(ctx, l.q, l.merchant, *t.Customer, t.Currency, fundingTransfers[t.Type], l.now()); err != nil {
				return gen.BillingLedgerTransfer{}, false, fmt.Errorf("ledger: balance watch: %w", err)
			}
		}
		return tr, true, nil
	}
	// Zero rows = ON CONFLICT DO NOTHING fired. Read the committed receipt;
	// the database, not the caller's lock order, enforces once-only posting.
	if errors.Is(err, pgx.ErrNoRows) {
		existing, found, gerr := l.transferAt(ctx, t)
		if gerr != nil {
			return gen.BillingLedgerTransfer{}, false, gerr
		}
		if !found {
			return gen.BillingLedgerTransfer{}, false, fmt.Errorf(
				"ledger: insert at %s conflicted but no committed row is visible", t.Coord)
		}
		return existing, false, nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && strings.Contains(pgErr.Message, "ledger_insufficient_funds") {
		return gen.BillingLedgerTransfer{}, false, fmt.Errorf("%w: %s", ErrInsufficientFunds, pgErr.Message)
	}
	return gen.BillingLedgerTransfer{}, false, err
}

// transferAt resolves the units-book row already committed at a transfer's
// full physical identity (coordinate + lot), if any.
func (l *Ledger) transferAt(ctx context.Context, t Transfer) (gen.BillingLedgerTransfer, bool, error) {
	row, err := l.q.GetLedgerTransferAtCoordinate(ctx, gen.GetLedgerTransferAtCoordinateParams{
		MerchantID:    l.merchant,
		Book:          string(books.Units),
		CustomerID:    t.Customer,
		Currency:      t.Currency,
		TransferType:  string(t.Type),
		Operation:     string(t.Coord.Operation),
		Source:        t.Coord.Source,
		SourceID:      t.Coord.SourceID,
		CreditGrantID: t.CreditGrantID,
	})
	if err == nil {
		return row, true, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.BillingLedgerTransfer{}, false, nil
	}
	return gen.BillingLedgerTransfer{}, false, fmt.Errorf("ledger: resolve transfer at %s: %w", t.Coord, err)
}

// Balance returns the account's maintained balance counter
// (credits_posted - debits_posted).
func (l *Ledger) Balance(ctx context.Context, account uuid.UUID) (int64, error) {
	return l.q.LedgerAccountBalance(ctx, gen.LedgerAccountBalanceParams{AccountID: account, MerchantID: l.merchant})
}

// DepositFrom credits the customer's balance from units_sold (a lot bought)
// or units_granted (a lot given). grantID attributes the deposit to its
// credit lot (uuid.Nil for a non-lot deposit).
func (l *Ledger) DepositFrom(ctx context.Context, from AccountType, customer uuid.UUID, currency string, amount int64, coord Coord, grantID uuid.UUID) (gen.BillingLedgerTransfer, error) {
	if from != UnitsSold && from != UnitsGranted {
		return gen.BillingLedgerTransfer{}, fmt.Errorf("ledger: a deposit's units are sold or granted, not %s", from)
	}
	clearing, err := l.EnsureSystemAccount(ctx, from, currency)
	if err != nil {
		return gen.BillingLedgerTransfer{}, err
	}
	cust, err := l.EnsureCustomerBalance(ctx, customer, currency)
	if err != nil {
		return gen.BillingLedgerTransfer{}, err
	}
	c := customer
	t := Transfer{
		Debit: clearing, Credit: cust, Amount: amount, Currency: currency, Type: Deposit,
		Coord: coord, Customer: &c,
	}
	if grantID != uuid.Nil {
		g := grantID
		t.CreditGrantID = &g
	}
	return l.Apply(ctx, t)
}

// ReverseUsage credits a customer's lot with what its usage posted beyond a
// period's price (DR units_redeemed / CR customer_units).
func (l *Ledger) ReverseUsage(ctx context.Context, customer uuid.UUID, currency string, amount int64, coord Coord, lot uuid.UUID) (gen.BillingLedgerTransfer, error) {
	rev, err := l.EnsureSystemAccount(ctx, UnitsRedeemed, currency)
	if err != nil {
		return gen.BillingLedgerTransfer{}, err
	}
	cust, err := l.EnsureCustomerBalance(ctx, customer, currency)
	if err != nil {
		return gen.BillingLedgerTransfer{}, err
	}
	c := customer
	return l.Apply(ctx, Transfer{Debit: rev, Credit: cust, Amount: amount, Currency: currency, Type: UsageReversed, Coord: coord, Customer: &c, CreditGrantID: &lot})
}

// AccrueOwed recognizes postpaid usage as revenue against the customer's
// arrears account (DR customer_owed / CR units_redeemed), leaving the
// balance untouched; PayOwed settles it. The arrears account's negative
// balance is the owed exposure.
func (l *Ledger) AccrueOwed(ctx context.Context, customer uuid.UUID, currency string, amount int64, coord Coord, invoice *uuid.UUID) (gen.BillingLedgerTransfer, error) {
	tr, _, err := l.AccrueOwedIdempotent(ctx, customer, currency, amount, coord, invoice)
	return tr, err
}

// AccrueOwedIdempotent is AccrueOwed reporting whether the accrual actually
// posted, or replayed a coordinate already committed.
func (l *Ledger) AccrueOwedIdempotent(ctx context.Context, customer uuid.UUID, currency string, amount int64, coord Coord, invoice *uuid.UUID) (gen.BillingLedgerTransfer, bool, error) {
	liab, err := l.EnsureCustomerArrears(ctx, customer, currency)
	if err != nil {
		return gen.BillingLedgerTransfer{}, false, err
	}
	rev, err := l.EnsureSystemAccount(ctx, UnitsRedeemed, currency)
	if err != nil {
		return gen.BillingLedgerTransfer{}, false, err
	}
	c := customer
	return l.ApplyIdempotent(ctx, Transfer{
		Debit: liab, Credit: rev, Amount: amount, Currency: currency, Type: OwedAccrual,
		Coord: coord, Customer: &c, Invoice: invoice,
	})
}

// WriteOffOwed cancels accrued arrears without money moving (DR
// units_redeemed / CR customer_owed), the inverse of AccrueOwed. Posted
// when a usage invoice is voided, so the ledger stops counting it as owed.
func (l *Ledger) WriteOffOwed(ctx context.Context, customer uuid.UUID, currency string, amount int64, coord Coord, invoice *uuid.UUID) (gen.BillingLedgerTransfer, error) {
	rev, err := l.EnsureSystemAccount(ctx, UnitsRedeemed, currency)
	if err != nil {
		return gen.BillingLedgerTransfer{}, err
	}
	liab, err := l.EnsureCustomerArrears(ctx, customer, currency)
	if err != nil {
		return gen.BillingLedgerTransfer{}, err
	}
	c := customer
	return l.Apply(ctx, Transfer{
		Debit: rev, Credit: liab, Amount: amount, Currency: currency, Type: OwedWriteoff,
		Coord: coord, Customer: &c, Invoice: invoice,
	})
}

// PayOwed settles accrued arrears via an external charge (DR units_sold /
// CR customer_owed), bringing the liability account back toward zero.
func (l *Ledger) PayOwed(ctx context.Context, customer uuid.UUID, currency string, amount int64, coord Coord, invoice *uuid.UUID) (gen.BillingLedgerTransfer, error) {
	clearing, err := l.EnsureSystemAccount(ctx, UnitsSold, currency)
	if err != nil {
		return gen.BillingLedgerTransfer{}, err
	}
	liab, err := l.EnsureCustomerArrears(ctx, customer, currency)
	if err != nil {
		return gen.BillingLedgerTransfer{}, err
	}
	c := customer
	return l.Apply(ctx, Transfer{
		Debit: clearing, Credit: liab, Amount: amount, Currency: currency, Type: OwedPayment,
		Coord: coord, Customer: &c, Invoice: invoice,
	})
}

// ReopenOwed owes again what a charged-back usage invoice payment paid (DR
// customer_owed / CR units_sold).
func (l *Ledger) ReopenOwed(ctx context.Context, customer uuid.UUID, currency string, amount int64, coord Coord, invoice *uuid.UUID) (gen.BillingLedgerTransfer, error) {
	liab, err := l.EnsureCustomerArrears(ctx, customer, currency)
	if err != nil {
		return gen.BillingLedgerTransfer{}, err
	}
	sold, err := l.EnsureSystemAccount(ctx, UnitsSold, currency)
	if err != nil {
		return gen.BillingLedgerTransfer{}, err
	}
	c := customer
	return l.Apply(ctx, Transfer{
		Debit: liab, Credit: sold, Amount: amount, Currency: currency, Type: OwedReopened,
		Coord: coord, Customer: &c, Invoice: invoice,
	})
}

// RepayOwed pays a customer's debt from their balance (DR customer_units /
// CR customer_owed), attributed to the funding credit lot.
func (l *Ledger) RepayOwed(ctx context.Context, customer uuid.UUID, currency string, amount int64, coord Coord, lot uuid.UUID, invoice *uuid.UUID) (gen.BillingLedgerTransfer, error) {
	cust, err := l.EnsureCustomerBalance(ctx, customer, currency)
	if err != nil {
		return gen.BillingLedgerTransfer{}, err
	}
	liab, err := l.EnsureCustomerArrears(ctx, customer, currency)
	if err != nil {
		return gen.BillingLedgerTransfer{}, err
	}
	c := customer
	return l.Apply(ctx, Transfer{
		Debit: cust, Credit: liab, Amount: amount, Currency: currency, Type: OwedRepayment,
		Coord: coord, CreditGrantID: &lot, Customer: &c, Invoice: invoice,
	})
}
