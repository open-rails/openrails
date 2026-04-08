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
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/vault"
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

	client, ok := s.NMIClients[provider]
	if !ok {
		return nil, fmt.Errorf("NMI provider '%s' is not configured", provider)
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
			return nil, errors.New("previous checkout attempt failed, please try again")
		}
	}

	customerVaultID, createdPaymentMethod, err := s.VaultResolver.ResolveVault(ctx, req, user, provider)
	if err != nil {
		_ = s.IdempotencyStore.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}

	orderID := uuid.New().String()
	if req.Metadata != nil {
		if runID := strings.TrimSpace(req.Metadata["e2e_run_id"]); runID != "" {
			orderID = fmt.Sprintf("e2e_%s_%s", runID, orderID)
		}
	}

	saleResp, err := client.RunSale(nmi.SaleParams{
		CustomerVaultID:  customerVaultID,
		Amount:           price.Amount,
		Currency:         price.Currency,
		OrderDescription: fmt.Sprintf("Purchase: %s - %s", product.DisplayName, price.DisplayName),
		OrderID:          orderID,
	})
	if err != nil {
		if createdPaymentMethod != nil && s.VaultService != nil {
			_ = s.VaultService.DeleteVault(ctx, createdPaymentMethod)
		}
		_ = s.IdempotencyStore.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, fmt.Errorf("payment failed: %w", err)
	}

	result, err := s.PurchaseService.RegisterPurchase(ctx, &payments.RegisterPurchaseRequest{
		UserID:        user.ID,
		PriceID:       price.ID,
		Processor:     provider,
		TransactionID: saleResp.TransactionID,
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
		log.WithError(err).WithField("transaction_id", saleResp.TransactionID).Error("failed to register purchase after successful NMI sale")
		_ = s.IdempotencyStore.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, fmt.Errorf("payment processed but failed to register: %w", err)
	}

	var delayedStartStr *string
	if result.DelayedStart != nil {
		str := result.DelayedStart.Format(time.RFC3339)
		delayedStartStr = &str
	}
	_ = s.IdempotencyStore.Complete(ctx, idempOp, idempotencyKey, checkoutSaleIdempotencyResult{
		TransactionID: saleResp.TransactionID,
		PaymentID:     result.PaymentID,
		DelayedStart:  delayedStartStr,
	})

	return &CheckoutResponse{
		Status:        "success",
		Action:        "new",
		Message:       "Purchase completed successfully",
		PaymentID:     &result.PaymentID,
		TransactionID: saleResp.TransactionID,
		DelayedStart:  result.DelayedStart,
	}, nil
}
