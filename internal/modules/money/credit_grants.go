package money

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/pagination"
)

var (
	ErrCreditGrantNotFound    = errors.New("credit_grant_not_found")
	ErrCreditGrantUnavailable = errors.New("credit_grant_unavailable")
	ErrCreditGrantHeld        = errors.New("credit_grant_held")
)

func creditGrantFromRow(row gen.GetCustomerCreditGrantRow, now time.Time) billing.CreditGrant {
	state := billing.CreditGrantActive
	switch {
	case row.Termination == "revoke":
		state = billing.CreditGrantRevoked
	case row.Termination == "expire":
		state = billing.CreditGrantExpired
	case row.Termination != "":
		state = billing.CreditGrantTerminated
	case row.EndsAt != nil && !row.EndsAt.After(now):
		state = billing.CreditGrantExpired
	case row.RemainingAmount <= 0:
		state = billing.CreditGrantSpent
	case row.StartsAt.After(now):
		state = billing.CreditGrantScheduled
	}
	return billing.CreditGrant{ID: billing.CreditGrantID(row.ID), CustomerID: billing.CustomerID(row.CustomerID), Currency: row.Currency,
		Amount: row.Amount, SpentAmount: row.SpentAmount, RemainingAmount: row.RemainingAmount, RevokedAmount: row.RevokedAmount,
		ExpiredAmount: row.ExpiredAmount, State: state, SourceType: row.SourceType, SourceID: billing.SourceRef(row.SourceType, row.SourceID),
		Description: row.Reason, StartsAt: row.StartsAt, ExpiresAt: row.EndsAt, CreatedAt: row.CreatedAt,
		TerminatedAt: row.TerminatedAt, TerminationReason: row.TerminationReason}
}

// ListCreditGrants lists a customer's credit grants, newest first.
func (s *MoneyService) ListCreditGrants(ctx context.Context, payer identity.CustomerID, params billing.CreditGrantListParams) (billing.ListPage[billing.CreditGrant], error) {
	if s == nil || s.db == nil {
		return billing.ListPage[billing.CreditGrant]{}, fmt.Errorf("money service not initialized")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return billing.ListPage[billing.CreditGrant]{}, err
	}
	limit, err := pagination.Limit(params.PageRequest)
	if err != nil {
		return billing.ListPage[billing.CreditGrant]{}, err
	}
	afterAt, afterID, err := pagination.After(params.Cursor)
	if err != nil {
		return billing.ListPage[billing.CreditGrant]{}, err
	}
	var currency, sourceID *string
	if params.Currency != "" {
		code := normalizeCurrency(params.Currency)
		currency = &code
	}
	if params.SourceID != "" {
		sourceID = &params.SourceID
	}
	rows, err := s.db.Gen(ctx).ListCustomerCreditGrants(ctx, gen.ListCustomerCreditGrantsParams{
		MerchantID: mid.UUID(), CustomerID: payer.UUID(), Currency: currency, SourceID: sourceID,
		AfterAt: afterAt, AfterID: afterID, RowLimit: pagination.Fetch(limit),
	})
	if err != nil {
		return billing.ListPage[billing.CreditGrant]{}, err
	}
	now := s.now()
	grants := make([]billing.CreditGrant, 0, len(rows))
	for _, row := range rows {
		grants = append(grants, creditGrantFromRow(gen.GetCustomerCreditGrantRow(row), now))
	}
	return pagination.Cut(grants, limit, func(g billing.CreditGrant) any {
		return pagination.TimeID{At: g.CreatedAt, ID: g.ID.UUID()}
	}), nil
}

// GetCreditGrant reads one of a customer's credit grants.
func (s *MoneyService) GetCreditGrant(ctx context.Context, payer identity.CustomerID, grantID uuid.UUID) (*billing.CreditGrant, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := s.db.Gen(ctx).GetCustomerCreditGrant(ctx, gen.GetCustomerCreditGrantParams{MerchantID: mid.UUID(), CustomerID: payer.UUID(), GrantID: grantID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCreditGrantNotFound
	}
	if err != nil {
		return nil, err
	}
	grant := creditGrantFromRow(row, s.now())
	return &grant, nil
}

// RevokeCreditGrant removes the unspent remainder under the payer money lock,
// respecting the same durable reservation total as admission and spending.
func (s *MoneyService) RevokeCreditGrant(ctx context.Context, payer identity.CustomerID, grantID uuid.UUID, reason string) (*billing.CreditGrant, error) {
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
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var result *billing.CreditGrant
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
		if current.State == billing.CreditGrantRevoked {
			current.Replayed = true
			result = &current
			return nil
		}
		if row.Termination != "" || current.State == billing.CreditGrantExpired || current.RemainingAmount <= 0 {
			return ErrCreditGrantUnavailable
		}
		bal, err := s.deriveBalance(ctx, q, mid.UUID(), payer.UUID(), row.Currency)
		if err != nil {
			return err
		}
		if current.RemainingAmount > bal.Balance {
			return ErrCreditGrantUnavailable
		}
		available, err := subtractOperationCapacity(bal.Balance, bal.HeldBalance, "durable holds")
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
		revoked := creditGrantFromRow(updated, s.now())
		result = &revoked
		return nil
	})
	return result, err
}
