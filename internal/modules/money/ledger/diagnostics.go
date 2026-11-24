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
//  1. Conservation: sum(credits_posted - debits_posted) over every account is
//     0. Cheap (O(accounts)); catches one-sided counter corruption.
//  2. Counter drift: ledger_accounts counters are a projection kept by the
//     transfer insert trigger. Disabling that trigger can skew balances while
//     conservation still holds; recomputing from ledger_transfers catches it.
//  3. Lot drift: a credit lot's units_credited and units_debited are the legs
//     on its balance account, kept by their own trigger; recomputed likewise.
//     Its units_reserved is what its payment's pending refunds reserve.
//  4. Rollup drift: ledger_type_totals and ledger_daily, recomputed from
//     ledger_transfers.
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

// IntegrityReport is the combined result. Empty means every invariant holds.
type IntegrityReport struct {
	Conservation []ConservationBreach `json:"conservation_breaches"`
	Counters     []CounterDrift       `json:"counter_drifts"`
	Lots         []LotDrift           `json:"lot_drifts"`
	Rollups      []RollupDrift        `json:"rollup_drifts"`
}

// OK reports whether the books are intact.
func (r IntegrityReport) OK() bool {
	return len(r.Conservation) == 0 && len(r.Counters) == 0 && len(r.Lots) == 0 && len(r.Rollups) == 0
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
	return r, nil
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
