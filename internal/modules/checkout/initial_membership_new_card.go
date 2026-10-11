package checkout

import (
	"context"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	log "github.com/sirupsen/logrus"
)

// initialMembershipVaultedMethodKey names a method this session vaulted from
// a card token; a declined enrollment removes it.
const initialMembershipVaultedMethodKey = "initial_membership_vaulted_method"

// discardCard removes a card a purchase saved from the buyer's token, in
// OpenRails and at the processor.
func discardCard(ctx context.Context, cards *paymentmethods.RailPaymentMethodService, method *models.PaymentMethod) {
	if method == nil || cards == nil {
		return
	}
	if err := cards.CleanupPaymentMethodBestEffort(context.WithoutCancel(ctx), method); err != nil {
		log.WithError(err).WithField("payment_method_id", method.ID).Warn("discard declined card")
	}
}

// declinedCard is the card a purchase saved from the buyer's token, read in
// the transaction that records its decline: whichever executor records the
// decline discards it once that commits. nil when it is already gone.
func declinedCard(ctx context.Context, d *db.DB, merchantID, methodID uuid.UUID) (*models.PaymentMethod, error) {
	row, err := d.Gen(ctx).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: merchantID, ID: methodID})
	if db.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return models.PaymentMethodFromGen(row)
}

// enrollmentCard is declinedCard for an enrollment whose checkout attempt
// vaulted its card from a token; nil for a card the buyer already had.
func enrollmentCard(ctx context.Context, d *db.DB, merchantID uuid.UUID, p InitialMembershipPayload) (*models.PaymentMethod, error) {
	if p.CheckoutAttemptID == nil {
		return nil, nil
	}
	session, err := NewCheckoutAttemptRepo(d).GetByID(ctx, *p.CheckoutAttemptID)
	if err != nil {
		return nil, err
	}
	if vaulted, _ := session.RailState[initialMembershipVaultedMethodKey].(string); vaulted != p.Terms.PaymentMethodID.String() {
		return nil, nil
	}
	return declinedCard(ctx, d, merchantID, p.Terms.PaymentMethodID)
}
