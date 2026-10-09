package checkout

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	solana "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// admitSolanaSubscribeSession creates a Solana subscribe checkout under the
// customer lock, after the checks card enrollment makes. The row itself is the
// reservation: card enrollment and every other subscribe see it while its
// first pull may still land, so money never moves for a slot already taken.
func (s *CheckoutAttemptService) admitSolanaSubscribeSession(ctx context.Context, session *models.CheckoutAttempt) error {
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		q := d.Gen(ctx)
		mid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		if session.PriceID == nil {
			return ErrCheckoutAttemptValidation
		}
		if err := db.EnsureCustomerRow(ctx, d.Qx(ctx), uuid.Nil, session.CustomerID); err != nil {
			return err
		}
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: session.CustomerID}); err != nil {
			return err
		}
		price, err := catalog.NewPriceService(d).GetByID(ctx, *session.PriceID)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		if conflict, err := q.GetConflictingInitialEnrollmentSubscription(ctx, gen.GetConflictingInitialEnrollmentSubscriptionParams{MerchantID: mid.UUID(), CustomerID: session.CustomerID, ProductID: price.ProductID}); err == nil {
			if conflict.Status == string(models.StatusCanceled) {
				return apperr.Conflictf("the customer's canceled subscription for this product or tier group may still bill at its provider until its stop is confirmed; resume it, or retry once the stop completes")
			}
			return apperr.Conflictf("customer already has a subscription for this product or tier group")
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err := q.GetConflictingInitialEnrollmentOperation(ctx, gen.GetConflictingInitialEnrollmentOperationParams{MerchantID: mid.UUID(), CustomerID: session.CustomerID, ProductID: price.ProductID}); err == nil {
			return apperr.Conflictf("another enrollment of this product or tier group is unresolved")
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if runway, err := q.GetPaidRunwaySubscription(ctx, gen.GetPaidRunwaySubscriptionParams{MerchantID: mid.UUID(), CustomerID: session.CustomerID, ProductID: price.ProductID, Now: now}); err == nil {
			sub, err := models.SubscriptionFromGen(runway)
			if err != nil {
				return err
			}
			return subscriptions.PaidRunwayRefusal(sub, now)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err := q.GetConflictingSubscribeAttempt(ctx, gen.GetConflictingSubscribeAttemptParams{ProductID: price.ProductID, MerchantID: mid.UUID(), CustomerID: session.CustomerID, OpenAfter: now.Add(-solana.LateSettlementWindow), ExceptID: session.ID}); err == nil {
			return apperr.Conflictf("another Solana subscription to this product or tier group awaits its first payment")
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return NewCheckoutAttemptRepo(d).Create(ctx, session)
	})
}
