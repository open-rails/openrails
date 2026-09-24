package checkout

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/pkg/billingauth"
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
	if method == nil || s.RailPaymentMethodService == nil {
		return
	}
	if err := s.RailPaymentMethodService.CleanupPaymentMethodBestEffort(ctx, method); err != nil {
		log.WithError(err).WithField("payment_method_id", method.ID).Warn("discard enrollment card")
	}
}

func (s *CheckoutSessionService) vaultEnrollmentCard(ctx context.Context, payment *CheckoutSessionPaymentRequest, session *models.CheckoutSession, target railTarget, user *UserIdentity) (*models.PaymentMethod, error) {
	vault, ok := s.checkoutService.(enrollmentCardVault)
	if !ok {
		return nil, errors.New("card vault unavailable")
	}
	req := &CheckoutRequest{
		PaymentToken: payment.PaymentToken,
		Rail:         target.PSP,
		Metadata:     session.Metadata,
		Email:        payment.Email,
		NameOnCard:   payment.NameOnCard,
		FirstName:    payment.FirstName,
		LastName:     payment.LastName,
		Address1:     payment.Address1,
		City:         payment.City,
		State:        payment.State,
		Zip:          payment.Zip,
		Country:      payment.Country,
		LastFour:     payment.LastFour,
		CardType:     payment.CardType,
		ExpiryDate:   payment.ExpiryDate,
	}
	return vault.vaultEnrollmentCard(ctx, req, user, target)
}

func (s *CheckoutSessionService) discardEnrollmentCard(ctx context.Context, method *models.PaymentMethod) {
	if vault, ok := s.checkoutService.(enrollmentCardVault); ok && method != nil {
		vault.discardEnrollmentCard(context.WithoutCancel(ctx), method)
	}
}

// acceptQuoteOnCreate confirms a just-quoted membership for the present payer.
// A definite decline removes a card this session vaulted from a token.
func (s *CheckoutSessionService) acceptQuoteOnCreate(ctx context.Context, quoted *CheckoutSessionResponse, user *UserIdentity, payer billingauth.DelegatedPrincipal) (*CheckoutSessionResponse, error) {
	id := quoted.ID.UUID()
	resp, err := s.confirmCustomerSession(ctx, id, &CheckoutSessionConfirmRequest{Payment: CheckoutSessionConfirmPayment{Rail: quoted.Payment.Rail}}, user, payer)
	if err != nil || (resp != nil && resp.Status == string(models.CheckoutSessionStatusFailed)) {
		s.discardDeclinedEnrollmentCard(ctx, id)
	}
	return resp, err
}

func (s *CheckoutSessionService) discardDeclinedEnrollmentCard(ctx context.Context, sessionID uuid.UUID) {
	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		return
	}
	raw, _ := session.RailState[initialMembershipVaultedMethodKey].(string)
	methodID, err := uuid.Parse(raw)
	if err != nil || methodID == uuid.Nil || s.paymentMethodService == nil {
		return
	}
	ctx = db.WithPSPID(ctx, session.PspID)
	operation, err := intents.NewStore(s.db).GetByIdempotencyKey(ctx, InitialMembershipIdempotencyKey("checkout_session:"+session.ID.String()))
	if err != nil || operation.Status != intents.StatusFailedTerminal {
		return
	}
	method, err := s.paymentMethodService.GetByID(ctx, methodID)
	if err != nil || method.CustomerID != session.CustomerID {
		return
	}
	s.discardEnrollmentCard(ctx, method)
}
