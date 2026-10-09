package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

var (
	ErrCreditGrantNotFound    = errors.New("credit_grant_not_found")
	ErrCreditGrantUnavailable = errors.New("credit_grant_unavailable")
	ErrCreditGrantHeld        = errors.New("credit_grant_held")
)

func creditGrantFromRow(row gen.GetCreditGrantRow, now time.Time) billing.CreditGrant {
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
		ExpiredAmount: row.ExpiredAmount, State: state, SourceType: row.SourceType, SourceID: sourceRef(row.SourceType, row.SourceID),
		Description: row.Reason, StartsAt: row.StartsAt, ExpiresAt: row.EndsAt, CreatedAt: row.CreatedAt,
		TerminatedAt: row.TerminatedAt, TerminationReason: row.TerminationReason}
}

// ListCreditGrants lists a customer's credit grants, newest first.
func (s *MoneyService) ListCreditGrants(ctx context.Context, params billing.CreditGrantListParams) (billing.ListPage[billing.CreditGrant], error) {
	if s == nil || s.db == nil {
		return billing.ListPage[billing.CreditGrant]{}, fmt.Errorf("money service not initialized")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return billing.ListPage[billing.CreditGrant]{}, err
	}
	if params.IDs != nil {
		rows, err := s.db.Gen(ctx).ListCreditGrantsByIDs(ctx, gen.ListCreditGrantsByIDsParams{MerchantID: mid.UUID(), Ids: uuidutil.Of(params.IDs)})
		if err != nil {
			return billing.ListPage[billing.CreditGrant]{}, err
		}
		var page billing.ListPage[billing.CreditGrant]
		for _, row := range rows {
			page.Items = append(page.Items, creditGrantFromRow(gen.GetCreditGrantRow(row), s.now()))
		}
		return page, nil
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
		MerchantID: mid.UUID(), CustomerID: params.CustomerID.UUID(), Currency: currency, SourceID: sourceID,
		AfterAt: afterAt, AfterID: afterID, RowLimit: pagination.Fetch(limit),
	})
	if err != nil {
		return billing.ListPage[billing.CreditGrant]{}, err
	}
	now := s.now()
	grants := make([]billing.CreditGrant, 0, len(rows))
	for _, row := range rows {
		grants = append(grants, creditGrantFromRow(gen.GetCreditGrantRow(row), now))
	}
	return pagination.Cut(grants, limit, func(g billing.CreditGrant) any {
		return pagination.TimeID{At: g.CreatedAt, ID: g.ID.UUID()}
	}), nil
}

// GetCreditGrant reads one of a customer's credit grants.
func (s *MoneyService) GetCreditGrant(ctx context.Context, grantID uuid.UUID) (*billing.CreditGrant, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := s.db.Gen(ctx).GetCreditGrant(ctx, gen.GetCreditGrantParams{MerchantID: mid.UUID(), GrantID: grantID})
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
func (s *MoneyService) RevokeCreditGrant(ctx context.Context, grantID uuid.UUID, reason string) (*billing.CreditGrant, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if grantID == uuid.Nil {
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
	args := gen.GetCreditGrantParams{MerchantID: mid.UUID(), GrantID: grantID}
	// The grant names its customer, whose money lock the revocation takes.
	addressed, err := s.db.Gen(ctx).GetCreditGrant(ctx, args)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCreditGrantNotFound
	}
	if err != nil {
		return nil, err
	}
	payer := addressed.CustomerID
	var result *billing.CreditGrant
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: payer}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrCreditGrantNotFound
			}
			return err
		}
		row, err := q.GetCreditGrant(ctx, args)
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
		original, err := q.GetGrant(ctx, gen.GetGrantParams{MerchantID: mid.UUID(), ID: grantID})
		if err != nil {
			return err
		}
		var provenance grants.Spec
		if len(original.SpecSnapshot) > 0 {
			if err := json.Unmarshal(original.SpecSnapshot, &provenance); err != nil {
				return err
			}
		}
		if original.PaymentID != nil && provenance.Deposit != nil && provenance.Deposit.PaidAmount != nil {
			pending, err := q.GetPurchasedCreditPendingRefunds(ctx, gen.GetPurchasedCreditPendingRefundsParams{MerchantID: mid.UUID(), PaymentID: *original.PaymentID})
			if err != nil {
				return err
			}
			if pending > 0 {
				return ErrCreditGrantHeld
			}
		}
		bal, err := s.deriveBalance(ctx, q, mid.UUID(), payer, row.Currency)
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
		if err := ledger.MaterializeGrant(ctx, original); err != nil {
			return err
		}
		updated, err := q.GetCreditGrant(ctx, args)
		if err != nil {
			return err
		}
		revoked := creditGrantFromRow(updated, s.now())
		result = &revoked
		return nil
	})
	return result, err
}

// sourceRef is a grant's prefixed source id, or nil when it has none.
func sourceRef(sourceType string, id *string) *string {
	if id == nil {
		return nil
	}
	ref := billing.SourceRef(sourceType, *id)
	return &ref
}
