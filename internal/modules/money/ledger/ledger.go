// Package ledger is the double-entry, append-only money ledger over
// ledger_accounts / ledger_transfers. A ledger is a (merchant, currency) pair;
// transfers move an amount debit->credit within one ledger and are immutable.
// Account counters hold balances; transfers are the truth. Every transfer is
// posted: holds live outside the ledger.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/open-rails/openrails/internal/db/gen"
)

// AccountType identifies an account's role within a (merchant, currency) ledger.
type AccountType string

const (
	CustomerBalance  AccountType = "customer_balance"
	PlatformRevenue  AccountType = "platform_revenue"
	RailClearing     AccountType = "processor_clearing"
	ArrearsLiability AccountType = "arrears_liability"
	ExpiredCredits   AccountType = "expired_credits"
	// RevokedCredits holds the unspent remainder clawed back when a credit grant
	// is revoked (ExpiredCredits is time-lapse): frozen and reversible, not
	// refunded; a refund moves it out to RailClearing.
	RevokedCredits       AccountType = "revoked_credits"
	PromotionalFunding   AccountType = "promotional_funding"
	CreditRefundClearing AccountType = "credit_refund_clearing"
	CreditRefundLoss     AccountType = "credit_refund_loss"
)

// TransferType is the closed vocabulary of ledger_transfers.transfer_type,
// mirrored by ledger_transfers_type_check. The lot-once index
// ledger_transfers_grant_id_transfer_type_key is partial on these literals, so
// a typo would escape it and post a duplicate.
type TransferType string

const (
	DepositBonus            TransferType = "deposit_bonus" // merchant-funded credit, not processor cash
	CreditPurchaseRevenue   TransferType = "credit_purchase_revenue"
	CreditRefund            TransferType = "credit_refund"
	CreditRefundRestore     TransferType = "credit_refund_restore"
	CreditRefundCash        TransferType = "credit_refund_cash"
	CreditRefundCashRestore TransferType = "credit_refund_cash_restore"
	CreditRefundFunding     TransferType = "credit_refund_funding"
	Deposit                 TransferType = "deposit"       // rail clearing -> customer balance
	CreditSpend             TransferType = "credit_spend"  // customer balance -> platform revenue
	CreditExpire            TransferType = "credit_expire" // unspent lot remainder, time-lapsed
	CreditRevoke            TransferType = "credit_revoke" // unspent lot remainder, clawed back
	// CreditReinstate reverses a clawback (revoked_credits -> customer_balance).
	CreditReinstate TransferType = "credit_reinstate"
	OwedAccrual     TransferType = "owed_accrual" // postpaid usage -> arrears liability
	OwedPayment     TransferType = "owed_payment" // arrears settled by an external charge
	// OwedWriteoff cancels accrued debt without money moving when an invoice is
	// voided: the inverse of OwedAccrual. OwedPayment means a rail collected.
	OwedWriteoff TransferType = "owed_writeoff"
	// OwedRepayment pays debt from a newly funded credit lot (customer balance
	// -> arrears liability), carrying that lot's grant_id.
	OwedRepayment TransferType = "owed_repayment"
)

// AllTransferTypes must equal the DB CHECK exactly (TestLedgerVocabularyMatchesSchema).
var AllTransferTypes = []TransferType{Deposit, DepositBonus, CreditPurchaseRevenue, CreditRefund, CreditRefundRestore, CreditRefundCash, CreditRefundCashRestore, CreditRefundFunding, CreditSpend, CreditExpire, CreditRevoke, CreditReinstate, OwedAccrual, OwedPayment, OwedWriteoff, OwedRepayment}

// LotOnceTransferTypes are the at-most-once-per-lot movements enforced by
// ledger_transfers_grant_id_transfer_type_key.
var LotOnceTransferTypes = []TransferType{Deposit, DepositBonus, CreditPurchaseRevenue, CreditExpire, CreditRevoke}

// Operation is the kind of money write that posted a transfer, part of the
// idempotency coordinate. The engine composes it and a caller supplies only
// (source, source_id), so two operations (an overage charge and the capture of
// the same request) never alias on one caller key.
type Operation string

const (
	OpSpend    Operation = "spend"    // MoneyService.SpendCredits
	OpCapture  Operation = "capture"  // MoneyService.CaptureAuthorized
	OpWithdraw Operation = "withdraw" // MoneyService.Withdraw
	OpDeposit  Operation = "deposit"  // credit grant / top-up

	OpCreditExpire    Operation = "credit_expire"
	OpCreditRevoke    Operation = "credit_revoke"
	OpCreditReinstate Operation = "credit_reinstate"

	OpArrearsAccrual   Operation = "arrears_accrual" // MoneyService.AccrueOwed
	OpMeteredRating    Operation = "metered_rating"  // rate-card sweep accrual
	OpInvoicePayment   Operation = "invoice_payment" // arrears settled by a rail charge
	OpManualInvoicePay Operation = "manual_invoice_payment"
	OpInvoiceVoid      Operation = "invoice_void"

	usageOpPrefix = "usage:"
)

// UsageOperation is the operation kind of a metered usage charge. event_type is
// part of the kind because usage_events already dedupes on it
// (usage_events_idem_key): two different event types at one (source, source_id)
// are two events, so they must be two ledger legs.
func UsageOperation(eventType string) Operation {
	return Operation(usageOpPrefix + strings.TrimSpace(eventType))
}

// Coord is the idempotency coordinate a durable money write posts at, within
// (merchant, customer, currency). All three parts are required;
// money.NewIdempotencyKey builds one for callers.
type Coord struct {
	Operation Operation
	Source    string
	SourceID  string
}

// Validate refuses a partial coordinate: a blank part makes a money write
// non-idempotent or ambiguous.
func (c Coord) Validate() error {
	if strings.TrimSpace(string(c.Operation)) == "" || c.Operation == usageOpPrefix {
		return fmt.Errorf("ledger: operation required on the idempotency coordinate")
	}
	if strings.TrimSpace(c.Source) == "" || strings.TrimSpace(c.SourceID) == "" {
		return fmt.Errorf("ledger: source and source_id required on the idempotency coordinate")
	}
	if len(c.Operation) > MaxCoordBytes || len(c.Source) > MaxCoordBytes || len(c.SourceID) > MaxCoordBytes {
		return fmt.Errorf("ledger: each part of the idempotency coordinate is at most %d bytes", MaxCoordBytes)
	}
	return nil
}

// MaxCoordBytes bounds each part of a coordinate; ledger_transfers checks it.
const MaxCoordBytes = 512

func (c Coord) String() string {
	return string(c.Operation) + "/" + c.Source + "/" + c.SourceID
}

// ErrInsufficientFunds is returned when a posting transfer would push the debit
// account below its sign-constraint floor.
var ErrInsufficientFunds = errors.New("ledger: transfer breaches the debit account's sign constraint")

// Ledger applies transfers and derives balances for one merchant. It operates
// over a gen.Queries bound to a (merchant-scoped) connection or transaction;
// compose it inside a pgx tx for atomic multi-transfer operations.
type Ledger struct {
	q        *gen.Queries
	merchant uuid.UUID
}

// New binds a Ledger to a query handle and the merchant whose ledgers it serves.
func New(q *gen.Queries, merchant uuid.UUID) *Ledger {
	return &Ledger{q: q, merchant: merchant}
}

// EnsureSystemAccount get-or-creates the merchant's system account of the given
// type + currency (customer_id NULL).
func (l *Ledger) EnsureSystemAccount(ctx context.Context, t AccountType, currency string) (uuid.UUID, error) {
	return l.ensureAccount(ctx, t, currency, nil, false, false)
}

// EnsureCustomerBalance get-or-creates a customer's balance account, flagged
// debits_must_not_exceed_credits so it cannot be overdrawn beyond an
// applier-supplied arrears floor.
func (l *Ledger) EnsureCustomerBalance(ctx context.Context, customer uuid.UUID, currency string) (uuid.UUID, error) {
	c := customer
	return l.ensureAccount(ctx, CustomerBalance, currency, &c, true, false)
}

// EnsureCustomerArrears get-or-creates the customer's own arrears-liability
// account, so outstanding owed is one counter read, not a sum over history.
// Not debits_must_not_exceed_credits: its negative balance IS the debt.
func (l *Ledger) EnsureCustomerArrears(ctx context.Context, customer uuid.UUID, currency string) (uuid.UUID, error) {
	c := customer
	return l.ensureAccount(ctx, ArrearsLiability, currency, &c, false, false)
}

// CustomerArrearsAccountID returns the customer's arrears account id, and false
// when it does not exist. Read-only: an exposure read never creates an account.
func (l *Ledger) CustomerArrearsAccountID(ctx context.Context, customer uuid.UUID, currency string) (uuid.UUID, bool, error) {
	c := customer
	acc, err := l.q.GetLedgerAccount(ctx, gen.GetLedgerAccountParams{
		MerchantID: l.merchant, AccountType: string(ArrearsLiability), Currency: currency, CustomerID: &c,
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
// exposure source: invoices lag the ledger and only mirror its accruals.
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
		MerchantID: l.merchant, AccountType: string(CustomerBalance), Currency: currency, CustomerID: &c,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, fmt.Errorf("ledger: get customer_balance account: %w", err)
	}
	return acc.ID, true, nil
}

func (l *Ledger) ensureAccount(ctx context.Context, t AccountType, currency string, customer *uuid.UUID, dmnec, cmned bool) (uuid.UUID, error) {
	get := gen.GetLedgerAccountParams{MerchantID: l.merchant, AccountType: string(t), Currency: currency, CustomerID: customer}
	acc, err := l.q.GetLedgerAccount(ctx, get)
	if err == nil {
		return acc.ID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("ledger: get %s account: %w", t, err)
	}
	created, err := l.q.InsertLedgerAccount(ctx, gen.InsertLedgerAccountParams{
		MerchantID: l.merchant, CustomerID: customer, AccountType: string(t), Currency: currency,
		DebitsMustNotExceedCredits: dmnec, CreditsMustNotExceedDebits: cmned,
	})
	if err != nil {
		// Lost a create race (unique index): re-read.
		if acc2, gerr := l.q.GetLedgerAccount(ctx, get); gerr == nil {
			return acc2.ID, nil
		}
		return uuid.Nil, fmt.Errorf("ledger: create %s account: %w", t, err)
	}
	return created.ID, nil
}

// Transfer is one double-entry movement to append.
type Transfer struct {
	Debit, Credit uuid.UUID
	Amount        int64
	Currency      string
	Type          TransferType
	// Coord is the operation coordinate this leg is idempotent on. Required.
	Coord Coord
	// GrantID attributes a credit_spend/credit_expire/deposit to its credit
	// lot, independently of Coord.
	GrantID           *uuid.UUID
	Customer          *uuid.UUID
	Invoker, Resource *string
	Invoice           *uuid.UUID
	// AllowDebitNegativeUpTo relaxes the debit account's
	// debits_must_not_exceed_credits floor (e.g. an arrears credit line).
	AllowDebitNegativeUpTo int64
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
		MerchantID:             l.merchant,
		DebitAccountID:         t.Debit,
		CreditAccountID:        t.Credit,
		Amount:                 t.Amount,
		Currency:               t.Currency,
		TransferType:           string(t.Type),
		AllowDebitNegativeUpTo: t.AllowDebitNegativeUpTo,
		Operation:              string(t.Coord.Operation),
		Source:                 t.Coord.Source,
		SourceID:               t.Coord.SourceID,
		GrantID:                t.GrantID,
		CustomerID:             t.Customer,
		InvokerID:              t.Invoker,
		Resource:               t.Resource,
		InvoiceID:              t.Invoice,
	})
	if err == nil {
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

// transferAt resolves the row already committed at a transfer's full physical
// identity (coordinate + lot), if any.
func (l *Ledger) transferAt(ctx context.Context, t Transfer) (gen.BillingLedgerTransfer, bool, error) {
	row, err := l.q.GetLedgerTransferAtCoordinate(ctx, gen.GetLedgerTransferAtCoordinateParams{
		MerchantID:   l.merchant,
		CustomerID:   t.Customer,
		Currency:     t.Currency,
		TransferType: string(t.Type),
		Operation:    string(t.Coord.Operation),
		Source:       t.Coord.Source,
		SourceID:     t.Coord.SourceID,
		GrantID:      t.GrantID,
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

// Deposit credits the customer's balance from the rail-clearing account
// (DR processor_clearing / CR customer_balance). grantID attributes the deposit
// to its credit lot (uuid.Nil for a non-lot deposit).
func (l *Ledger) Deposit(ctx context.Context, customer uuid.UUID, currency string, amount int64, coord Coord, grantID uuid.UUID) (gen.BillingLedgerTransfer, error) {
	clearing, err := l.EnsureSystemAccount(ctx, RailClearing, currency)
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
		t.GrantID = &g
	}
	return l.Apply(ctx, t)
}

// AccrueOwed recognizes postpaid usage as revenue against the customer's
// arrears account (DR arrears_liability / CR platform_revenue), leaving the
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
	rev, err := l.EnsureSystemAccount(ctx, PlatformRevenue, currency)
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
// platform_revenue / CR arrears_liability), the inverse of AccrueOwed. Posted
// when an invoice is voided, so the ledger stops counting it as owed.
func (l *Ledger) WriteOffOwed(ctx context.Context, customer uuid.UUID, currency string, amount int64, coord Coord, invoice *uuid.UUID) (gen.BillingLedgerTransfer, error) {
	rev, err := l.EnsureSystemAccount(ctx, PlatformRevenue, currency)
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

// PayOwed settles accrued arrears via an external charge (DR processor_clearing /
// CR arrears_liability), bringing the liability account back toward zero.
func (l *Ledger) PayOwed(ctx context.Context, customer uuid.UUID, currency string, amount int64, coord Coord, invoice *uuid.UUID) (gen.BillingLedgerTransfer, error) {
	clearing, err := l.EnsureSystemAccount(ctx, RailClearing, currency)
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

// RepayOwed pays a customer's debt from their balance (DR customer_balance /
// CR arrears_liability), attributed to the funding credit lot.
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
		Coord: coord, GrantID: &lot, Customer: &c, Invoice: invoice,
	})
}
