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
	// CreditReinstate reverses a clawback (revoked_credits -> customer_balance).
	// The revoke is deliberately reversible (#514); this is how.
	CreditReinstate TransferType = "credit_reinstate"
	OwedAccrual     TransferType = "owed_accrual" // postpaid usage -> arrears liability
	OwedPayment     TransferType = "owed_payment" // arrears settled by an external charge
	// OwedWriteoff cancels accrued debt without money moving (or#897): the exact
	// inverse of OwedAccrual, posted when an invoice is voided. Distinct from
	// OwedPayment, which means a rail actually collected.
	OwedWriteoff TransferType = "owed_writeoff"
)

// AllTransferTypes must equal the DB CHECK exactly (TestTransferTypeVocabularyMatchesSchema).
var AllTransferTypes = []TransferType{Deposit, CreditSpend, CreditExpire, CreditRevoke, CreditReinstate, OwedAccrual, OwedPayment, OwedWriteoff}

// LotOnceTransferTypes are the at-most-once-per-lot movements enforced by
// idx_ledger_transfers_lot_once.
var LotOnceTransferTypes = []TransferType{Deposit, CreditExpire, CreditRevoke}

// Operation is the KIND of money write that posted a transfer — the or#894
// discriminator in the idempotency coordinate. It is ENGINE-COMPOSED: a caller
// supplies only (source, source_id), so two different operations can never
// alias on one caller key, and a caller cannot claim another operation's key.
//
// Without it, a wasted-spend overage charge and the CAPTURE of the same
// rendered request both landed at ("invoke", request_id): the capture moved 0
// micros, returned the waste transfer, and reported success.
type Operation string

const (
	OpSpend    Operation = "spend"    // MoneyService.SpendCredits
	OpCapture  Operation = "capture"  // MoneyService.CaptureAuthorized
	OpWithdraw Operation = "withdraw" // MoneyService.Withdraw
	OpDeposit  Operation = "deposit"  // credit grant / top-up

	OpCreditExpire    Operation = "credit_expire"
	OpCreditRevoke    Operation = "credit_revoke"
	OpCreditReinstate Operation = "credit_reinstate"

	OpArrearsAccrual     Operation = "arrears_accrual"      // MoneyService.AccrueOwed
	OpMeteredRating      Operation = "metered_rating"       // rate-card sweep accrual
	OpMinimumSpendTrueUp Operation = "minimum_spend_trueup" // invoice close true-up
	OpInvoicePayment     Operation = "invoice_payment"      // arrears settled by a rail charge
	OpManualInvoicePay   Operation = "manual_invoice_payment"
	OpInvoiceVoid        Operation = "invoice_void"

	usageOpPrefix = "usage:"
)

// UsageOperation is the operation kind of a metered usage charge. event_type is
// part of the kind because usage_events already dedupes on it
// (uq_usage_events_idem): two different event types at one (source, source_id)
// are two events, so they must be two ledger legs.
func UsageOperation(eventType string) Operation {
	return Operation(usageOpPrefix + strings.TrimSpace(eventType))
}

// Coord is the idempotency coordinate a durable money write posts at, within
// (merchant, customer, currency). All three parts are required — money.
// IdempotencyKey is the only constructor callers use to build one.
type Coord struct {
	Operation Operation
	Source    string
	SourceID  string
}

// Validate refuses a partial coordinate. A blank part is the shape that made a
// money write silently non-idempotent (or#891) or ambiguous (or#894).
func (c Coord) Validate() error {
	if strings.TrimSpace(string(c.Operation)) == "" || c.Operation == usageOpPrefix {
		return fmt.Errorf("ledger: operation required on the idempotency coordinate")
	}
	if strings.TrimSpace(c.Source) == "" || strings.TrimSpace(c.SourceID) == "" {
		return fmt.Errorf("ledger: source and source_id required on the idempotency coordinate")
	}
	return nil
}

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

// EnsureCustomerArrears get-or-creates a customer's OWN arrears-liability
// account (or#897). Receivables are per-debtor: a merchant-wide liability
// account can only answer "how much is owed in total", so per-payer exposure
// had to be summed over that payer's whole transfer history — O(records) on the
// admission hot path, which is exactly the shape the work-scales-with-activity
// law exists to prevent. With one account per debtor, outstanding owed is the
// account's counter: O(1), symmetric with balance.
//
// NOT debits_must_not_exceed_credits: an arrears account is SUPPOSED to go
// negative — that negative balance IS the debt.
func (l *Ledger) EnsureCustomerArrears(ctx context.Context, customer uuid.UUID, currency string) (uuid.UUID, error) {
	c := customer
	return l.ensureAccount(ctx, ArrearsLiability, currency, &c, false, false)
}

// CustomerArrearsAccountID returns the customer's arrears account id, and false
// when it does not exist. Read-only: an exposure READ must never create an
// account (#534), so a payer who has never accrued reads a clean zero.
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

// OutstandingOwed is the payer's unpaid arrears in this currency, as a POSITIVE
// amount, read O(1) from the arrears account's maintained counters. Debt makes
// the account balance negative (accruals debit it, payments credit it), so the
// exposure is its negation. Zero when the payer has no arrears account.
//
// This is the ONLY exposure substrate (or#878 ruling, or#897). It replaced an
// invoice-derived sum: invoices are presentation/collection artifacts, they lag
// the ledger by a finalize cycle, and every invoice line already has an
// owed_accrual leg — so the invoice view could only ever be a stale copy.
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
	// Coord is the operation coordinate this leg is idempotent on (or#894).
	// Required on every transfer.
	Coord Coord
	// GrantID attributes a credit_spend/credit_expire/deposit to its #514 credit
	// lot, independently of Coord (which carries the OPERATION coordinate).
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
//
// Deprecated in favour of ApplyIdempotent, which reports whether the write
// actually landed. Apply keeps the old shape for read-through call sites that
// genuinely do not care; it is a thin wrapper and carries no second contract.
func (l *Ledger) Apply(ctx context.Context, t Transfer) (gen.OpenrailsLedgerTransfer, error) {
	tr, _, err := l.ApplyIdempotent(ctx, t)
	return tr, err
}

// ApplyIdempotent is THE durable money write (or#892). Every ledger movement in
// the system funnels through it, and once-only is enforced by the DATABASE:
// the insert is ON CONFLICT DO NOTHING against
// idx_ledger_transfers_operation_once, so a replay at the same coordinate
// inserts nothing no matter what order the caller took its locks in.
//
// applied reports what happened:
//   - true  — this call posted the transfer; the balance counters moved.
//   - false — the coordinate was already committed. NOTHING moved in this call,
//     and the returned row is the transfer that DID land. This is the
//     applied-vs-replayed signal consumers were rebuilding claim tables to get.
//
// The sign-constraint check runs before the insert, so a replay of a transfer
// that would now breach the floor still resolves as a replay rather than a
// spurious insufficient-funds error: the money already moved once, legitimately.
func (l *Ledger) ApplyIdempotent(ctx context.Context, t Transfer) (tr gen.OpenrailsLedgerTransfer, applied bool, err error) {
	// The coordinate is validated HERE, at the one insert every money movement
	// funnels through, so no new spend path can post an unkeyed or ambiguous leg.
	if err := t.Coord.Validate(); err != nil {
		return gen.OpenrailsLedgerTransfer{}, false, err
	}
	// A replay must not be turned away by the floor: resolve it first.
	if existing, found, gerr := l.transferAt(ctx, t); gerr != nil {
		return gen.OpenrailsLedgerTransfer{}, false, gerr
	} else if found {
		return existing, false, nil
	}
	if err := l.checkDebitFloor(ctx, t.Debit, t.Amount, t.AllowDebitNegativeUpTo); err != nil {
		return gen.OpenrailsLedgerTransfer{}, false, err
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
	// Zero rows = ON CONFLICT DO NOTHING fired: a concurrent transaction
	// committed this coordinate between the read above and this insert. The
	// database, not the lock order, is what refused it.
	if errors.Is(err, pgx.ErrNoRows) {
		existing, found, gerr := l.transferAt(ctx, t)
		if gerr != nil {
			return gen.OpenrailsLedgerTransfer{}, false, gerr
		}
		if !found {
			return gen.OpenrailsLedgerTransfer{}, false, fmt.Errorf(
				"ledger: insert at %s conflicted but no committed row is visible", t.Coord)
		}
		return existing, false, nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && strings.Contains(pgErr.Message, "ledger_insufficient_funds") {
		return gen.OpenrailsLedgerTransfer{}, false, fmt.Errorf("%w: %s", ErrInsufficientFunds, pgErr.Message)
	}
	return gen.OpenrailsLedgerTransfer{}, false, err
}

// transferAt resolves the row already committed at a transfer's full physical
// identity (coordinate + lot), if any.
func (l *Ledger) transferAt(ctx context.Context, t Transfer) (gen.OpenrailsLedgerTransfer, bool, error) {
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
		return gen.OpenrailsLedgerTransfer{}, false, nil
	}
	return gen.OpenrailsLedgerTransfer{}, false, fmt.Errorf("ledger: resolve transfer at %s: %w", t.Coord, err)
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
func (l *Ledger) Deposit(ctx context.Context, customer uuid.UUID, currency string, amount int64, coord Coord, grantID uuid.UUID) (gen.OpenrailsLedgerTransfer, error) {
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
		Coord: coord, Customer: &c,
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
func (l *Ledger) AccrueOwed(ctx context.Context, customer uuid.UUID, currency string, amount int64, coord Coord, invoice *uuid.UUID) (gen.OpenrailsLedgerTransfer, error) {
	tr, _, err := l.AccrueOwedIdempotent(ctx, customer, currency, amount, coord, invoice)
	return tr, err
}

// AccrueOwedIdempotent is AccrueOwed reporting whether the accrual actually
// posted, or replayed a coordinate already committed.
func (l *Ledger) AccrueOwedIdempotent(ctx context.Context, customer uuid.UUID, currency string, amount int64, coord Coord, invoice *uuid.UUID) (gen.OpenrailsLedgerTransfer, bool, error) {
	liab, err := l.EnsureCustomerArrears(ctx, customer, currency)
	if err != nil {
		return gen.OpenrailsLedgerTransfer{}, false, err
	}
	rev, err := l.EnsureSystemAccount(ctx, PlatformRevenue, currency)
	if err != nil {
		return gen.OpenrailsLedgerTransfer{}, false, err
	}
	c := customer
	return l.ApplyIdempotent(ctx, Transfer{
		Debit: liab, Credit: rev, Amount: amount, Currency: currency, Type: OwedAccrual,
		Coord: coord, Customer: &c, Invoice: invoice,
	})
}

// WriteOffOwed cancels accrued arrears WITHOUT money moving (DR platform_revenue
// / CR arrears_liability) — the exact inverse of AccrueOwed. Posted when an
// invoice is voided: the debt is cancelled, so the revenue recognised at accrual
// is given back and the payer's liability returns toward zero. Without this the
// invoice says "voided" and the ledger says "still owed", and since the ledger
// is the exposure substrate the payer stays capped for a bill nobody owes.
func (l *Ledger) WriteOffOwed(ctx context.Context, customer uuid.UUID, currency string, amount int64, coord Coord, invoice *uuid.UUID) (gen.OpenrailsLedgerTransfer, error) {
	rev, err := l.EnsureSystemAccount(ctx, PlatformRevenue, currency)
	if err != nil {
		return gen.OpenrailsLedgerTransfer{}, err
	}
	liab, err := l.EnsureCustomerArrears(ctx, customer, currency)
	if err != nil {
		return gen.OpenrailsLedgerTransfer{}, err
	}
	c := customer
	return l.Apply(ctx, Transfer{
		Debit: rev, Credit: liab, Amount: amount, Currency: currency, Type: OwedWriteoff,
		Coord: coord, Customer: &c, Invoice: invoice,
	})
}

// PayOwed settles accrued arrears via an external charge (DR processor_clearing /
// CR arrears_liability), bringing the liability account back toward zero.
func (l *Ledger) PayOwed(ctx context.Context, customer uuid.UUID, currency string, amount int64, coord Coord, invoice *uuid.UUID) (gen.OpenrailsLedgerTransfer, error) {
	clearing, err := l.EnsureSystemAccount(ctx, RailClearing, currency)
	if err != nil {
		return gen.OpenrailsLedgerTransfer{}, err
	}
	liab, err := l.EnsureCustomerArrears(ctx, customer, currency)
	if err != nil {
		return gen.OpenrailsLedgerTransfer{}, err
	}
	c := customer
	return l.Apply(ctx, Transfer{
		Debit: clearing, Credit: liab, Amount: amount, Currency: currency, Type: OwedPayment,
		Coord: coord, Customer: &c, Invoice: invoice,
	})
}

// NOTE: in-ledger two-phase (Authorize/Capture/Release over pending transfers)
// was retired in migration 014 — admission holds live in Redis (#513), so every
// transfer here is posted. Re-add a durable two-phase path here (with an expiry
// sweep) if a future flow genuinely needs in-ledger holds.
