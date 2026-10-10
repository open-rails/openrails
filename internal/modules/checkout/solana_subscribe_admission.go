package checkout

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

// admitSolanaSubscribeSession creates a Solana subscribe checkout under the
// customer lock, once the customer's membership slot is free. The row itself
// is the reservation: card enrollment and every other purchase see it while
// its first pull may still land, so money never moves for a slot taken.
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
		slot := intents.MembershipSlot{MerchantID: mid.UUID(), CustomerID: session.CustomerID, ProductID: price.ProductID, At: s.now().UTC(), Attempt: session.ID}
		if err := slot.Refuse(ctx, q); err != nil {
			return err
		}
		return NewCheckoutAttemptRepo(d).Create(ctx, session)
	})
}
