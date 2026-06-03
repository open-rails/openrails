package subscriptions

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
)

// RemoveCancelledSubscriptionsForActivation preserves cancelled subscriptions for later
// refund/chargeback correlation while marking them as superseded by the new activation.
func RemoveCancelledSubscriptionsForActivation(ctx context.Context, dbb *db.DB, userID string, productID uuid.UUID, excludeID uuid.UUID) (int, error) {
	if dbb == nil {
		return 0, fmt.Errorf("database handle is required")
	}
	if strings.TrimSpace(userID) == "" {
		return 0, fmt.Errorf("userID is required")
	}
	if productID == uuid.Nil {
		return 0, fmt.Errorf("productID is required")
	}

	var supersededBy interface{}
	if excludeID != uuid.Nil {
		supersededBy = excludeID.String()
	}

	query := dbb.Q(ctx).NewUpdate().
		TableExpr("billing.subscriptions").
		Set("gateway_response = CASE WHEN jsonb_typeof(gateway_response) = 'object' THEN gateway_response || jsonb_build_object('superseded_at', current_timestamp, 'superseded_by_subscription_id', ?) ELSE jsonb_build_object('previous_gateway_response', gateway_response, 'superseded_at', current_timestamp, 'superseded_by_subscription_id', ?) END", supersededBy, supersededBy).
		Set("updated_at = current_timestamp").
		Where("user_id = ?", userID).
		Where("product_id = ?", productID).
		Where("status = ?", models.StatusCancelled)
	if excludeID != uuid.Nil {
		query = query.Where("id != ?", excludeID)
	}

	res, err := query.Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("mark cancelled subscriptions superseded: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read superseded subscription count: %w", err)
	}
	return int(rows), nil
}
