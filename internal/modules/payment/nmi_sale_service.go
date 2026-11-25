package payment

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
)

// ErrPurchaseProcessing is returned when the provider write's outcome is not
// yet final (transport-ambiguous charge being verified, parked on an
// offline/misconfigured provider, or racing executor). The durable intent
// finishes the work out-of-band; the client may retry with the SAME
// idempotency key to observe the final result. It is never a decline.
var ErrPurchaseProcessing = errors.New("payment is processing; retry with the same idempotency key to check the result")

type NMISaleService struct {
	Fulfillment              *FulfillmentService
	RailPaymentMethodService *paymentmethods.RailPaymentMethodService
	// ResolveNMIClient arms the ctx merchant's NMI client from the armed rail
	// state: the only client source; nil fails closed.
	ResolveNMIClient func(context.Context, string) (*nmi.NMIClient, error)
	// Config arms the Stripe sale clients.
	Config *billing.Config
	// Posture gates provider writes per merchant; nil parks them.
	Posture intents.ModeView
	// StripeEngines arms the accepted Stripe account for saved-card sales.
	StripeEngines intents.StripeEngineServiceResolver
	// Intents executes the durable write-ahead sale intent. Every card charge
	// in this flow goes through it; there is no direct provider call here.
	Intents *intents.Runner
	// Orders settles an order sale in its completion transaction.
	Orders *orders.Service
	// Custody charges cards a Basis Theory custodian holds.
	Custody *CustodianSaleService
}

func NewNMISaleService(
	fulfillment *FulfillmentService,
	railPMService *paymentmethods.RailPaymentMethodService,
) *NMISaleService {
	return &NMISaleService{
		Fulfillment:              fulfillment,
		RailPaymentMethodService: railPMService,
	}
}

func (s *NMISaleService) requireArmed(ctx context.Context, target railTarget) error {
	if target.Rail != string(models.RailStripe) {
		if _, err := s.nmiClient(ctx, target.PSP); err != nil {
			return fmt.Errorf("NMI provider '%s' is not configured: %w", target.PSP, err)
		}
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	if s.StripeEngines == nil || target.Scope == nil {
		return errors.New("Stripe card sales are not configured")
	}
	psp := target.Scope.ID
	if _, found, err := s.StripeEngines.ResolveStripeEngineService(ctx, mid.UUID(), &psp); err != nil || !found {
		return fmt.Errorf("Stripe provider '%s' is not configured", target.PSP)
	}
	return nil
}

func (s *NMISaleService) nmiClient(ctx context.Context, provider string) (*nmi.NMIClient, error) {
	if s.ResolveNMIClient == nil {
		return nil, fmt.Errorf("missing client")
	}
	return s.ResolveNMIClient(ctx, provider)
}
