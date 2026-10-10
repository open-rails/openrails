package checkout

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/attempts"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	log "github.com/sirupsen/logrus"
)

// initialMembershipVaultedMethodKey names a method this session vaulted from
// a card token; a declined enrollment removes it.
const initialMembershipVaultedMethodKey = "initial_membership_vaulted_method"

type enrollmentCardVault interface {
	vaultEnrollmentCard(ctx context.Context, req *CheckoutRequest, user *UserIdentity, target railTarget) (*models.PaymentMethod, error)
	discardEnrollmentCard(ctx context.Context, method *models.PaymentMethod)
}

func (s *CheckoutService) vaultEnrollmentCard(ctx context.Context, req *CheckoutRequest, user *UserIdentity, target railTarget) (*models.PaymentMethod, error) {
	if s.PaymentMethodResolver == nil {
		return nil, errors.New("payment method service unavailable")
	}
	_, _, method, _, err := s.PaymentMethodResolver.ResolvePaymentMethod(ctx, req, user, target)
	if err != nil {
		return nil, err
	}
	if method == nil {
		return nil, errors.New("card could not be saved")
	}
	return method, nil
}

func (s *CheckoutService) discardEnrollmentCard(ctx context.Context, method *models.PaymentMethod) {
	discardCard(ctx, s.RailPaymentMethodService, method)
}

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

func (s *CheckoutAttemptService) vaultEnrollmentCard(ctx context.Context, payment *CheckoutAttemptPaymentRequest, session *models.CheckoutAttempt, target railTarget, user *UserIdentity) (*models.PaymentMethod, error) {
	vault, ok := s.checkoutService.(enrollmentCardVault)
	if !ok {
		return nil, errors.New("card vault unavailable")
	}
	req := &CheckoutRequest{
		PaymentToken: payment.PaymentToken,
		Card:         payment.Card,
		Rail:         target.PSP,
		Metadata:     session.Metadata,
		Email:        payment.Email,
		NameOnCard:   payment.NameOnCard,
		FirstName:    payment.FirstName,
		LastName:     payment.LastName,
		Address1:     payment.Address1,
		Address2:     payment.Address2,
		Phone:        payment.Phone,
		City:         payment.City,
		State:        payment.State,
		Zip:          payment.Zip,
		Country:      payment.Country,
		LastFour:     payment.LastFour,
		CardType:     payment.CardType,
		ExpiryDate:   payment.ExpiryDate,
	}
	if session.PriceID != nil {
		req.attempt = cardAttempt{target: session.PriceID.String(), owner: attempts.OwnerEngine}
	}
	return vault.vaultEnrollmentCard(ctx, req, user, target)
}

func (s *CheckoutAttemptService) discardEnrollmentCard(ctx context.Context, method *models.PaymentMethod) {
	if vault, ok := s.checkoutService.(enrollmentCardVault); ok && method != nil {
		vault.discardEnrollmentCard(context.WithoutCancel(ctx), method)
	}
}
