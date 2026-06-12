package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/vault"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
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
	Complete(ctx context.Context, operation, key string, result any) error
}

type CheckoutNMISaleService struct {
	PurchaseService  *CheckoutPurchaseService
	VaultResolver    *CheckoutVaultService
	VaultService     *vault.VaultService
	IdempotencyStore checkoutIdempotencyStore
	NMIClients       map[string]*nmi.NMIClient
	ResolveNMIClient func(context.Context, string) (*nmi.NMIClient, error)
}

func NewCheckoutNMISaleService(
	purchaseService *CheckoutPurchaseService,
	vaultResolver *CheckoutVaultService,
	vaultService *vault.VaultService,
	idempotencyStore checkoutIdempotencyStore,
	nmiClients map[string]*nmi.NMIClient,
) *CheckoutNMISaleService {
	return &CheckoutNMISaleService{
		PurchaseService:  purchaseService,
		VaultResolver:    vaultResolver,
		VaultService:     vaultService,
		IdempotencyStore: idempotencyStore,
		NMIClients:       nmiClients,
	}
}

func (s *CheckoutNMISaleService) Process(ctx context.Context, req *CheckoutRequest, user *UserIdentity, price *models.Price, product *models.Product, idempotencyKey string, provider string) (*CheckoutResponse, error) {
	const idempOp = "nmi_sale"
	provider = strings.TrimSpace(strings.ToLower(provider))
	if provider == "" {
		return nil, errors.New("processor is required")
	}

	client, err := s.nmiClient(ctx, provider)
	if err != nil {
		return nil, fmt.Errorf("NMI provider '%s' is not configured: %w", provider, err)
	}
	orderID := nmiSaleOrderID(idempotencyKey, req.Metadata)

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
			var delayedStart *time.Time
			if cached.DelayedStart != nil {
				if t, err := time.Parse(time.RFC3339, *cached.DelayedStart); err == nil {
					delayedStart = &t
				}
			}
			return &CheckoutResponse{Status: "success", Action: "new", Message: "Purchase already completed", PaymentID: &cached.PaymentID, TransactionID: cached.TransactionID, DelayedStart: delayedStart}, nil
		case IdempotencyStatusPending:
			return nil, errors.New("checkout already in progress, please wait")
		case IdempotencyStatusFailed:
			if s.PurchaseService != nil && s.PurchaseService.PaymentService != nil {
				if existingAttempt, err := s.PurchaseService.PaymentService.GetByMetadataValue(ctx, "nmi_order_id", orderID); err == nil && payments.PaymentStatusCompleted(existingAttempt.Status) {
					return s.completeNMISaleRegistration(ctx, req, user, price, provider, existingAttempt.TransactionID, idempotencyKey, idempOp, orderID)
				}
			}
			return nil, errors.New("previous checkout attempt failed, please try again")
		}
	}

	customerVaultID, createdPaymentMethod, err := s.VaultResolver.ResolveVault(ctx, req, user, provider)
	if err != nil {
		_ = s.IdempotencyStore.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}

	attemptMetadata := nmiSaleAttemptMetadata(idempotencyKey, orderID, req.Metadata, "pending", "")
	attemptTransactionID := nmiSaleAttemptTransactionID(orderID)

	if s.PurchaseService == nil || s.PurchaseService.PaymentService == nil {
		err := errors.New("payment service unavailable")
		_ = s.IdempotencyStore.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}

	if existingAttempt, err := s.PurchaseService.PaymentService.GetByMetadataValue(ctx, "nmi_order_id", orderID); err == nil {
		switch strings.ToLower(strings.TrimSpace(existingAttempt.Status)) {
		case "", payments.PaymentStatusCompletedValue:
			return s.completeNMISaleRegistration(ctx, req, user, price, provider, existingAttempt.TransactionID, idempotencyKey, idempOp, orderID)
		case payments.PaymentStatusPendingValue:
			return nil, errors.New("NMI sale attempt is already pending, please wait")
		default:
			err := errors.New("previous NMI sale attempt failed, retry with a new idempotency key")
			_ = s.IdempotencyStore.Fail(ctx, idempOp, idempotencyKey, err)
			return nil, err
		}
	} else if !repo.IsNotFound(err) {
		_ = s.IdempotencyStore.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, fmt.Errorf("load NMI sale attempt: %w", err)
	}

	subjectID, err := s.PurchaseService.PaymentService.EnsureSubjectID(ctx, user.ID)
	if err != nil {
		_ = s.IdempotencyStore.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, fmt.Errorf("resolve tenant subject for NMI sale attempt: %w", err)
	}

	attempt, err := s.PurchaseService.PaymentService.ReserveProviderAttempt(ctx, &models.Payment{
		ID:              uuidutil.NewV7(),
		TenantSubjectID: subjectID,
		PriceID:         price.ID,
		Processor:       models.Processor(provider),
		TransactionID:   attemptTransactionID,
		Amount:          price.Amount,
		ListAmount:      price.Amount,
		Currency:        price.Currency,
		Status:          payments.PaymentStatusPendingValue,
		Metadata:        attemptMetadata,
	})
	if err != nil {
		_ = s.IdempotencyStore.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, fmt.Errorf("reserve NMI sale attempt: %w", err)
	}

	saleResp, err := client.RunSale(nmi.SaleParams{
		CustomerVaultID:  customerVaultID,
		Amount:           price.Amount,
		Currency:         price.Currency,
		OrderDescription: fmt.Sprintf("Purchase: %s", product.DisplayName),
		OrderID:          orderID,
	})
	if err != nil {
		_ = s.PurchaseService.PaymentService.MarkFailed(ctx, attempt.ID)
		if createdPaymentMethod != nil && s.VaultService != nil {
			_ = s.VaultService.DeleteVault(ctx, createdPaymentMethod)
		}
		_ = s.IdempotencyStore.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, fmt.Errorf("payment failed: %w", err)
	}
	completedAttempt, err := s.PurchaseService.PaymentService.CompleteProviderAttempt(ctx, attempt.ID, saleResp.TransactionID, nmiSaleAttemptMetadata(idempotencyKey, orderID, req.Metadata, "completed", saleResp.TransactionID))
	if err != nil {
		log.WithError(err).WithField("transaction_id", saleResp.TransactionID).Error("failed to complete NMI sale attempt after successful provider sale")
		_ = s.IdempotencyStore.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, fmt.Errorf("payment processed but failed to record provider transaction: %w", err)
	}

	return s.completeNMISaleRegistration(ctx, req, user, price, provider, completedAttempt.TransactionID, idempotencyKey, idempOp, orderID)
}

func (s *CheckoutNMISaleService) nmiClient(ctx context.Context, provider string) (*nmi.NMIClient, error) {
	if s.ResolveNMIClient != nil {
		return s.ResolveNMIClient(ctx, provider)
	}
	client, ok := s.NMIClients[provider]
	if !ok || client == nil {
		return nil, fmt.Errorf("missing client")
	}
	return client, nil
}

func (s *CheckoutNMISaleService) completeNMISaleRegistration(ctx context.Context, req *CheckoutRequest, user *UserIdentity, price *models.Price, provider string, transactionID string, idempotencyKey string, idempOp string, orderID string) (*CheckoutResponse, error) {
	result, err := s.PurchaseService.RegisterPurchase(ctx, &payments.RegisterPurchaseRequest{
		UserID:        user.ID,
		PriceID:       price.ID,
		Processor:     provider,
		TransactionID: transactionID,
		Amount:        price.Amount,
		Currency:      price.Currency,
		Metadata: func() map[string]any {
			if req.Metadata == nil {
				return nil
			}
			if runID := strings.TrimSpace(req.Metadata["e2e_run_id"]); runID != "" {
				return map[string]any{"e2e_run_id": runID, "order_id": orderID}
			}
			return nil
		}(),
	})
	if err != nil {
		log.WithError(err).WithField("transaction_id", transactionID).Error("failed to register purchase after successful NMI sale")
		_ = s.IdempotencyStore.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, fmt.Errorf("payment processed but failed to register: %w", err)
	}

	var delayedStartStr *string
	if result.DelayedStart != nil {
		str := result.DelayedStart.Format(time.RFC3339)
		delayedStartStr = &str
	}
	_ = s.IdempotencyStore.Complete(ctx, idempOp, idempotencyKey, checkoutSaleIdempotencyResult{
		TransactionID: transactionID,
		PaymentID:     result.PaymentID,
		DelayedStart:  delayedStartStr,
	})

	return &CheckoutResponse{
		Status:        "success",
		Action:        "new",
		Message:       "Purchase completed successfully",
		PaymentID:     &result.PaymentID,
		TransactionID: transactionID,
		DelayedStart:  result.DelayedStart,
	}, nil
}

func nmiSaleAttemptTransactionID(orderID string) string {
	return "nmi_sale_attempt:" + strings.TrimSpace(orderID)
}

func nmiSaleOrderID(idempotencyKey string, metadata map[string]string) string {
	orderID := nmiIdempotentOrderID("sale", idempotencyKey)
	if orderID == "" {
		orderID = uuid.New().String()
	}
	if runID := strings.TrimSpace(metadata["e2e_run_id"]); runID != "" {
		orderID = fmt.Sprintf("%s_e2e_%s", orderID, nmiOrderIDSuffix(runID))
	}
	return orderID
}

func nmiSaleAttemptMetadata(idempotencyKey string, orderID string, requestMetadata map[string]string, status string, transactionID string) map[string]any {
	metadata := map[string]any{
		"checkout_idempotency_key": strings.TrimSpace(idempotencyKey),
		"nmi_order_id":             strings.TrimSpace(orderID),
		"nmi_attempt_status":       status,
	}
	if runID := strings.TrimSpace(requestMetadata["e2e_run_id"]); runID != "" {
		metadata["e2e_run_id"] = runID
	}
	if transactionID != "" {
		metadata["provider_transaction_id"] = transactionID
	}
	return metadata
}
