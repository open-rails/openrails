package subscriptions

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/uptrace/bun"
)

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

	subIDs := make([]uuid.UUID, 0, 1)
	query := dbb.GetDB().NewSelect().
		TableExpr("billing.subscriptions").
		Column("id").
		Where("user_id = ?", userID).
		Where("product_id = ?", productID).
		Where("status = ?", models.StatusCancelled)
	if excludeID != uuid.Nil {
		query = query.Where("id != ?", excludeID)
	}
	if err := query.Scan(ctx, &subIDs); err != nil {
		return 0, err
	}
	if len(subIDs) == 0 {
		return 0, nil
	}

	if _, err := dbb.GetDB().NewUpdate().
		TableExpr("billing.checkout_sessions").
		Set("subscription_id = NULL").
		Where("subscription_id IN (?)", bun.In(subIDs)).
		Exec(ctx); err != nil {
		return 0, err
	}

	res, err := dbb.GetDB().NewDelete().
		TableExpr("billing.subscriptions").
		Where("id IN (?)", bun.In(subIDs)).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(rows), nil
}
