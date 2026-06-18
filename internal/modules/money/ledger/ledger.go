// Package ledger is the #512 double-entry, append-only money ledger expressed
// over the sqlc-generated ledger_accounts / ledger_transfers tables.
//
// A ledger is a (merchant, currency) pair. Accounts belong to one ledger;
// transfers move an amount debit->credit within one ledger and are immutable
// (the openrails_app role is granted SELECT,INSERT only). Balances are maintained
// on account counters, with transfers as the immutable truth. Two-phase transfers
// (Authorize -> Capture/Release) model real money movement; the throwaway
// admission hold lives in Redis (#513), not here.
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
	CustomerBalance   AccountType = "customer_balance"
	PlatformRevenue   AccountType = "platform_revenue"
	ProcessorClearing AccountType = "processor_clearing"
	ArrearsLiability  AccountType = "arrears_liability"
	ExpiredCredits    AccountType = "expired_credits"
	// RevokedCredits holds the unspent remainder clawed back when a credit grant
	// is revoked (distinct from ExpiredCredits = time-lapse). The money is frozen
	// here (recoverable/reversible), not refunded; a refund moves it out to
	// ProcessorClearing. (#514, see docs/consistency-invariants.md §11 decision 4.)
	RevokedCredits AccountType = "revoked_credits"
	FXLiquidity    AccountType = "fx_liquidity"
	World          AccountType = "world"
)

// Phase is a transfer's two-phase state.
type Phase string

const (
	// Posted is a single-phase transfer; it counts in the balance immediately.
	Posted Phase = "posted"
	// Pending is a two-phase hold; it counts in held (not balance) until resolved.
	Pending Phase = "pending"
	// PostPending resolves a pending by posting (up to) its amount.
	PostPending Phase = "post_pending"
	// VoidPending resolves a pending by cancelling it (no balance effect).
	VoidPending Phase = "void_pending"
)

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
	Type          string
	Phase         Phase      // default Posted
	PendingID     *uuid.UUID // set for PostPending / VoidPending
	// Opaque control-plane references (ledger purity — no business logic here).
	Source, SourceID *string
	// GrantID attributes a credit_spend/credit_expire/deposit to its #514 credit
	// lot, independently of Source/SourceID (which carry the OPERATION coordinate).
	GrantID           *uuid.UUID
	Customer          *uuid.UUID
	Invoker, Resource *string
	Invoice           *uuid.UUID
	// AllowDebitNegativeUpTo relaxes the debit account's
	// debits_must_not_exceed_credits floor (e.g. an arrears credit line).
	AllowDebitNegativeUpTo int64
}

// Apply appends one transfer, enforcing the debit account's sign constraint for
// transfers that post to the balance (posted / post_pending).
func (l *Ledger) Apply(ctx context.Context, t Transfer) (gen.OpenrailsLedgerTransfer, error) {
	phase := t.Phase
	if phase == "" {
		phase = Posted
	}
	if phase == Posted || phase == PostPending {
		if err := l.checkDebitFloor(ctx, t.Debit, t.Amount, t.AllowDebitNegativeUpTo); err != nil {
			return gen.OpenrailsLedgerTransfer{}, err
		}
	}
	tr, err := l.q.InsertLedgerTransfer(ctx, gen.InsertLedgerTransferParams{
		MerchantID:             l.merchant,
		DebitAccountID:         t.Debit,
		CreditAccountID:        t.Credit,
		Amount:                 t.Amount,
		Currency:               t.Currency,
		TransferType:           t.Type,
		Phase:                  string(phase),
		PendingID:              t.PendingID,
		AllowDebitNegativeUpTo: t.AllowDebitNegativeUpTo,
		Source:                 t.Source,
		SourceID:               t.SourceID,
		GrantID:                t.GrantID,
		CustomerID:             t.Customer,
		InvokerID:              t.Invoker,
		Resource:               t.Resource,
		InvoiceID:              t.Invoice,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && strings.Contains(pgErr.Message, "ledger_insufficient_funds") {
			return gen.OpenrailsLedgerTransfer{}, fmt.Errorf("%w: %s", ErrInsufficientFunds, pgErr.Message)
		}
		return gen.OpenrailsLedgerTransfer{}, err
	}
	return tr, nil
}

func (l *Ledger) checkDebitFloor(ctx context.Context, account uuid.UUID, amount, floor int64) error {
	acc, err := l.q.GetLedgerAccountByID(ctx, gen.GetLedgerAccountByIDParams{MerchantID: l.merchant, ID: account})
	if err != nil {
		return fmt.Errorf("ledger: load debit account: %w", err)
	}
	if !acc.DebitsMustNotExceedCredits {
		return nil
	}
	bal := acc.CreditsPosted - acc.DebitsPosted
	if bal-amount < -floor {
		return fmt.Errorf("%w (balance %d, amount %d, floor %d)", ErrInsufficientFunds, bal, amount, floor)
	}
	return nil
}

// Balance returns the account's maintained balance counter
// (credits_posted - debits_posted).
func (l *Ledger) Balance(ctx context.Context, account uuid.UUID) (int64, error) {
	return l.q.LedgerAccountBalance(ctx, gen.LedgerAccountBalanceParams{AccountID: account, MerchantID: l.merchant})
}

// Held returns the account's maintained unresolved-pending-debits counter.
func (l *Ledger) Held(ctx context.Context, account uuid.UUID) (int64, error) {
	return l.q.LedgerAccountHeld(ctx, gen.LedgerAccountHeldParams{MerchantID: l.merchant, AccountID: account})
}

// Available returns Balance - Held.
func (l *Ledger) Available(ctx context.Context, account uuid.UUID) (int64, error) {
	bal, err := l.Balance(ctx, account)
	if err != nil {
		return 0, err
	}
	held, err := l.Held(ctx, account)
	if err != nil {
		return 0, err
	}
	return bal - held, nil
}

// Conservation returns the sum of every account's balance in the (merchant,
// currency) ledger. Double-entry guarantees it is 0.
func (l *Ledger) Conservation(ctx context.Context, currency string) (int64, error) {
	return l.q.LedgerLedgerNet(ctx, gen.LedgerLedgerNetParams{MerchantID: l.merchant, Currency: currency})
}

// --- flow constructors: the standard money movements as transfer pairs --------

// Deposit credits the customer's balance from the processor-clearing account
// (DR processor_clearing / CR customer_balance). grantID attributes the deposit
// to its #514 credit lot (uuid.Nil for a non-lot deposit).
func (l *Ledger) Deposit(ctx context.Context, customer uuid.UUID, currency string, amount int64, source, sourceID string, grantID uuid.UUID) (gen.OpenrailsLedgerTransfer, error) {
	clearing, err := l.EnsureSystemAccount(ctx, ProcessorClearing, currency)
	if err != nil {
		return gen.OpenrailsLedgerTransfer{}, err
	}
	cust, err := l.EnsureCustomerBalance(ctx, customer, currency)
	if err != nil {
		return gen.OpenrailsLedgerTransfer{}, err
	}
	c := customer
	t := Transfer{
		Debit: clearing, Credit: cust, Amount: amount, Currency: currency, Type: "deposit",
		Source: &source, SourceID: &sourceID, Customer: &c,
	}
	if grantID != uuid.Nil {
		g := grantID
		t.GrantID = &g
	}
	return l.Apply(ctx, t)
}

// Spend debits the customer's balance into platform revenue
// (DR customer_balance / CR platform_revenue). floor is the arrears credit line.
func (l *Ledger) Spend(ctx context.Context, customer uuid.UUID, currency string, amount int64, invoker, resource string, floor int64) (gen.OpenrailsLedgerTransfer, error) {
	cust, err := l.EnsureCustomerBalance(ctx, customer, currency)
	if err != nil {
		return gen.OpenrailsLedgerTransfer{}, err
	}
	rev, err := l.EnsureSystemAccount(ctx, PlatformRevenue, currency)
	if err != nil {
		return gen.OpenrailsLedgerTransfer{}, err
	}
	c := customer
	return l.Apply(ctx, Transfer{
		Debit: cust, Credit: rev, Amount: amount, Currency: currency, Type: "spend",
		Invoker: &invoker, Resource: &resource, Customer: &c, AllowDebitNegativeUpTo: floor,
	})
}

// Expire moves an unspent credit remainder from the customer's balance to the
// expired-credits account (DR customer_balance / CR expired_credits).
func (l *Ledger) Expire(ctx context.Context, customer uuid.UUID, currency string, amount int64, source, sourceID string) (gen.OpenrailsLedgerTransfer, error) {
	cust, err := l.EnsureCustomerBalance(ctx, customer, currency)
	if err != nil {
		return gen.OpenrailsLedgerTransfer{}, err
	}
	exp, err := l.EnsureSystemAccount(ctx, ExpiredCredits, currency)
	if err != nil {
		return gen.OpenrailsLedgerTransfer{}, err
	}
	c := customer
	return l.Apply(ctx, Transfer{
		Debit: cust, Credit: exp, Amount: amount, Currency: currency, Type: "expire",
		Source: &source, SourceID: &sourceID, Customer: &c,
	})
}

// AccrueOwed recognizes postpaid usage as a revenue claim against the arrears
// liability account (DR arrears_liability / CR platform_revenue). The customer
// balance is untouched — the debt is tracked as a pending invoice item by the
// caller and nets out when PayOwed settles it. arrears_liability's net balance
// (which goes negative as debt accrues) is the conserved owed exposure.
func (l *Ledger) AccrueOwed(ctx context.Context, customer uuid.UUID, currency string, amount int64, source, sourceID string, invoice *uuid.UUID) (gen.OpenrailsLedgerTransfer, error) {
	liab, err := l.EnsureSystemAccount(ctx, ArrearsLiability, currency)
	if err != nil {
		return gen.OpenrailsLedgerTransfer{}, err
	}
	rev, err := l.EnsureSystemAccount(ctx, PlatformRevenue, currency)
	if err != nil {
		return gen.OpenrailsLedgerTransfer{}, err
	}
	c := customer
	return l.Apply(ctx, Transfer{
		Debit: liab, Credit: rev, Amount: amount, Currency: currency, Type: "owed_accrual",
		Source: &source, SourceID: &sourceID, Customer: &c, Invoice: invoice,
	})
}

// PayOwed settles accrued arrears via an external charge (DR processor_clearing /
// CR arrears_liability), bringing the liability account back toward zero.
func (l *Ledger) PayOwed(ctx context.Context, customer uuid.UUID, currency string, amount int64, source, sourceID string, invoice *uuid.UUID) (gen.OpenrailsLedgerTransfer, error) {
	clearing, err := l.EnsureSystemAccount(ctx, ProcessorClearing, currency)
	if err != nil {
		return gen.OpenrailsLedgerTransfer{}, err
	}
	liab, err := l.EnsureSystemAccount(ctx, ArrearsLiability, currency)
	if err != nil {
		return gen.OpenrailsLedgerTransfer{}, err
	}
	c := customer
	return l.Apply(ctx, Transfer{
		Debit: clearing, Credit: liab, Amount: amount, Currency: currency, Type: "owed_payment",
		Source: &source, SourceID: &sourceID, Customer: &c, Invoice: invoice,
	})
}

// Authorize places a two-phase hold debiting the customer balance (pending).
func (l *Ledger) Authorize(ctx context.Context, customer uuid.UUID, currency string, amount int64, invoker, resource string) (gen.OpenrailsLedgerTransfer, error) {
	cust, err := l.EnsureCustomerBalance(ctx, customer, currency)
	if err != nil {
		return gen.OpenrailsLedgerTransfer{}, err
	}
	rev, err := l.EnsureSystemAccount(ctx, PlatformRevenue, currency)
	if err != nil {
		return gen.OpenrailsLedgerTransfer{}, err
	}
	c := customer
	return l.Apply(ctx, Transfer{
		Debit: cust, Credit: rev, Amount: amount, Currency: currency, Type: "authorize", Phase: Pending,
		Invoker: &invoker, Resource: &resource, Customer: &c,
	})
}

// Capture posts (some of) a pending hold's amount; actual must be in (0, pending].
// The remaining hold is released implicitly once the pending is resolved.
func (l *Ledger) Capture(ctx context.Context, pendingID uuid.UUID, actual, floor int64) (gen.OpenrailsLedgerTransfer, error) {
	p, err := l.pending(ctx, pendingID)
	if err != nil {
		return gen.OpenrailsLedgerTransfer{}, err
	}
	if actual <= 0 || actual > p.Amount {
		return gen.OpenrailsLedgerTransfer{}, fmt.Errorf("ledger: capture amount %d out of range (pending %d)", actual, p.Amount)
	}
	pid := pendingID
	return l.Apply(ctx, Transfer{
		Debit: p.DebitAccountID, Credit: p.CreditAccountID, Amount: actual, Currency: p.Currency,
		Type: "capture", Phase: PostPending, PendingID: &pid, Customer: p.CustomerID, AllowDebitNegativeUpTo: floor,
	})
}

// Release voids a pending hold (no balance effect).
func (l *Ledger) Release(ctx context.Context, pendingID uuid.UUID) (gen.OpenrailsLedgerTransfer, error) {
	p, err := l.pending(ctx, pendingID)
	if err != nil {
		return gen.OpenrailsLedgerTransfer{}, err
	}
	pid := pendingID
	return l.Apply(ctx, Transfer{
		Debit: p.DebitAccountID, Credit: p.CreditAccountID, Amount: p.Amount, Currency: p.Currency,
		Type: "release", Phase: VoidPending, PendingID: &pid, Customer: p.CustomerID,
	})
}

func (l *Ledger) pending(ctx context.Context, pendingID uuid.UUID) (gen.OpenrailsLedgerTransfer, error) {
	p, err := l.q.GetLedgerTransfer(ctx, gen.GetLedgerTransferParams{MerchantID: l.merchant, ID: pendingID})
	if err != nil {
		return gen.OpenrailsLedgerTransfer{}, fmt.Errorf("ledger: load pending %s: %w", pendingID, err)
	}
	if Phase(p.Phase) != Pending {
		return gen.OpenrailsLedgerTransfer{}, fmt.Errorf("ledger: transfer %s is not pending (phase %s)", pendingID, p.Phase)
	}
	return p, nil
}
