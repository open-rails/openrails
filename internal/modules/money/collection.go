package money

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/merchant"
)

// CollectionAdapter charges a processor-specific saved payment method.
type CollectionAdapter interface {
	ChargeSavedMethod(ctx context.Context, method gen.OpenrailsPaymentMethod, req ChargeRequest) (ChargeResult, error)
}

// ScopedCharger validates merchant/customer/payment-method scope before
// dispatching an off-session invoice or top-up charge to a processor adapter.
type ScopedCharger struct {
	db       *db.DB
	adapters map[string]CollectionAdapter
}

func NewScopedCharger(database *db.DB, adapters map[string]CollectionAdapter) *ScopedCharger {
	cp := make(map[string]CollectionAdapter, len(adapters))
	for processor, adapter := range adapters {
		processor = normalizeProcessor(processor)
		if processor == "" || adapter == nil {
			continue
		}
		cp[processor] = adapter
	}
	return &ScopedCharger{db: database, adapters: cp}
}

func (c *ScopedCharger) ChargeSavedMethod(ctx context.Context, req ChargeRequest) (ChargeResult, error) {
	if c == nil || c.db == nil {
		return ChargeResult{}, fmt.Errorf("scoped charger not initialized")
	}
	if req.PaymentMethodID == uuid.Nil {
		return ChargeResult{}, fmt.Errorf("payment_method_id required")
	}
	if req.Payer.IsZero() {
		return ChargeResult{}, fmt.Errorf("payer required")
	}
	merchantID := req.MerchantID
	if merchantID == uuid.Nil {
		tid, err := merchant.Require(ctx)
		if err != nil {
			return ChargeResult{}, err
		}
		merchantID = tid.UUID()
		req.MerchantID = merchantID
	}

	method, err := c.db.Gen(ctx).GetPaymentMethodByID(ctx, req.PaymentMethodID)
	if err != nil {
		return ChargeResult{}, fmt.Errorf("load payment method: %w", err)
	}
	if method.MerchantID != merchantID {
		return ChargeResult{}, fmt.Errorf("payment method belongs to another merchant")
	}
	if method.CustomerID != req.Payer.UUID() {
		return ChargeResult{}, fmt.Errorf("payment method belongs to another customer")
	}
	if method.FailureReason != nil && strings.TrimSpace(*method.FailureReason) != "" {
		return ChargeResult{}, fmt.Errorf("payment method is not eligible for invoice collection")
	}

	processor := normalizeProcessor(method.Processor)
	if processor == "" {
		return ChargeResult{}, fmt.Errorf("payment method processor required")
	}
	switch processor {
	case string(models.ProcessorCCBill), string(models.ProcessorSolana):
		return ChargeResult{}, fmt.Errorf("processor %q does not support invoice collection", processor)
	}

	adapter := c.adapters[processor]
	if adapter == nil {
		return ChargeResult{}, fmt.Errorf("no invoice collection adapter configured for processor %q", processor)
	}
	res, err := adapter.ChargeSavedMethod(ctx, method, req)
	if err != nil {
		return ChargeResult{}, err
	}
	if strings.TrimSpace(res.Processor) == "" {
		res.Processor = processor
	}
	return res, nil
}

func normalizeProcessor(processor string) string {
	return strings.ToLower(strings.TrimSpace(processor))
}
