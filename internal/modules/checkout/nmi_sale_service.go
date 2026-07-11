package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/pkg/merchant"
	log "github.com/sirupsen/logrus"
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
}

// ErrCheckoutProcessing is returned when the provider write's outcome is not
// yet final (transport-ambiguous charge being verified, parked on an
// offline/misconfigured provider, or racing executor). The durable intent
// finishes the work out-of-band; the client may retry with the SAME
// idempotency key to observe the final result. It is never a decline.
var ErrCheckoutProcessing = errors.New("payment is processing; retry with the same idempotency key to check the result")

type CheckoutNMISaleService struct {
	PurchaseService  *CheckoutPurchaseService
	VaultResolver    *CheckoutVaultService
	VaultService     *paymentmethods.VaultService
	IdempotencyStore checkoutIdempotencyStore
	// ResolveNMIClient arms the ctx merchant's NMI client from the armed rail
	// state (#788) — the ONLY client source; nil fails closed.
	ResolveNMIClient func(context.Context, string) (*nmi.NMIClient, error)
	// Intents executes the durable write-ahead sale intent (#674). Every NMI
	// charge in this flow goes through it — there is no direct RunSale here.
	Intents intentExecutor
}

func NewCheckoutNMISaleService(
	purchaseService *CheckoutPurchaseService,
	vaultResolver *CheckoutVaultService,
	vaultService *paymentmethods.VaultService,
	idempotencyStore checkoutIdempotencyStore,
) *CheckoutNMISaleService {
	return &CheckoutNMISaleService{
		PurchaseService:  purchaseService,
		VaultResolver:    vaultResolver,
		VaultService:     vaultService,
		IdempotencyStore: idempotencyStore,
	}
}

// Process runs a one-time NMI sale as a write-through provider intent (#674):
// durable intent first (unique on the checkout idempotency key), inline
// execution in this request, provider order id derived from the intent id.
// A crash/timeout at ANY point leaves a pending/unknown intent the scheduled
// executor/verifier resolves against the SAME order id — never a blind retry
// under a fresh key, never a charged-but-unrecorded sale.
func (s *CheckoutNMISaleService) Process(ctx context.Context, req *CheckoutRequest, user *UserIdentity, price *models.Price, product *models.Product, idempotencyKey string, target railTarget) (*CheckoutResponse, error) {
	const idempOp = "nmi_sale"
	// Rows and intents speak rail vocabulary; the provider (account key) pins
	// the NMI client.
	provider := target.Rail
	if provider == "" {
		return nil, errors.New("rail is required")
	}
	if s.Intents == nil {
		return nil, errors.New("checkout sale intent executor not wired")
	}
	// Fail fast on misconfiguration instead of parking a user-facing checkout.
	if _, err := s.nmiClient(ctx, target.PSP); err != nil {
		return nil, fmt.Errorf("NMI provider '%s' is not configured: %w", target.PSP, err)
	}
	if _, err := customerIDFromUser(user.ID); err != nil {
		return nil, err
	}

	idempRec, alreadyExists, err := s.IdempotencyStore.Begin(ctx, idempOp, idempotencyKey)
	if err != nil {
		return nil, fmt.Errorf("idempotency check failed: %w", err)
	}
	if alreadyExists {
		switch idempRec.Status {
		case IdempotencyStatusSuccess:
			var cached checkoutSaleIdempotencyResult
			if err := json.Unmarshal(idempRec.Result, &cached); err != nil {
				log.WithError(err).Warn("failed to unmarshal cached sale result, proceeding anyway")
				return &CheckoutResponse{Status: "success", Action: "new", Message: "Purchase already completed", TransactionID: cached.TransactionID}, nil
			}
			return saleResponse(cached, "Purchase already completed"), nil
		case IdempotencyStatusPending:
			return nil, errors.New("checkout already in progress, please wait")
		case IdempotencyStatusFailed:
			// Fall through: the durable intent below is the source of truth —
			// it replays a success, reports a decline, or keeps verifying an
			// ambiguous charge under the ORIGINAL order id.
		}
	}

	customerVaultID, vaultBillingID, resolvedMethod, createdVault, err := s.VaultResolver.ResolveVault(ctx, req, user, target)
	if err != nil {
		_ = s.IdempotencyStore.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}
	// #297: the instrument's unscheduled-sequence anchor. "" (fresh vault or
	// legacy instrument) makes this charge the sequence's initial CIT
	// (indicator=stored) and finalize captures the returned transaction id.
	storedCredentialRef := ""
	if resolvedMethod != nil {
		storedCredentialRef = strings.TrimSpace(resolvedMethod.StoredCredentialUnscheduledRef)
	}

	tid, err := merchant.Require(ctx)
	if err != nil {
		_ = s.IdempotencyStore.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}
	intent, err := s.Intents.EnqueueAndExecute(ctx, intents.EnqueueParams{
		MerchantID: tid.UUID(),
		Provider:   provider,
		IntentType: TypeNMISale,
		PriceID:    &price.ID,
		Payload: NMISalePayload{
			Provider:            provider,
			PSP:                 target.PSP,
			CustomerVaultID:     customerVaultID,
			BillingID:           vaultBillingID,
			AmountMicros:        price.Amount,
			Currency:            price.Currency,
			Description:         fmt.Sprintf("Purchase: %s", product.DisplayName),
			UserID:              user.ID,
			PriceID:             price.ID,
			StoredCredentialRef: storedCredentialRef,
			E2ERunID:            strings.TrimSpace(req.Metadata["e2e_run_id"]),
		},
		IdempotencyKey: NMISaleIdempotencyKey(idempotencyKey),
		NextAttemptAt:  time.Now().UTC(),
		Origin:         intents.OriginUser,
		OriginReason:   "checkout one-time sale",
	})
	if err != nil {
		_ = s.IdempotencyStore.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, fmt.Errorf("post sale intent: %w", err)
	}

	switch intent.Status {
	case intents.StatusSucceeded:
		cached, derr := saleResultFromIntent(intent)
		if derr != nil {
			return nil, fmt.Errorf("sale succeeded but evidence unreadable: %w", derr)
		}
		payload, _ := json.Marshal(cached)
		_ = s.IdempotencyStore.Complete(ctx, idempOp, idempotencyKey, payload)
		return saleResponse(cached, "Purchase completed successfully"), nil
	case intents.StatusFailedTerminal:
		// Verified-clean decline/rejection: no money moved. Direct best-effort
		// cleanup, NOT an intent (#674 tail): the vault was created for THIS
		// declined attempt and is referenced nowhere — harmless if lost.
		if createdVault && resolvedMethod != nil && s.VaultService != nil {
			_ = s.VaultService.CleanupVaultBestEffort(ctx, resolvedMethod)
		}
		reason := "payment failed"
		if intent.LastFailureReason != nil && *intent.LastFailureReason != "" {
			reason = "payment failed: " + *intent.LastFailureReason
		}
		failErr := errors.New(reason)
		_ = s.IdempotencyStore.Fail(ctx, idempOp, idempotencyKey, failErr)
		return nil, failErr
	default:
		// pending (parked), in_flight (racing executor), unknown_needs_verify,
		// failed_retryable: the intent ledger finishes it. Recording a redis
		// failure lets the client's retry re-drive the SAME intent immediately.
		_ = s.IdempotencyStore.Fail(ctx, idempOp, idempotencyKey, ErrCheckoutProcessing)
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
