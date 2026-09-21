package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/open-rails/openrails/internal/modules/payments"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/pkg/merchant"
)

type checkoutSaleIdempotencyResult struct {
	TransactionID string    `json:"transaction_id"`
	PaymentID     uuid.UUID `json:"payment_id"`
	DelayedStart  *string   `json:"delayed_start,omitempty"`
}

type checkoutIdempotencyStore interface {
	Begin(ctx context.Context, operation, key string) (*IdempotencyRecord, bool, error)
	Fail(ctx context.Context, operation, key string, operationErr error) error
	Complete(ctx context.Context, operation, key string, result json.RawMessage) error
}

// intentExecutor is the write-through provider-intents surface (#674):
// EnqueueAndExecute posts the durable intent and executes it inline; anything
// not finished inline is drained by the scheduled executor/verifier.
type intentExecutor interface {
	EnqueueAndExecute(ctx context.Context, p intents.EnqueueParams) (gen.OpenrailsRailIntent, error)
	EnqueueOwnedAndExecute(ctx context.Context, p intents.EnqueueParams, owns func(gen.OpenrailsRailIntent) error) (gen.OpenrailsRailIntent, error)
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
	// state (#788) — the ONLY client source; nil fails closed.
	ResolveNMIClient func(context.Context, string) (*nmi.NMIClient, error)
	// Intents executes the durable write-ahead sale intent (#674). Every NMI
	// charge in this flow goes through it — there is no direct RunSale here.
	Intents intentExecutor
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

// Process runs a one-time NMI sale as a write-through provider intent (#674):
// durable intent first (unique on the checkout idempotency key), inline
// execution in this request, provider order id derived from the intent id.
// A crash/timeout at ANY point leaves a pending/unknown intent the scheduled
// executor/verifier resolves against the SAME order id — never a blind retry
// under a fresh key, never a charged-but-unrecorded sale.
func (s *CheckoutNMISaleService) Process(ctx context.Context, req *CheckoutRequest, user *UserIdentity, price *models.Price, product *models.Product, idempotencyKey string, target railTarget) (*CheckoutResponse, error) {
	// Rows and intents speak rail vocabulary; the provider (account key) pins
	// the NMI client.
	provider := target.Rail
	if provider == "" {
		return nil, errors.New("rail is required")
	}
	if s.Intents == nil {
		return nil, errors.New("checkout sale intent executor not wired")
	}
	if _, err := customerIDFromUser(user.ID); err != nil {
		return nil, err
	}

	if s.RailPaymentMethodService == nil || s.RailPaymentMethodService.DB == nil {
		return nil, errors.New("sale database unavailable")
	}
	database := s.RailPaymentMethodService.DB
	fingerprint := saleRequestFingerprint(req, user, price.ID, target)
	prior, err := intents.NewStore(database).GetByIdempotencyKey(ctx, NMISaleIdempotencyKey(idempotencyKey))
	if err == nil {
		if err := ownsSaleRequest(prior, user.ID, price.ID, fingerprint); err != nil {
			return nil, err
		}
		// The canonical operation owns replay, including uncertain or declined
		// outcomes. No Redis success shortcut may bypass its request binding.
		intent, err := s.Intents.EnqueueOwnedAndExecute(ctx, saleReplayParams(prior), func(in gen.OpenrailsRailIntent) error { return ownsSaleRequest(in, user.ID, price.ID, fingerprint) })
		if err != nil {
			return nil, err
		}
		return renderSaleOperation(intent)
	}
	if !db.IsNotFound(err) {
		return nil, err
	}

	// Fail fast on misconfiguration instead of parking a user-facing checkout.
	if _, err := s.nmiClient(ctx, target.PSP); err != nil {
		return nil, fmt.Errorf("NMI provider '%s' is not configured: %w", target.PSP, err)
	}

	railCustomerRef, railMethodRef, resolvedMethod, createdPaymentMethod, err := s.PaymentMethodResolver.ResolvePaymentMethod(ctx, req, user, target)
	if err != nil {
		return nil, err
	}
	if resolvedMethod == nil {
		return nil, errors.New("sale requires a saved instrument")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var intent gen.OpenrailsRailIntent
	err = database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		bound := database.NewWithPgxTx(tx)
		customer, err := customerIDFromUser(user.ID)
		if err != nil {
			return err
		}
		if _, err := bound.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: tid.UUID(), ID: customer}); err != nil {
			return err
		}
		prepared, err := s.prepareAcceptedSale(ctx, bound, req, user, price.ID, resolvedMethod.ID, target, fingerprint)
		if err != nil {
			return err
		}
		if prepared.Instrument.RailCustomerRef != railCustomerRef || prepared.Instrument.RailMethodRef != railMethodRef {
			return errors.New("sale instrument changed during admission")
		}
		intent, err = intents.NewStore(bound).Enqueue(ctx, intents.EnqueueParams{MerchantID: tid.UUID(), Provider: provider, PspID: prepared.Instrument.PSPID, IntentType: payments.TypeNMISale, PriceID: &prepared.PriceID, Payload: prepared, IdempotencyKey: NMISaleIdempotencyKey(idempotencyKey), NextAttemptAt: prepared.AcceptedAt, Origin: intents.OriginUser, OriginReason: "checkout one-time sale"})
		if err != nil {
			return err
		}
		return ownsSaleRequest(intent, user.ID, price.ID, fingerprint)
	})
	if err != nil {
		return nil, err
	}
	intent, err = s.Intents.EnqueueOwnedAndExecute(ctx, saleReplayParams(intent), func(in gen.OpenrailsRailIntent) error { return ownsSaleRequest(in, user.ID, price.ID, fingerprint) })
	if err != nil {
		return nil, err
	}
	if intent.Status == intents.StatusFailedTerminal && createdPaymentMethod && s.RailPaymentMethodService != nil {
		_ = s.RailPaymentMethodService.CleanupPaymentMethodBestEffort(ctx, resolvedMethod)
	}
	return renderSaleOperation(intent)
}

func renderSaleOperation(intent gen.OpenrailsRailIntent) (*CheckoutResponse, error) {
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
func saleResultFromIntent(intent gen.OpenrailsRailIntent) (checkoutSaleIdempotencyResult, error) {
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

func (s *CheckoutNMISaleService) nmiClient(ctx context.Context, provider string) (*nmi.NMIClient, error) {
	if s.ResolveNMIClient == nil {
		return nil, fmt.Errorf("missing client")
	}
	return s.ResolveNMIClient(ctx, provider)
}
