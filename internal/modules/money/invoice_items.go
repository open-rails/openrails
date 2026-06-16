package money

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

func insertPendingInvoiceItemTx(
	ctx context.Context,
	q *gen.Queries,
	merchantID, customerID uuid.UUID,
	currency string,
	sourceType, sourceID string,
	eventType *string,
	amount int64,
	invoiceAt time.Time,
	metadata map[string]any,
) error {
	if amount <= 0 {
		return nil
	}
	sourceType = strings.TrimSpace(sourceType)
	sourceID = strings.TrimSpace(sourceID)
	if sourceType == "" || sourceID == "" {
		return fmt.Errorf("invoice item source_type and source_id required")
	}
	if invoiceAt.IsZero() {
		invoiceAt = time.Now().UTC()
	} else {
		invoiceAt = invoiceAt.UTC()
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	meta, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("money: encode invoice item metadata: %w", err)
	}
	return q.InsertInvoiceItem(ctx, gen.InsertInvoiceItemParams{
		ID:         uuidutil.NewV7(),
		MerchantID: merchantID,
		CustomerID: customerID,
		Currency:   normalizeCurrency(currency),
		SourceType: sourceType,
		SourceID:   sourceID,
		EventType:  eventType,
		PeriodFrom: invoiceAt,
		PeriodTo:   invoiceAt,
		InvoiceAt:  invoiceAt,
		Quantity:   1,
		UnitAmount: amount,
		Amount:     amount,
		Status:     "pending",
		Metadata:   meta,
		CreatedAt:  invoiceAt,
		UpdatedAt:  invoiceAt,
	})
}
