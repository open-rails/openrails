package ledger

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
)

// #833 ledger integrity diagnostics. Two invariants hold the double-entry
// ledger up and neither was ever computed:
//
//  1. CONSERVATION — per (merchant, currency), sum(credits_posted - debits_posted)
//     over every account is 0. Structural given the transfer model; a non-zero
//     sum means money was created or destroyed inside one ledger.
//
//  2. COUNTER DRIFT — ledger_accounts.{credits,debits}_posted is a MAINTAINED
//     PROJECTION written by the SECURITY DEFINER insert trigger, not a derived
//     view. Bypass the trigger (superuser session, COPY, restore, a migration
//     that disables triggers) and the counters diverge from ledger_transfers
//     with no error anywhere — every balance read is then silently wrong.
//     Recompute the counters from the append-only log and compare.
//
// The two are NOT redundant, and neither subsumes the other: a transfer that
// skipped the trigger entirely leaves BOTH sides untouched, so conservation
// still sums to zero and only the recompute catches it. A one-sided counter
// corruption breaks conservation, which is the far cheaper check to run often
// (O(accounts), no scan of the transfer log).
//
// Both run over whatever the supplied handle can see: on a merchant-scoped
// (RLS) connection that is one merchant; on a privileged connection, pass
// uuid.Nil to sweep the fleet.

// ConservationBreach is one (merchant, currency) ledger whose account balances
// do not sum to zero.
type ConservationBreach struct {
	MerchantID uuid.UUID `json:"merchant_id"`
	Currency   string    `json:"currency"`
	Net        int64     `json:"net"` // sum(credits_posted - debits_posted); must be 0
	Accounts   int64     `json:"accounts"`
}

func (b ConservationBreach) String() string {
	return fmt.Sprintf("conservation: merchant=%s currency=%s net=%d across %d accounts (must be 0)",
		b.MerchantID, b.Currency, b.Net, b.Accounts)
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
	return fmt.Sprintf("counter drift: account=%s (%s/%s) credits stored=%d logged=%d, debits stored=%d logged=%d",
		d.AccountID, d.AccountType, d.Currency, d.StoredCredits, d.LoggedCredits, d.StoredDebits, d.LoggedDebits)
}

// IntegrityReport is the combined result. Empty means both invariants hold.
type IntegrityReport struct {
	Conservation []ConservationBreach `json:"conservation_breaches"`
	Counters     []CounterDrift       `json:"counter_drifts"`
}

// OK reports whether the ledger is intact.
func (r IntegrityReport) OK() bool { return len(r.Conservation) == 0 && len(r.Counters) == 0 }

// CheckIntegrity runs both diagnostics. merchant uuid.Nil means "everything the
// handle can see".
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

const conservationSQL = `
SELECT merchant_id,
       currency,
       SUM(credits_posted - debits_posted)::bigint AS net,
       COUNT(*)::bigint                            AS accounts
FROM openrails.ledger_accounts
WHERE ($1::uuid IS NULL OR merchant_id = $1::uuid)
GROUP BY merchant_id, currency
HAVING SUM(credits_posted - debits_posted) <> 0
ORDER BY merchant_id, currency`

// CheckConservation returns every (merchant, currency) ledger whose balances do
// not sum to zero. An empty slice is the healthy answer.
func CheckConservation(ctx context.Context, q gen.DBTX, merchant uuid.UUID) ([]ConservationBreach, error) {
	rows, err := q.Query(ctx, conservationSQL, merchantFilter(merchant))
	if err != nil {
		return nil, fmt.Errorf("ledger: conservation check: %w", err)
	}
	defer rows.Close()

	var out []ConservationBreach
	for rows.Next() {
		var b ConservationBreach
		if err := rows.Scan(&b.MerchantID, &b.Currency, &b.Net, &b.Accounts); err != nil {
			return nil, fmt.Errorf("ledger: conservation check: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// counterDriftSQL rebuilds each account's counters from the immutable transfer
// log (one pass, credit and debit legs unioned) and returns only the accounts
// where the maintained projection disagrees.
const counterDriftSQL = `
WITH logged AS (
    SELECT account_id, SUM(credit)::bigint AS credits, SUM(debit)::bigint AS debits
    FROM (
        SELECT credit_account_id AS account_id, amount AS credit, 0::bigint AS debit
        FROM openrails.ledger_transfers
        WHERE ($1::uuid IS NULL OR merchant_id = $1::uuid)
        UNION ALL
        SELECT debit_account_id, 0::bigint, amount
        FROM openrails.ledger_transfers
        WHERE ($1::uuid IS NULL OR merchant_id = $1::uuid)
    ) legs
    GROUP BY account_id
)
SELECT a.id, a.merchant_id, a.currency, a.account_type, a.customer_id,
       a.credits_posted, COALESCE(l.credits, 0)::bigint,
       a.debits_posted,  COALESCE(l.debits, 0)::bigint
FROM openrails.ledger_accounts a
LEFT JOIN logged l ON l.account_id = a.id
WHERE ($1::uuid IS NULL OR a.merchant_id = $1::uuid)
  AND (a.credits_posted <> COALESCE(l.credits, 0)
    OR a.debits_posted  <> COALESCE(l.debits, 0))
ORDER BY a.merchant_id, a.currency, a.id`

// CheckCounterDrift recomputes every account's counters from ledger_transfers
// and returns the accounts whose stored projection disagrees. An empty slice is
// the healthy answer.
func CheckCounterDrift(ctx context.Context, q gen.DBTX, merchant uuid.UUID) ([]CounterDrift, error) {
	rows, err := q.Query(ctx, counterDriftSQL, merchantFilter(merchant))
	if err != nil {
		return nil, fmt.Errorf("ledger: counter drift check: %w", err)
	}
	defer rows.Close()

	var out []CounterDrift
	for rows.Next() {
		var d CounterDrift
		if err := rows.Scan(&d.AccountID, &d.MerchantID, &d.Currency, &d.AccountType, &d.CustomerID,
			&d.StoredCredits, &d.LoggedCredits, &d.StoredDebits, &d.LoggedDebits); err != nil {
			return nil, fmt.Errorf("ledger: counter drift check: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// merchantFilter maps the nil UUID onto a SQL NULL ("no merchant predicate").
func merchantFilter(merchant uuid.UUID) *uuid.UUID {
	if merchant == uuid.Nil {
		return nil
	}
	m := merchant
	return &m
}
