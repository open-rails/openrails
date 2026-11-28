package books

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/currency"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

var (
	// ErrNotMoney refuses a general-ledger posting in a currency outside the
	// registry, a merchant's own unit included.
	ErrNotMoney = errors.New("books: the general ledger holds registry currencies only")
	// ErrCoordinateReused is a posting at a coordinate that already posted
	// another transfer.
	ErrCoordinateReused = errors.New("books: the coordinate already posted a different transfer")
)

// CheckCurrency refuses a currency the general ledger cannot hold.
func CheckCurrency(code string) error {
	if u, ok := currency.Lookup(code); !ok || u.Code != code {
		return fmt.Errorf("%w: %q", ErrNotMoney, code)
	}
	return nil
}

// OpenedLateFindingType is the finding a posting raises when it creates a
// merchant-wide general-ledger account that nothing opened first.
const OpenedLateFindingType = "books.account.opened_late"

// merchantWide is a general-ledger account with no customer: Open creates it
// where its currency or PSP becomes sellable.
func (a Account) merchantWide() bool { return a.Book == General && a.Scope != Customer }

// EnsureAccount get-or-creates one account of the chart in a currency, with
// the chart's book, class and sign rules: a customer-scoped type names its
// customer, a PSP-scoped one its PSP, bank_cash its bank account. created
// reports this call made it; one a concurrent writer created first is read
// back. A merchant-wide general-ledger account is Open's to create; one
// created here, inside the caller's transaction, raises OpenedLateFindingType
// once.
func EnsureAccount(ctx context.Context, q *gen.Queries, merchant uuid.UUID, r Ref, currency string) (id uuid.UUID, created bool, err error) {
	acct, ok := Lookup(r.Type)
	if !ok {
		return uuid.Nil, false, fmt.Errorf("books: unknown account type %q", r.Type)
	}
	if (acct.Scope == Customer) != (r.Customer != nil) || (acct.Scope == PSP) != (r.PSP != nil) || (acct.Scope == Bank) != (r.Bank != nil) {
		return uuid.Nil, false, fmt.Errorf("books: a %s account is %s-scoped", r.Type, acct.Scope)
	}
	get := gen.GetLedgerAccountParams{MerchantID: merchant, AccountType: string(r.Type), Currency: currency, CustomerID: r.Customer, PspID: r.PSP, BankAccountID: r.Bank}
	row, err := q.GetLedgerAccount(ctx, get)
	if err == nil {
		return row.ID, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, fmt.Errorf("books: get %s account: %w", r.Type, err)
	}
	if created, err = insertAccount(ctx, q, merchant, acct, currency, r); err != nil {
		return uuid.Nil, false, err
	}
	if created && acct.merchantWide() {
		if err := openedLate(ctx, q, merchant, r, currency); err != nil {
			return uuid.Nil, false, err
		}
	}
	row, err = q.GetLedgerAccount(ctx, get)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("books: get %s account: %w", r.Type, err)
	}
	return row.ID, created, nil
}

// openedLate records a merchant-wide account a posting had to create.
func openedLate(ctx context.Context, q *gen.Queries, merchant uuid.UUID, r Ref, currency string) error {
	t := r.Type
	subject, evidence := string(t)+"/"+currency, map[string]string{"account_type": string(t), "currency": currency}
	if r.PSP != nil {
		subject += "/" + r.PSP.String()
		evidence["psp_id"] = r.PSP.String()
	}
	if r.Bank != nil {
		subject += "/" + r.Bank.String()
		evidence["bank_account_id"] = r.Bank.String()
	}
	raw, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	action := "A posting created this general-ledger account inside its own transaction, where a concurrent first posting waits on it. Open the books where its currency or PSP becomes sellable (books.Open)."
	_, err = q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID: merchant, FindingType: OpenedLateFindingType, SubjectKey: subject,
		Severity: "low", Status: "requires_review", RecommendedAction: &action, Evidence: raw,
	})
	if err != nil {
		return fmt.Errorf("books: raise %s: %w", OpenedLateFindingType, err)
	}
	return nil
}

// insertAccount creates one account unless a concurrent writer did.
func insertAccount(ctx context.Context, q *gen.Queries, merchant uuid.UUID, acct Account, currency string, r Ref) (bool, error) {
	_, err := q.InsertLedgerAccount(ctx, gen.InsertLedgerAccountParams{
		MerchantID: merchant, Book: string(acct.Book), Class: string(acct.Class),
		CustomerID: r.Customer, PspID: r.PSP, BankAccountID: r.Bank, AccountType: string(acct.Type), Currency: currency,
		DebitsMustNotExceedCredits: acct.DebitsMustNotExceedCredits,
		CreditsMustNotExceedDebits: acct.CreditsMustNotExceedDebits,
	})
	if err == nil {
		return true, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return false, fmt.Errorf("books: create %s account: %w", acct.Type, err)
}

// Open makes a merchant's general ledger whole in a short transaction of its
// own: in every currency its catalog prices in, every currency named and
// every currency already open, every merchant-wide account, each of its PSPs'
// own accounts, and each bank account's bank cash in the bank account's
// currency. A merchant's own unit is not money and is skipped. It
// runs where a currency or PSP becomes sellable, outside any business
// transaction, so no first posting waits on another's uncommitted account;
// once whole it only reads.
func Open(ctx context.Context, pool *db.Pool, merchant uuid.UUID, currencies ...string) error {
	if db.InCallerTransaction(ctx) {
		return db.ErrCallerTransaction
	}
	q := gen.New(pool)
	sellable, err := q.ListSellableCurrencies(ctx, merchant)
	if err != nil {
		return fmt.Errorf("books: read the sellable currencies: %w", err)
	}
	open, err := q.ListOpenGeneralLedgerAccounts(ctx, merchant)
	if err != nil {
		return fmt.Errorf("books: read the open general ledger: %w", err)
	}
	psps, err := q.ListGeneralLedgerPSPs(ctx, merchant)
	if err != nil {
		return fmt.Errorf("books: read the PSPs: %w", err)
	}
	banks, err := q.ListGeneralLedgerBankAccounts(ctx, merchant)
	if err != nil {
		return fmt.Errorf("books: read the bank accounts: %w", err)
	}
	type key struct {
		t         AccountType
		currency  string
		psp, bank uuid.UUID
	}
	have := map[key]bool{}
	var money []string
	for _, a := range open {
		var psp, bank uuid.UUID
		if a.PspID != nil {
			psp = *a.PspID
		}
		if a.BankAccountID != nil {
			bank = *a.BankAccountID
		}
		have[key{AccountType(a.AccountType), a.Currency, psp, bank}] = true
		money = append(money, a.Currency)
	}
	for _, b := range banks {
		if CheckCurrency(b.Currency) == nil {
			money = append(money, b.Currency)
		}
	}
	for _, c := range append(sellable, currencies...) {
		if CheckCurrency(c) == nil {
			money = append(money, c)
		}
	}
	slices.Sort(money)
	type missing struct {
		acct     Account
		currency string
		ref      Ref
	}
	var want []missing
	for _, c := range slices.Compact(money) {
		for _, a := range chart {
			switch {
			case !a.merchantWide():
			case a.Scope == System:
				if !have[key{a.Type, c, uuid.Nil, uuid.Nil}] {
					want = append(want, missing{a, c, Ref{Type: a.Type}})
				}
			case a.Scope == PSP:
				for _, p := range psps {
					if !have[key{a.Type, c, p, uuid.Nil}] {
						want = append(want, missing{a, c, Ref{Type: a.Type, PSP: &p}})
					}
				}
			case a.Scope == Bank:
				for _, b := range banks {
					if b.Currency == c && !have[key{a.Type, c, uuid.Nil, b.ID}] {
						want = append(want, missing{a, c, Ref{Type: a.Type, Bank: &b.ID}})
					}
				}
			}
		}
	}
	if len(want) == 0 {
		return nil
	}
	return pool.CommittedMerchantTx(ctx, billing.MerchantID(merchant), func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		for _, m := range want {
			if _, err := insertAccount(ctx, q, merchant, m.acct, m.currency, m.ref); err != nil {
				return err
			}
		}
		return nil
	})
}

// Prepare is Open where a currency or PSP becomes sellable: it never fails
// the operation that sells, and inside a caller's transaction it does nothing.
// A gap it leaves is filled by the first posting, which raises
// OpenedLateFindingType.
func Prepare(ctx context.Context, pool *db.Pool, logger *slog.Logger, merchant uuid.UUID, currencies ...string) {
	if pool == nil {
		return
	}
	if err := Open(ctx, pool, merchant, currencies...); err != nil && !errors.Is(err, db.ErrCallerTransaction) && logger != nil {
		logger.WarnContext(ctx, "books: the general ledger was not opened; its first posting will open it", "merchant_id", merchant, "error", err)
	}
}

// Books posts one merchant's general-ledger transfers.
type Books struct {
	q        *gen.Queries
	merchant uuid.UUID
}

// New binds the general ledger to a query handle and the merchant it serves.
func New(q *gen.Queries, merchant uuid.UUID) *Books {
	return &Books{q: q, merchant: merchant}
}

// Ref names an account: its type, and the customer, PSP or bank account its
// scope needs.
type Ref struct {
	Type     AccountType
	Customer *uuid.UUID
	PSP      *uuid.UUID
	Bank     *uuid.UUID
}

// Posting is one general-ledger transfer.
type Posting struct {
	Debit, Credit Ref
	Amount        int64
	Currency      string
	Type          TransferType
	Coord         Coord
	// EffectiveAt is when the event happened, the transfer's business date.
	EffectiveAt time.Time
	// Customer is the customer the transfer is attributed to. A
	// customer-scoped leg names the same customer.
	Customer                                   *uuid.UUID
	Invoice, InvoiceLine, Product, CreditGrant *uuid.UUID
	// RevenueSchedule is the schedule whose deferred revenue the transfer
	// funds, recognises or reduces.
	RevenueSchedule   *uuid.UUID
	Invoker, Resource *string
}

// Post appends one general-ledger transfer, once at its coordinate. applied
// is false when the coordinate already posted the same transfer: nothing
// moved, and the row is the one that landed. A different transfer there is
// ErrCoordinateReused.
func (b *Books) Post(ctx context.Context, p Posting) (tr gen.BillingLedgerTransfer, applied bool, err error) {
	if err := p.check(); err != nil {
		return tr, false, err
	}
	debit, _, err := EnsureAccount(ctx, b.q, b.merchant, p.Debit, p.Currency)
	if err != nil {
		return tr, false, err
	}
	credit, _, err := EnsureAccount(ctx, b.q, b.merchant, p.Credit, p.Currency)
	if err != nil {
		return tr, false, err
	}
	at := p.EffectiveAt.UTC().Truncate(time.Microsecond)
	tr, err = b.q.InsertGeneralLedgerTransfer(ctx, gen.InsertGeneralLedgerTransferParams{
		MerchantID: b.merchant, DebitAccountID: debit, CreditAccountID: credit,
		Amount: p.Amount, Currency: p.Currency, TransferType: string(p.Type),
		Operation: string(p.Coord.Operation), Source: p.Coord.Source, SourceID: p.Coord.SourceID,
		CreditGrantID: p.CreditGrant, CustomerID: p.Customer, InvokerID: p.Invoker, Resource: p.Resource,
		InvoiceID: p.Invoice, InvoiceLineID: p.InvoiceLine, ProductID: p.Product, EffectiveAt: at, RevenueScheduleID: p.RevenueSchedule,
	})
	if err == nil {
		return tr, true, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		tr, err = b.q.GetLedgerTransferAtCoordinate(ctx, gen.GetLedgerTransferAtCoordinateParams{
			MerchantID: b.merchant, Book: string(General), CustomerID: p.Customer, Currency: p.Currency, TransferType: string(p.Type),
			Operation: string(p.Coord.Operation), Source: p.Coord.Source, SourceID: p.Coord.SourceID,
			CreditGrantID: p.CreditGrant, InvoiceLineID: p.InvoiceLine,
		})
		if err != nil {
			return tr, false, fmt.Errorf("books: resolve transfer at %s: %w", p.Coord, err)
		}
		if tr.DebitAccountID != debit || tr.CreditAccountID != credit || tr.Amount != p.Amount || !tr.EffectiveAt.Equal(at) ||
			!sameUUID(tr.InvoiceID, p.Invoice) || !sameUUID(tr.ProductID, p.Product) || !sameUUID(tr.RevenueScheduleID, p.RevenueSchedule) ||
			!sameText(tr.InvokerID, p.Invoker) || !sameText(tr.Resource, p.Resource) {
			return tr, false, fmt.Errorf("%w at %s", ErrCoordinateReused, p.Coord)
		}
		return tr, false, nil
	}
	return tr, false, err
}

func (p Posting) check() error {
	if !slices.Contains(GeneralTransferTypes, p.Type) {
		return fmt.Errorf("books: %q is not a general-ledger transfer type", p.Type)
	}
	if err := p.Coord.Validate(); err != nil {
		return err
	}
	if _, ok := SourceBasis(p.Coord.Source); ProviderFigure(p.Type) && !ok {
		return fmt.Errorf("books: a %s posts at a provider source of known basis, not %q", p.Type, p.Coord.Source)
	}
	if p.Amount <= 0 {
		return fmt.Errorf("books: amount must be positive")
	}
	if p.EffectiveAt.IsZero() {
		return fmt.Errorf("books: effective_at is required")
	}
	if p.Customer == nil && (p.Invoice != nil || p.CreditGrant != nil) {
		return fmt.Errorf("books: a posting for an invoice or a credit lot names its customer")
	}
	if err := CheckCurrency(p.Currency); err != nil {
		return err
	}
	for _, r := range []Ref{p.Debit, p.Credit} {
		acct, ok := Lookup(r.Type)
		if !ok || acct.Book != General {
			return fmt.Errorf("books: %q is not a general-ledger account", r.Type)
		}
		if acct.Scope == Customer && (p.Customer == nil || r.Customer == nil || *r.Customer != *p.Customer) {
			return fmt.Errorf("books: a %s leg names the posting's own customer", r.Type)
		}
	}
	return nil
}

func sameUUID(a, b *uuid.UUID) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func sameText(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
