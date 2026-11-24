package books

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
	"github.com/open-rails/openrails/internal/currency"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

var (
	// ErrNotMoney refuses a general-ledger posting in a currency outside the
	// registry, a merchant's own unit included.
	ErrNotMoney = errors.New("books: the general ledger holds registry currencies only")
	// ErrSignRule is a general-ledger posting that would breach a customer
	// account's sign rule. A merchant-wide account's rule holds at commit,
	// whose refusal names ledger_type_total_sign.
	ErrSignRule = errors.New("books: transfer breaches an account's sign rule")
	// ErrNotOpen is a posting to a merchant-wide account its currency or PSP
	// has not opened (Open).
	ErrNotOpen = errors.New("books: the merchant-wide account is not open; books.Open it before the transaction")
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

// merchantWide is a general-ledger account with no customer: opened by Open,
// never in a business transaction.
func (a Account) merchantWide() bool { return a.Book == General && a.Scope != Customer }

// EnsureAccount get-or-creates one account of the chart in a currency, with
// the chart's book, class and sign rules: a customer-scoped type names its
// customer, cash_clearing its PSP. created reports this call made it; one a
// concurrent writer created first is read back. A merchant-wide
// general-ledger account is only read: Open creates it, or it is ErrNotOpen.
func EnsureAccount(ctx context.Context, q *gen.Queries, merchant uuid.UUID, t AccountType, currency string, customer, psp *uuid.UUID) (id uuid.UUID, created bool, err error) {
	acct, ok := Lookup(t)
	if !ok {
		return uuid.Nil, false, fmt.Errorf("books: unknown account type %q", t)
	}
	if (acct.Scope == Customer) != (customer != nil) || (acct.Scope == PSP) != (psp != nil) {
		return uuid.Nil, false, fmt.Errorf("books: a %s account is %s-scoped", t, acct.Scope)
	}
	get := gen.GetLedgerAccountParams{MerchantID: merchant, AccountType: string(t), Currency: currency, CustomerID: customer, PspID: psp}
	row, err := q.GetLedgerAccount(ctx, get)
	if err == nil {
		return row.ID, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, fmt.Errorf("books: get %s account: %w", t, err)
	}
	if acct.merchantWide() {
		return uuid.Nil, false, fmt.Errorf("%w: %s %s", ErrNotOpen, t, currency)
	}
	created, err = insertAccount(ctx, q, merchant, acct, currency, customer, psp)
	if err != nil {
		return uuid.Nil, false, err
	}
	row, err = q.GetLedgerAccount(ctx, get)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("books: get %s account: %w", t, err)
	}
	return row.ID, created, nil
}

// insertAccount creates one account unless a concurrent writer did.
func insertAccount(ctx context.Context, q *gen.Queries, merchant uuid.UUID, acct Account, currency string, customer, psp *uuid.UUID) (bool, error) {
	_, err := q.InsertLedgerAccount(ctx, gen.InsertLedgerAccountParams{
		MerchantID: merchant, Book: string(acct.Book), Class: string(acct.Class),
		CustomerID: customer, PspID: psp, AccountType: string(acct.Type), Currency: currency,
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

// Open opens a merchant's general ledger in a currency: every merchant-wide
// account, and psp's cash clearing when psp is set. It commits on its own, so
// it runs before the business transaction that posts there: created inside
// one, an account's uncommitted row would hold every other first writer until
// that transaction ends. Once open it only reads.
func Open(ctx context.Context, pool *db.Pool, merchant uuid.UUID, currency string, psp *uuid.UUID) error {
	if db.InCallerTransaction(ctx) {
		return db.ErrCallerTransaction
	}
	if err := CheckCurrency(currency); err != nil {
		return err
	}
	var want []Account
	for _, a := range chart {
		if a.merchantWide() && (a.Scope == System || psp != nil) {
			want = append(want, a)
		}
	}
	count := gen.CountOpenGeneralLedgerAccountsParams{MerchantID: merchant, Currency: currency, PspID: psp}
	open, err := gen.New(pool).CountOpenGeneralLedgerAccounts(ctx, count)
	if err != nil {
		return fmt.Errorf("books: read the open general ledger: %w", err)
	}
	if open == int64(len(want)) {
		return nil
	}
	return pool.CommittedMerchantTx(ctx, billing.MerchantID(merchant), func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		for _, a := range want {
			var scope *uuid.UUID
			if a.Scope == PSP {
				scope = psp
			}
			if _, err := insertAccount(ctx, q, merchant, a, currency, nil, scope); err != nil {
				return err
			}
		}
		return nil
	})
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

// Ref names a general-ledger account: its type, and the customer or PSP its
// scope needs.
type Ref struct {
	Type     AccountType
	Customer *uuid.UUID
	PSP      *uuid.UUID
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
	Customer                               *uuid.UUID
	Order, OrderLine, Product, CreditGrant *uuid.UUID
	Invoker, Resource                      *string
}

// Post appends one general-ledger transfer, once at its coordinate. applied
// is false when the coordinate already posted the same transfer: nothing
// moved, and the row is the one that landed. A different transfer there is
// ErrCoordinateReused.
func (b *Books) Post(ctx context.Context, p Posting) (tr gen.BillingLedgerTransfer, applied bool, err error) {
	if err := p.check(); err != nil {
		return tr, false, err
	}
	debit, _, err := EnsureAccount(ctx, b.q, b.merchant, p.Debit.Type, p.Currency, p.Debit.Customer, p.Debit.PSP)
	if err != nil {
		return tr, false, err
	}
	credit, _, err := EnsureAccount(ctx, b.q, b.merchant, p.Credit.Type, p.Currency, p.Credit.Customer, p.Credit.PSP)
	if err != nil {
		return tr, false, err
	}
	at := p.EffectiveAt.UTC().Truncate(time.Microsecond)
	tr, err = b.q.InsertGeneralLedgerTransfer(ctx, gen.InsertGeneralLedgerTransferParams{
		MerchantID: b.merchant, DebitAccountID: debit, CreditAccountID: credit,
		Amount: p.Amount, Currency: p.Currency, TransferType: string(p.Type),
		Operation: string(p.Coord.Operation), Source: p.Coord.Source, SourceID: p.Coord.SourceID,
		CreditGrantID: p.CreditGrant, CustomerID: p.Customer, InvokerID: p.Invoker, Resource: p.Resource,
		OrderID: p.Order, OrderLineID: p.OrderLine, ProductID: p.Product, EffectiveAt: at,
	})
	if err == nil {
		return tr, true, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		tr, err = b.q.GetLedgerTransferAtCoordinate(ctx, gen.GetLedgerTransferAtCoordinateParams{
			MerchantID: b.merchant, Book: string(General), CustomerID: p.Customer, Currency: p.Currency, TransferType: string(p.Type),
			Operation: string(p.Coord.Operation), Source: p.Coord.Source, SourceID: p.Coord.SourceID,
			CreditGrantID: p.CreditGrant, OrderLineID: p.OrderLine,
		})
		if err != nil {
			return tr, false, fmt.Errorf("books: resolve transfer at %s: %w", p.Coord, err)
		}
		if tr.DebitAccountID != debit || tr.CreditAccountID != credit || tr.Amount != p.Amount || !tr.EffectiveAt.Equal(at) ||
			!sameUUID(tr.OrderID, p.Order) || !sameUUID(tr.ProductID, p.Product) || !sameText(tr.InvokerID, p.Invoker) || !sameText(tr.Resource, p.Resource) {
			return tr, false, fmt.Errorf("%w at %s", ErrCoordinateReused, p.Coord)
		}
		return tr, false, nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (strings.Contains(pgErr.Message, "ledger_insufficient_funds") || strings.Contains(pgErr.Message, "ledger_credit_constraint")) {
		return tr, false, fmt.Errorf("%w: %s", ErrSignRule, pgErr.Message)
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
	if p.Amount <= 0 {
		return fmt.Errorf("books: amount must be positive")
	}
	if p.EffectiveAt.IsZero() {
		return fmt.Errorf("books: effective_at is required")
	}
	if p.Customer == nil && (p.Order != nil || p.CreditGrant != nil) {
		return fmt.Errorf("books: a posting for an order or a credit lot names its customer")
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
