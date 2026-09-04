package money

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

var (
	ErrCreditGrantNotFound    = errors.New("credit_grant_not_found")
	ErrCreditGrantUnavailable = errors.New("credit_grant_unavailable")
	ErrCreditGrantHeld        = errors.New("credit_grant_held")
)

// CreditGrant is a derived support view, never another balance authority.
type CreditGrant struct {
	ID                uuid.UUID  `json:"id"`
	CustomerID        uuid.UUID  `json:"customer_id"`
	Currency          string     `json:"currency"`
	Amount            int64      `json:"amount"`
	SpentAmount       int64      `json:"spent_amount"`
	RemainingAmount   int64      `json:"remaining_amount"`
	RevokedAmount     int64      `json:"revoked_amount"`
	ExpiredAmount     int64      `json:"expired_amount"`
	State             string     `json:"state"`
	SourceType        string     `json:"source_type"`
	SourceID          string     `json:"source_id"`
	Reason            *string    `json:"reason,omitempty"`
	StartsAt          time.Time  `json:"starts_at"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	TerminatedAt      *time.Time `json:"terminated_at,omitempty"`
	TerminationReason *string    `json:"termination_reason,omitempty"`
}

type CreditGrantPage struct {
	UnitDecimals int           `json:"unit_decimals"`
	Grants       []CreditGrant `json:"grants"`
	Total        int64         `json:"total"`
	Limit        int           `json:"limit"`
	Offset       int           `json:"offset"`
}

type CreditGrantRevocation struct {
	Grant    CreditGrant `json:"grant"`
	Replayed bool        `json:"replayed"`
}

func creditGrantFromRow(row gen.GetCustomerCreditGrantRow, now time.Time) CreditGrant {
	state := "active"
	switch {
	case row.Termination == "revoke":
		state = "revoked"
	case row.Termination == "expire":
		state = "expired"
	case row.Termination != "":
		state = "terminated"
	case row.EndsAt != nil && !row.EndsAt.After(now):
		state = "expired"
	case row.RemainingAmount <= 0:
		state = "spent"
	case row.StartsAt.After(now):
		state = "scheduled"
	}
	return CreditGrant{ID: row.ID, CustomerID: row.CustomerID, Currency: row.Currency, Amount: row.Amount,
		SpentAmount: row.SpentAmount, RemainingAmount: row.RemainingAmount, RevokedAmount: row.RevokedAmount, ExpiredAmount: row.ExpiredAmount,
		State: state, SourceType: row.SourceType, SourceID: row.SourceID, Reason: row.Reason, StartsAt: row.StartsAt, ExpiresAt: row.EndsAt,
		CreatedAt: row.CreatedAt, TerminatedAt: row.TerminatedAt, TerminationReason: row.TerminationReason}
}

func (s *MoneyService) ListCreditGrants(ctx context.Context, payer identity.CustomerID, currency string, limit, offset int) (*CreditGrantPage, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	currency = normalizeUnit(currency)
	decimals, _, err := s.ResolveUnit(ctx, currency)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	if offset < 0 || offset > math.MaxInt32 {
		return nil, fmt.Errorf("invalid offset")
	}
	q := s.db.Gen(ctx)
	total, err := q.CountCustomerCreditGrants(ctx, gen.CountCustomerCreditGrantsParams{MerchantID: mid.UUID(), CustomerID: payer.UUID(), Currency: currency})
	if err != nil {
		return nil, err
	}
	rows, err := q.ListCustomerCreditGrants(ctx, gen.ListCustomerCreditGrantsParams{MerchantID: mid.UUID(), CustomerID: payer.UUID(), Currency: currency, PageLimit: int32(limit), PageOffset: int32(offset)})
	if err != nil {
		return nil, err
	}
	page := &CreditGrantPage{UnitDecimals: decimals, Grants: make([]CreditGrant, 0, len(rows)), Total: total, Limit: limit, Offset: offset}
	for _, row := range rows {
		page.Grants = append(page.Grants, creditGrantFromRow(gen.GetCustomerCreditGrantRow(row), s.now()))
	}
	return page, nil
}

// RevokeCreditGrant removes the whole unspent remainder under the existing
// customer money lock. The reader must inspect live Redis admission holds while
// that lock is held, matching admission's PG -> Redis ordering. Failure to read
// holds fails closed. Repeating a completed revoke returns its original result.
func (s *MoneyService) RevokeCreditGrant(ctx context.Context, payer identity.CustomerID, grantID uuid.UUID, reason string, readAdmissionHeld func(context.Context, string) (int64, error)) (*CreditGrantRevocation, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() || grantID == uuid.Nil {
		return nil, ErrCreditGrantNotFound
	}
	reason = strings.TrimSpace(reason)
	if reason == "" || utf8.RuneCountInString(reason) > 500 {
		return nil, fmt.Errorf("reason is required (maximum 500 characters)")
	}
	if readAdmissionHeld == nil {
		return nil, fmt.Errorf("live admission hold reader is required")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var result *CreditGrantRevocation
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		// Lock only an existing customer. A failed grant address must not create one.
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: payer.UUID()}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrCreditGrantNotFound
			}
			return err
		}
		args := gen.GetCustomerCreditGrantParams{MerchantID: mid.UUID(), CustomerID: payer.UUID(), GrantID: grantID}
		row, err := q.GetCustomerCreditGrant(ctx, args)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCreditGrantNotFound
		}
		if err != nil {
			return err
		}
		current := creditGrantFromRow(row, s.now())
		if current.State == "revoked" {
			result = &CreditGrantRevocation{Grant: current, Replayed: true}
			return nil
		}
		if row.Termination != "" || current.State == "expired" || current.RemainingAmount <= 0 {
			return ErrCreditGrantUnavailable
		}
		bal, err := s.deriveBalance(ctx, q, mid.UUID(), payer.UUID(), row.Currency)
		if err != nil {
			return err
		}
		if current.RemainingAmount > bal.Balance {
			return ErrCreditGrantUnavailable
		}
		held, err := readAdmissionHeld(ctx, row.Currency)
		if err != nil {
			return fmt.Errorf("read live admission holds: %w", err)
		}
		if held < 0 {
			return fmt.Errorf("negative admission holds")
		}
		available, err := subtractOperationCapacity(bal.Balance, bal.HeldBalance, "durable holds")
		if err != nil {
			return err
		}
		available, err = subtractOperationCapacity(available, held, "admission holds")
		if err != nil {
			return err
		}
		if current.RemainingAmount > available {
			return ErrCreditGrantHeld
		}
		ledger := grants.New(q, mid.UUID())
		ledger.SetClock(s.now)
		if _, err := ledger.Revoke(ctx, grantID, reason); err != nil {
			return err
		}
		original, err := q.GetGrant(ctx, gen.GetGrantParams{MerchantID: mid.UUID(), ID: grantID})
		if err != nil {
			return err
		}
		if err := ledger.MaterializeGrant(ctx, original); err != nil {
			return err
		}
		updated, err := q.GetCustomerCreditGrant(ctx, args)
		if err != nil {
			return err
		}
		result = &CreditGrantRevocation{Grant: creditGrantFromRow(updated, s.now())}
		return nil
	})
	return result, err
}
