package ledger

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Ledger integrity diagnostics, per (merchant, currency):
//
//  1. Conservation: sum(credits_posted - debits_posted) over every account is
//     0. Cheap (O(accounts)); catches one-sided counter corruption.
//  2. Counter drift: ledger_accounts counters are a projection kept by the
//     transfer insert trigger. Bypassing it (COPY, restore, disabled triggers)
//     silently skews every balance while conservation still holds; recomputing
//     from ledger_transfers catches it.
//
// Both take an explicitly selected merchant; fleet checks call once per merchant.

// ConservationBreach is one (merchant, currency) ledger whose account balances
// do not sum to zero.
type ConservationBreach struct {
	MerchantID uuid.UUID `json:"merchant_id"`
	Currency   string    `json:"currency"`
	Net        int64     `json:"net"` // sum(credits_posted - debits_posted); must be 0
	Accounts   int64     `json:"accounts"`
}

func (b ConservationBreach) String() string {
	return fmt.Sprintf("conservation: merchant=%s net=%s across %d accounts (must be 0)",
		b.MerchantID, moneyutil.FormatAmount(b.Net, b.Currency), b.Accounts)
}

// CounterDrift is one account whose maintained counters disagree with the sum
// of the transfers actually logged against it.
type CounterDrift struct {
	AccountID     uuid.UUID  `json:"account_id"`
	MerchantID    uuid.UUID  `json:"merchant_id"`
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

// IntegrityReport is the combined result. Empty means both invariants hold.
type IntegrityReport struct {
	Conservation []ConservationBreach `json:"conservation_breaches"`
	Counters     []CounterDrift       `json:"counter_drifts"`
}

// OK reports whether the ledger is intact.
func (r IntegrityReport) OK() bool { return len(r.Conservation) == 0 && len(r.Counters) == 0 }

// CheckIntegrity runs both diagnostics for one explicitly selected merchant.
func CheckIntegrity(ctx context.Context, q gen.DBTX, merchant uuid.UUID) (IntegrityReport, error) {
	var r IntegrityReport
	var err error
	if r.Conservation, err = CheckConservation(ctx, q, merchant); err != nil {
		return r, err
	}
	if r.Counters, err = CheckCounterDrift(ctx, q, merchant); err != nil {
		return r, err
	}
	return r, nil
}

// CheckConservation returns every (merchant, currency) ledger whose balances do
// not sum to zero. An empty slice is the healthy answer.
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
