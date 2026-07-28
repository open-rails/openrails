// Package ledger is the #512 double-entry, append-only money ledger expressed
// over the sqlc-generated ledger_accounts / ledger_transfers tables.
//
// A ledger is a (merchant, currency) pair. Accounts belong to one ledger;
// transfers move an amount debit->credit within one ledger and are immutable
// (the openrails_app role is granted SELECT,INSERT only). Balances are maintained
// on account counters, with transfers as the immutable truth. Every transfer is
// posted (single-phase): the admission hold lives in Redis (#513), never as an
// in-ledger pending (the two-phase apparatus was retired, migration 014).
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
	// is revoked (distinct from ExpiredCredits = time-lapse). The money is frozen
	// here (recoverable/reversible), not refunded; a refund moves it out to
	// RailClearing. (#514, see docs/consistency-invariants.md §11 decision 4.)
	RevokedCredits AccountType = "revoked_credits"
	FXLiquidity    AccountType = "fx_liquidity"
	World          AccountType = "world"
)

// TransferType is the closed vocabulary of ledger_transfers.transfer_type,
// mirrored by the schema's ledger_transfers_type_check (#832). It was free text:
// idx_ledger_transfers_lot_once — what stops a credit lot being deposited,
// expired or revoked twice — is PARTIAL on named transfer_type literals, so a
// typo fell outside the index and the duplicate posted silently.
type TransferType string

const (
	Deposit      TransferType = "deposit"       // rail clearing -> customer balance
	CreditSpend  TransferType = "credit_spend"  // customer balance -> platform revenue
	CreditExpire TransferType = "credit_expire" // unspent lot remainder, time-lapsed
	CreditRevoke TransferType = "credit_revoke" // unspent lot remainder, clawed back
	OwedAccrual  TransferType = "owed_accrual"  // postpaid usage -> arrears liability
	OwedPayment  TransferType = "owed_payment"  // arrears settled by an external charge
)

// AllTransferTypes must equal the DB CHECK exactly (TestTransferTypeVocabularyMatchesSchema).
var AllTransferTypes = []TransferType{Deposit, CreditSpend, CreditExpire, CreditRevoke, OwedAccrual, OwedPayment}

// LotOnceTransferTypes are the at-most-once-per-lot movements enforced by
// idx_ledger_transfers_lot_once.
var LotOnceTransferTypes = []TransferType{Deposit, CreditExpire, CreditRevoke}

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

// CustomerBalanceAccountID returns the customer's balance account id, and false
// when it does not exist yet. Read-only: unlike EnsureCustomerBalance it NEVER
// creates the account, so a balance READ for a never-registered customer is a
// clean zero, not an account-creating write (#534).
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

// Apply appends one (posted, single-phase) transfer, enforcing the debit
// account's sign constraint before it posts to the balance counters.
func (l *Ledger) Apply(ctx context.Context, t Transfer) (gen.OpenrailsLedgerTransfer, error) {
	if err := l.checkDebitFloor(ctx, t.Debit, t.Amount, t.AllowDebitNegativeUpTo); err != nil {
		return gen.OpenrailsLedgerTransfer{}, err
	}
	tr, err := l.q.InsertLedgerTransfer(ctx, gen.InsertLedgerTransferParams{
		MerchantID:             l.merchant,
		DebitAccountID:         t.Debit,
		CreditAccountID:        t.Credit,
		Amount:                 t.Amount,
		Currency:               t.Currency,
		TransferType:           string(t.Type),
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

// --- flow constructors: the standard money movements as transfer pairs --------

// Deposit credits the customer's balance from the rail-clearing account
// (DR processor_clearing / CR customer_balance). grantID attributes the deposit
// to its #514 credit lot (uuid.Nil for a non-lot deposit).
func (l *Ledger) Deposit(ctx context.Context, customer uuid.UUID, currency string, amount int64, source, sourceID string, grantID uuid.UUID) (gen.OpenrailsLedgerTransfer, error) {
	clearing, err := l.EnsureSystemAccount(ctx, RailClearing, currency)
	if err != nil {
		return gen.OpenrailsLedgerTransfer{}, err
	}
	cust, err := l.EnsureCustomerBalance(ctx, customer, currency)
	if err != nil {
		return gen.OpenrailsLedgerTransfer{}, err
	}
	c := customer
	t := Transfer{
		Debit: clearing, Credit: cust, Amount: amount, Currency: currency, Type: Deposit,
		Source: &source, SourceID: &sourceID, Customer: &c,
	}
	if grantID != uuid.Nil {
		g := grantID
		t.GrantID = &g
	}
	return l.Apply(ctx, t)
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
		Debit: liab, Credit: rev, Amount: amount, Currency: currency, Type: OwedAccrual,
		Source: &source, SourceID: &sourceID, Customer: &c, Invoice: invoice,
	})
}

// PayOwed settles accrued arrears via an external charge (DR processor_clearing /
// CR arrears_liability), bringing the liability account back toward zero.
func (l *Ledger) PayOwed(ctx context.Context, customer uuid.UUID, currency string, amount int64, source, sourceID string, invoice *uuid.UUID) (gen.OpenrailsLedgerTransfer, error) {
	clearing, err := l.EnsureSystemAccount(ctx, RailClearing, currency)
	if err != nil {
		return gen.OpenrailsLedgerTransfer{}, err
	}
	liab, err := l.EnsureSystemAccount(ctx, ArrearsLiability, currency)
	if err != nil {
		return gen.OpenrailsLedgerTransfer{}, err
	}
	c := customer
	return l.Apply(ctx, Transfer{
		Debit: clearing, Credit: liab, Amount: amount, Currency: currency, Type: OwedPayment,
		Source: &source, SourceID: &sourceID, Customer: &c, Invoice: invoice,
	})
}

// NOTE: in-ledger two-phase (Authorize/Capture/Release over pending transfers)
// was retired in migration 014 — admission holds live in Redis (#513), so every
// transfer here is posted. Re-add a durable two-phase path here (with an expiry
// sweep) if a future flow genuinely needs in-ledger holds.
