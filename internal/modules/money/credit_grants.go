package money

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/catalogrules"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/destructive"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money/credits"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

var (
	ErrCreditGrantNotFound    = errors.New("credit_grant_not_found")
	ErrCreditGrantUnavailable = errors.New("credit_grant_unavailable")
	ErrCreditGrantHeld        = errors.New("credit_grant_held")
)

// creditGrantFromRow reads a lot's state from its columns: a lot with nothing
// left ended by a refund when one withdrew from it, else by spending.
func creditGrantFromRow(row gen.GetCreditGrantRow, now time.Time) billing.CreditGrant {
	state := billing.CreditGrantActive
	switch {
	case row.RevokedAt != nil:
		state = billing.CreditGrantRevoked
	case row.ExpiresAt != nil && !row.ExpiresAt.After(now):
		state = billing.CreditGrantExpired
	case row.Remaining <= 0 && row.Refunded > 0:
		state = billing.CreditGrantRefunded
	case row.Remaining <= 0:
		state = billing.CreditGrantSpent
	case row.StartsAt.After(now):
		state = billing.CreditGrantScheduled
	}
	var paidCurrency *catalog.Currency
	if row.PaidCurrency != nil {
		c := catalog.Currency(*row.PaidCurrency)
		paidCurrency = &c
	}
	return billing.CreditGrant{ID: billing.CreditGrantID(row.ID), CustomerID: billing.CustomerID(row.CustomerID), Currency: catalog.Currency(row.Unit),
		Amount: row.Units, SpentAmount: row.Spent, RemainingAmount: row.Remaining, RevokedAmount: row.Revoked,
		ExpiredAmount: row.Expired, RefundedAmount: row.Refunded, State: state,
		FundingSource: billing.CreditFundingSource(row.FundingSource), PaidAmount: row.PaidAmount, PaidCurrency: paidCurrency,
		SourceID: sourceRef(credits.FundingSource(row.FundingSource), row.SourceID), Description: row.Description,
		StartsAt: row.StartsAt, ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt,
		RevokedAt: row.RevokedAt, RevokeReason: row.RevokeReason}
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
		code := normalizeCurrency(string(params.Currency))
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
	row, err := s.db.Gen(ctx).GetCreditGrant(ctx, gen.GetCreditGrantParams{MerchantID: mid.UUID(), ID: grantID})
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
// actor is the staff subject the revoke event names, "" for none.
func (s *MoneyService) RevokeCreditGrant(ctx context.Context, grantID uuid.UUID, reason, actor string) (*billing.CreditGrant, error) {
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
	args := gen.GetCreditGrantParams{MerchantID: mid.UUID(), ID: grantID}
	// The grant names its customer, whose money lock the revocation takes.
	addressed, err := s.db.Gen(ctx).GetCreditGrant(ctx, args)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCreditGrantNotFound
	}
	if err != nil {
		return nil, err
	}
	current := creditGrantFromRow(addressed, s.now())
	if current.State == billing.CreditGrantRevoked {
		current.Replayed = true
		return &current, nil
	}
	if current.State == billing.CreditGrantExpired || current.RemainingAmount <= 0 {
		return nil, ErrCreditGrantUnavailable
	}
	command, err := destructive.CommandFromContext(ctx, destructive.RevokeCredit, grantID.String(), "credit-revoke:"+grantID.String(), struct{ Reason string }{reason})
	if err != nil {
		return nil, err
	}
	payer := addressed.CustomerID
	var result *billing.CreditGrant
	err = destructive.RunLocal(ctx, s.db, s.destructiveAdmissions, command, func(ctx context.Context, _ pgx.Tx, txDB *db.DB) (bool, error) {
		changed := false
		err := func() error {

			q := txDB.Gen(ctx)
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
			if current.State == billing.CreditGrantExpired || current.RemainingAmount <= 0 {
				return ErrCreditGrantUnavailable
			}
			// What running charges accrued is spend the lots have funded: it is
			// posted before the remainder is read.
			if err := s.settleRunningCharges(ctx, q, mid.UUID(), payer, row.Unit); err != nil {
				return err
			}
			if row, err = q.GetCreditGrant(ctx, args); err != nil {
				return err
			}
			if current = creditGrantFromRow(row, s.now()); current.RemainingAmount <= 0 {
				return ErrCreditGrantUnavailable
			}
			lot, err := q.GetCreditLot(ctx, gen.GetCreditLotParams{MerchantID: mid.UUID(), ID: grantID})
			if err != nil {
				return err
			}
			// A pending refund of the lot's payment needs it whole.
			if lot.UnitsReserved > 0 {
				return ErrCreditGrantHeld
			}
			bal, err := s.deriveBalance(ctx, q, mid.UUID(), payer, row.Unit)
			if err != nil {
				return err
			}
			if current.RemainingAmount > bal.Balance {
				return ErrCreditGrantUnavailable
			}
			if bal.HeldBalance < 0 {
				return fmt.Errorf("revoke credit grant: held balance %d is negative", bal.HeldBalance)
			}
			if current.RemainingAmount > satSub(bal.Balance, bal.HeldBalance) {
				return ErrCreditGrantHeld
			}
			changed = true
			if _, err := s.creditLots(q, mid.UUID()).Revoke(ctx, grantID, reason, actor, time.Time{}); err != nil {
				return err
			}
			updated, err := q.GetCreditGrant(ctx, args)
			if err != nil {
				return err
			}
			revoked := creditGrantFromRow(updated, s.now())
			result = &revoked
			return nil
		}()
		return changed, err
	})
	return result, err
}

// sourceRef is a lot's source id, a bought lot's prefixed as its payment's,
// or nil when it has none.
func sourceRef(funding credits.FundingSource, id *string) *string {
	if id == nil {
		return nil
	}
	ref := *id
	if funding.Paid() {
		ref = catalogrules.SourceRef("purchase", *id)
	}
	return &ref
}
