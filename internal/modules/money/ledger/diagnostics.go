package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Integrity diagnostics of both books, per (merchant, book, currency):
//
//  1. Conservation: every balance sums to 0, an account's counters or, for a
//     merchant-wide general-ledger account, its type total. Cheap
//     (O(accounts)); catches one-sided counter corruption.
//  2. Counter drift: ledger_accounts counters are a projection kept by the
//     transfer insert trigger. Disabling that trigger can skew balances while
//     conservation still holds; recomputing from ledger_transfers catches it.
//  3. Lot drift: a credit lot's units_credited and units_debited are the legs
//     on its balance account, kept by their own trigger; recomputed likewise.
//     Its units_reserved is what its payment's pending refunds reserve.
//  4. Rollup drift: ledger_type_totals and ledger_daily, recomputed from
//     ledger_transfers.
//  5. Sign breaches: a general-ledger account across its sign rule, a
//     customer's by its counters, a merchant-wide one by its type total. The
//     general ledger never refuses one: it raises books.sign_breach.
//  6. Side warnings: one with no sign rule whose type total sits opposite its
//     class's normal side. Reported, never a failure.
//
// Each takes an explicitly selected merchant; ledger-audit calls once per merchant.

// ConservationBreach is one (merchant, book, currency) ledger whose account
// balances do not sum to zero.
type ConservationBreach struct {
	MerchantID uuid.UUID `json:"merchant_id"`
	Book       string    `json:"book"`
	Currency   string    `json:"currency"`
	Net        int64     `json:"net"` // sum(credits_posted - debits_posted); must be 0
	Accounts   int64     `json:"accounts"`
}

func (b ConservationBreach) String() string {
	return fmt.Sprintf("conservation: merchant=%s book=%s net=%s across %d accounts (must be 0)",
		b.MerchantID, b.Book, moneyutil.FormatAmount(b.Net, b.Currency), b.Accounts)
}

// CounterDrift is one account whose maintained counters disagree with the sum
// of the transfers actually logged against it.
type CounterDrift struct {
	AccountID     uuid.UUID  `json:"account_id"`
	MerchantID    uuid.UUID  `json:"merchant_id"`
	Book          string     `json:"book"`
	Currency      string     `json:"currency"`
	AccountType   string     `json:"account_type"`
	CustomerID    *uuid.UUID `json:"customer_id,omitempty"`
	StoredCredits int64      `json:"stored_credits"`
	LoggedCredits int64      `json:"logged_credits"`
	StoredDebits  int64      `json:"stored_debits"`
	LoggedDebits  int64      `json:"logged_debits"`
}

func (d CounterDrift) String() string {
	return fmt.Sprintf("counter drift: account=%s (%s) credits stored=%s logged=%s, debits stored=%s logged=%s",
		d.AccountID, d.AccountType, moneyutil.FormatAmount(d.StoredCredits, d.Currency), moneyutil.FormatAmount(d.LoggedCredits, d.Currency),
		moneyutil.FormatAmount(d.StoredDebits, d.Currency), moneyutil.FormatAmount(d.LoggedDebits, d.Currency))
}

// LotDrift is one credit lot whose counters disagree with its logged legs, or
// whose reservation with its payment's pending refunds.
type LotDrift struct {
	CreditGrantID    uuid.UUID `json:"credit_grant_id"`
	MerchantID       uuid.UUID `json:"merchant_id"`
	CustomerID       uuid.UUID `json:"customer_id"`
	Unit             string    `json:"unit"`
	StoredCredited   int64     `json:"stored_credited"`
	LoggedCredited   int64     `json:"logged_credited"`
	StoredDebited    int64     `json:"stored_debited"`
	LoggedDebited    int64     `json:"logged_debited"`
	StoredReserved   int64     `json:"stored_reserved"`
	ExpectedReserved int64     `json:"expected_reserved"`
}

func (d LotDrift) String() string {
	return fmt.Sprintf("lot drift: credit_grant=%s credited stored=%s logged=%s, debited stored=%s logged=%s, reserved stored=%s expected=%s",
		d.CreditGrantID, moneyutil.FormatAmount(d.StoredCredited, d.Unit), moneyutil.FormatAmount(d.LoggedCredited, d.Unit),
		moneyutil.FormatAmount(d.StoredDebited, d.Unit), moneyutil.FormatAmount(d.LoggedDebited, d.Unit),
		moneyutil.FormatAmount(d.StoredReserved, d.Unit), moneyutil.FormatAmount(d.ExpectedReserved, d.Unit))
}

// RollupDrift is one ledger_type_totals row (Bucket nil) or ledger_daily
// bucket whose kept amounts disagree with the transfers logged into it. A
// missing row keeps 0.
type RollupDrift struct {
	Book          string     `json:"book"`
	Currency      string     `json:"currency"`
	AccountType   string     `json:"account_type"`
	PSPID         *uuid.UUID `json:"psp_id,omitempty"`
	Bucket        *time.Time `json:"bucket,omitempty"`
	TransferType  string     `json:"transfer_type,omitempty"`
	ProductID     *uuid.UUID `json:"product_id,omitempty"`
	StoredCredits int64      `json:"stored_credits"`
	LoggedCredits int64      `json:"logged_credits"`
	StoredDebits  int64      `json:"stored_debits"`
	LoggedDebits  int64      `json:"logged_debits"`
}

func (d RollupDrift) String() string {
	where := "ledger_type_totals"
	if d.Bucket != nil {
		where = fmt.Sprintf("ledger_daily %s %s", d.Bucket.UTC().Format(time.RFC3339), d.TransferType)
	}
	return fmt.Sprintf("rollup drift: %s %s/%s credits stored=%s logged=%s, debits stored=%s logged=%s",
		where, d.Book, d.AccountType, moneyutil.FormatAmount(d.StoredCredits, d.Currency), moneyutil.FormatAmount(d.LoggedCredits, d.Currency),
		moneyutil.FormatAmount(d.StoredDebits, d.Currency), moneyutil.FormatAmount(d.LoggedDebits, d.Currency))
}

// SignBreach is a general-ledger account across the side its sign rule
// holds it to: a customer's by its counters, a merchant-wide one by its type
// total.
type SignBreach struct {
	Currency      string     `json:"currency"`
	AccountType   string     `json:"account_type"`
	PSPID         *uuid.UUID `json:"psp_id,omitempty"`
	CustomerID    *uuid.UUID `json:"customer_id,omitempty"`
	CreditsPosted int64      `json:"credits_posted"`
	DebitsPosted  int64      `json:"debits_posted"`
}

func (b SignBreach) String() string {
	who := ""
	if b.CustomerID != nil {
		who = " customer=" + b.CustomerID.String()
	}
	return fmt.Sprintf("sign: general/%s%s balance=%s against its sign rule",
		b.AccountType, who, moneyutil.FormatAmount(b.CreditsPosted-b.DebitsPosted, b.Currency))
}

// SideWarning is a merchant-wide general-ledger account with no sign rule
// whose type total sits opposite its class's normal side.
type SideWarning struct {
	Currency      string     `json:"currency"`
	AccountType   string     `json:"account_type"`
	Class         string     `json:"class"`
	PSPID         *uuid.UUID `json:"psp_id,omitempty"`
	CreditsPosted int64      `json:"credits_posted"`
	DebitsPosted  int64      `json:"debits_posted"`
}

func (w SideWarning) String() string {
	return fmt.Sprintf("side: general/%s balance=%s opposite its %s class's normal side (warning)",
		w.AccountType, moneyutil.FormatAmount(w.CreditsPosted-w.DebitsPosted, w.Currency), w.Class)
}

// IntegrityReport is the combined result. Empty but for Sides means every
// invariant holds.
type IntegrityReport struct {
	Conservation []ConservationBreach `json:"conservation_breaches"`
	Counters     []CounterDrift       `json:"counter_drifts"`
	Lots         []LotDrift           `json:"lot_drifts"`
	Rollups      []RollupDrift        `json:"rollup_drifts"`
	Signs        []SignBreach         `json:"sign_breaches"`
	// Sides are warnings: they never make the report fail.
	Sides []SideWarning `json:"side_warnings"`
}

// OK reports whether the books are intact; side warnings do not count.
func (r IntegrityReport) OK() bool {
	return len(r.Conservation) == 0 && len(r.Counters) == 0 && len(r.Lots) == 0 && len(r.Rollups) == 0 && len(r.Signs) == 0
}

// CheckIntegrity runs every diagnostic for one explicitly selected merchant.
func CheckIntegrity(ctx context.Context, q gen.DBTX, merchant uuid.UUID) (IntegrityReport, error) {
	var r IntegrityReport
	var err error
	if r.Conservation, err = CheckConservation(ctx, q, merchant); err != nil {
		return r, err
	}
	if r.Counters, err = CheckCounterDrift(ctx, q, merchant); err != nil {
		return r, err
	}
	if r.Lots, err = CheckLotDrift(ctx, q, merchant); err != nil {
		return r, err
	}
	if r.Rollups, err = CheckRollupDrift(ctx, q, merchant); err != nil {
		return r, err
	}
	if r.Signs, err = CheckSignBreaches(ctx, q, merchant); err != nil {
		return r, err
	}
	if r.Sides, err = CheckSides(ctx, q, merchant); err != nil {
		return r, err
	}
	return r, nil
}

// CheckSides returns the merchant-wide general-ledger accounts with no sign
// rule whose type total sits opposite their class's normal side.
func CheckSides(ctx context.Context, q gen.DBTX, merchant uuid.UUID) ([]SideWarning, error) {
	if merchant == uuid.Nil {
		return nil, fmt.Errorf("ledger: merchant is required")
	}
	rows, err := gen.New(q).ListLedgerSideWarnings(ctx, merchant)
	if err != nil {
		return nil, fmt.Errorf("ledger: side check: %w", err)
	}
	var out []SideWarning
	for _, row := range rows {
		out = append(out, SideWarning{Currency: row.Currency, AccountType: row.AccountType, Class: row.Class, PSPID: row.PspID,
			CreditsPosted: row.CreditsPosted, DebitsPosted: row.DebitsPosted})
	}
	return out, nil
}

// CheckSignBreaches returns the general-ledger accounts across their sign
// rule. An empty slice is the healthy answer.
func CheckSignBreaches(ctx context.Context, q gen.DBTX, merchant uuid.UUID) ([]SignBreach, error) {
	if merchant == uuid.Nil {
		return nil, fmt.Errorf("ledger: merchant is required")
	}
	rows, err := gen.New(q).ListLedgerSignBreaches(ctx, merchant)
	if err != nil {
		return nil, fmt.Errorf("ledger: sign check: %w", err)
	}
	var out []SignBreach
	for _, row := range rows {
		out = append(out, SignBreach{Currency: row.Currency, AccountType: row.AccountType, PSPID: row.PspID, CustomerID: row.CustomerID,
			CreditsPosted: row.CreditsPosted, DebitsPosted: row.DebitsPosted})
	}
	return out, nil
}

// CheckLotDrift recomputes every credit lot's counters from the legs on its
// balance account, and its reservation from its payment's pending refunds,
// and returns the lots whose stored values disagree. An empty slice is the
// healthy answer; convergence repairs a reservation.
func CheckLotDrift(ctx context.Context, q gen.DBTX, merchant uuid.UUID) ([]LotDrift, error) {
	if merchant == uuid.Nil {
		return nil, fmt.Errorf("ledger: merchant is required")
	}
	rows, err := gen.New(q).ListCreditLotDrifts(ctx, merchant)
	if err != nil {
		return nil, fmt.Errorf("ledger: lot drift check: %w", err)
	}
	var out []LotDrift
	for _, row := range rows {
		out = append(out, LotDrift{
			CreditGrantID: row.CreditGrantID, MerchantID: row.MerchantID, CustomerID: row.CustomerID, Unit: row.Unit,
			StoredCredited: row.StoredCredited, LoggedCredited: row.LoggedCredited,
			StoredDebited: row.StoredDebited, LoggedDebited: row.LoggedDebited,
			StoredReserved: row.StoredReserved, ExpectedReserved: row.ExpectedReserved,
		})
	}
	return out, nil
}

// CheckConservation returns every (merchant, book, currency) ledger whose
// balances do not sum to zero. An empty slice is the healthy answer.
func CheckConservation(ctx context.Context, q gen.DBTX, merchant uuid.UUID) ([]ConservationBreach, error) {
	if merchant == uuid.Nil {
		return nil, fmt.Errorf("ledger: merchant is required")
	}
	rows, err := gen.New(q).ListLedgerConservationBreaches(ctx, merchant)
	if err != nil {
		return nil, fmt.Errorf("ledger: conservation check: %w", err)
	}

	var out []ConservationBreach
	for _, row := range rows {
		out = append(out, ConservationBreach{
			MerchantID: row.MerchantID,
			Book:       row.Book,
			Currency:   row.Currency,
			Net:        row.Net,
			Accounts:   row.Accounts,
		})
	}
	return out, nil
}

// CheckCounterDrift recomputes every account's counters from ledger_transfers
// and returns the accounts whose stored projection disagrees. An empty slice is
// the healthy answer.
func CheckCounterDrift(ctx context.Context, q gen.DBTX, merchant uuid.UUID) ([]CounterDrift, error) {
	if merchant == uuid.Nil {
		return nil, fmt.Errorf("ledger: merchant is required")
	}
	rows, err := gen.New(q).ListLedgerCounterDrifts(ctx, merchant)
	if err != nil {
		return nil, fmt.Errorf("ledger: counter drift check: %w", err)
	}

	var out []CounterDrift
	for _, row := range rows {
		out = append(out, CounterDrift{
			AccountID:     row.AccountID,
			MerchantID:    row.MerchantID,
			Book:          row.Book,
			Currency:      row.Currency,
			AccountType:   row.AccountType,
			CustomerID:    row.CustomerID,
			StoredCredits: row.StoredCredits,
			LoggedCredits: row.LoggedCredits,
			StoredDebits:  row.StoredDebits,
			LoggedDebits:  row.LoggedDebits,
		})
	}
	return out, nil
}

// CheckRollupDrift recomputes ledger_type_totals and ledger_daily from
// ledger_transfers and returns every row whose kept amounts disagree. An
// empty slice is the healthy answer.
func CheckRollupDrift(ctx context.Context, q gen.DBTX, merchant uuid.UUID) ([]RollupDrift, error) {
	if merchant == uuid.Nil {
		return nil, fmt.Errorf("ledger: merchant is required")
	}
	totals, err := gen.New(q).ListLedgerTypeTotalDrifts(ctx, merchant)
	if err != nil {
		return nil, fmt.Errorf("ledger: total drift check: %w", err)
	}
	days, err := gen.New(q).ListLedgerDailyDrifts(ctx, merchant)
	if err != nil {
		return nil, fmt.Errorf("ledger: daily drift check: %w", err)
	}
	var out []RollupDrift
	for _, row := range totals {
		out = append(out, RollupDrift{
			Book: row.Book, Currency: row.Currency, AccountType: row.AccountType, PSPID: row.PspID,
			StoredCredits: row.StoredCredits, LoggedCredits: row.LoggedCredits,
			StoredDebits: row.StoredDebits, LoggedDebits: row.LoggedDebits,
		})
	}
	for _, row := range days {
		bucket := row.BucketAt
		out = append(out, RollupDrift{
			Book: row.Book, Currency: row.Currency, AccountType: row.AccountType, PSPID: row.PspID,
			Bucket: &bucket, TransferType: row.TransferType, ProductID: row.ProductID,
			StoredCredits: row.StoredCredits, LoggedCredits: row.LoggedCredits,
			StoredDebits: row.StoredDebits, LoggedDebits: row.LoggedDebits,
		})
	}
	return out, nil
}
