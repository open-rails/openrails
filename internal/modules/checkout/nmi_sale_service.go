package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
)

type checkoutSaleIdempotencyResult struct {
	TransactionID string    `json:"transaction_id"`
	PaymentID     uuid.UUID `json:"payment_id"`
	DelayedStart  *string   `json:"delayed_start,omitempty"`
}

// intentExecutor is the write-through provider-intents surface:
// EnqueueAndExecute posts the durable intent and executes it inline; anything
// not finished inline is drained by the scheduled executor/verifier.
type intentExecutor interface {
	EnqueueAndExecute(ctx context.Context, p intents.EnqueueParams) (gen.BillingProviderIntent, error)
	EnqueueOwnedAndExecute(ctx context.Context, p intents.EnqueueParams, owns func(gen.BillingProviderIntent) error) (gen.BillingProviderIntent, error)
}

// ErrCheckoutProcessing is returned when the provider write's outcome is not
// yet final (transport-ambiguous charge being verified, parked on an
// offline/misconfigured provider, or racing executor). The durable intent
// finishes the work out-of-band; the client may retry with the SAME
// idempotency key to observe the final result. It is never a decline.
var ErrCheckoutProcessing = errors.New("payment is processing; retry with the same idempotency key to check the result")

type CheckoutNMISaleService struct {
	PurchaseService          *CheckoutPurchaseService
	PaymentMethodResolver    *CheckoutPaymentMethodResolver
	RailPaymentMethodService *paymentmethods.RailPaymentMethodService
	// ResolveNMIClient arms the ctx merchant's NMI client from the armed rail
	// state: the only client source; nil fails closed.
	ResolveNMIClient func(context.Context, string) (*nmi.NMIClient, error)
	// Config arms the Stripe sale clients.
	Config *config.Config
	// Posture gates provider writes per merchant; nil parks them.
	Posture intents.ModeView
	// StripeEngines arms the accepted Stripe account for saved-card sales.
	StripeEngines intents.StripeEngineServiceResolver
	// Intents executes the durable write-ahead sale intent. Every card charge
	// in this flow goes through it; there is no direct provider call here.
	Intents intentExecutor
	// Orders settles an order sale in its completion transaction.
	Orders *orders.Service
	// Custody charges cards a Basis Theory custodian holds.
	Custody *CheckoutCustodianSaleService
}

func NewCheckoutNMISaleService(
	purchaseService *CheckoutPurchaseService,
	pmResolver *CheckoutPaymentMethodResolver,
	railPMService *paymentmethods.RailPaymentMethodService,
) *CheckoutNMISaleService {
	return &CheckoutNMISaleService{
		PurchaseService:          purchaseService,
		PaymentMethodResolver:    pmResolver,
		RailPaymentMethodService: railPMService,
	}
}

func renderSaleOperation(intent gen.BillingProviderIntent) (*CheckoutResponse, error) {
	switch intent.Status {
	case intents.StatusSucceeded:
		cached, derr := saleResultFromIntent(intent)
		if derr != nil {
			return nil, fmt.Errorf("sale succeeded but evidence unreadable: %w", derr)
		}

		return saleResponse(cached, "Purchase completed successfully"), nil
	case intents.StatusFailedTerminal:
		return nil, terminalCheckoutError(intent, "payment failed")
	default:
		return nil, ErrCheckoutProcessing
	}

}

// saleResultFromIntent reads the producer-facing evidence off a succeeded
// sale intent.
func saleResultFromIntent(intent gen.BillingProviderIntent) (checkoutSaleIdempotencyResult, error) {
	var out checkoutSaleIdempotencyResult
	var evidence struct {
		TransactionID string `json:"transaction_id"`
		PaymentID     string `json:"payment_id"`
		DelayedStart  string `json:"delayed_start"`
	}
	if len(intent.ResultEvidence) == 0 {
		return out, errors.New("no result evidence")
	}
	if err := json.Unmarshal(intent.ResultEvidence, &evidence); err != nil {
		return out, err
	}
	out.TransactionID = evidence.TransactionID
	if evidence.PaymentID != "" {
		if id, err := uuid.Parse(evidence.PaymentID); err == nil {
			out.PaymentID = id
		}
	}
	if evidence.DelayedStart != "" {
		ds := evidence.DelayedStart
		out.DelayedStart = &ds
	}
	return out, nil
}

func saleResponse(cached checkoutSaleIdempotencyResult, message string) *CheckoutResponse {
	var delayedStart *time.Time
	if cached.DelayedStart != nil {
		if t, err := time.Parse(time.RFC3339, *cached.DelayedStart); err == nil {
			delayedStart = &t
		}
	}
	resp := &CheckoutResponse{
		Status:        "success",
		Action:        "new",
		Message:       message,
		TransactionID: cached.TransactionID,
		DelayedStart:  delayedStart,
	}
	if cached.PaymentID != uuid.Nil {
		id := cached.PaymentID
		resp.PaymentID = &id
	}
	return resp
}

func (s *CheckoutNMISaleService) requireArmed(ctx context.Context, target railTarget) error {
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

func (s *CheckoutNMISaleService) nmiClient(ctx context.Context, provider string) (*nmi.NMIClient, error) {
	if s.ResolveNMIClient == nil {
		return nil, fmt.Errorf("missing client")
	}
	return s.ResolveNMIClient(ctx, provider)
}
