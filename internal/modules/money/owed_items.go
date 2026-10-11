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

// insertPendingOwedItemTx queues an owed accrual for its usage order. Rows
// leave the pending workspace only when an order bills them.
func insertPendingOwedItemTx(
	ctx context.Context,
	q *gen.Queries,
	merchantID, customerID uuid.UUID,
	currency string,
	sourceType, sourceID string,
	amount int64,
	accruedAt time.Time,
	metadata map[string]any,
) error {
	if amount <= 0 {
		return nil
	}
	sourceType = strings.TrimSpace(sourceType)
	sourceID = strings.TrimSpace(sourceID)
	if sourceType == "" || sourceID == "" {
		return fmt.Errorf("owed item source_type and source_id required")
	}
	if accruedAt.IsZero() {
		return fmt.Errorf("owed item timestamp required")
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	meta, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("money: encode owed item metadata: %w", err)
	}
	now := time.Now().UTC()
	return q.InsertPendingOwedItem(ctx, gen.InsertPendingOwedItemParams{
		ID:         uuidutil.NewV7(),
		MerchantID: merchantID,
		CustomerID: customerID,
		Currency:   normalizeCurrency(currency),
		SourceType: sourceType,
		SourceID:   sourceID,
		AccruedAt:  accruedAt.UTC(),
		Amount:     amount,
		Metadata:   meta,
		CreatedAt:  now,
		UpdatedAt:  now,
	})
}
