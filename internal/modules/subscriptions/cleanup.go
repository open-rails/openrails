package subscriptions

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/repo"
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

	var supersededBy *string
	var exclude *uuid.UUID
	if excludeID != uuid.Nil {
		v := excludeID.String()
		supersededBy = &v
		exclude = &excludeID
	}

	tsid, err := repo.ResolveTenantSubjectID(userID)
	if err != nil {
		return 0, err
	}

	rows, err := dbb.Gen(ctx).MarkCancelledSubscriptionsSuperseded(ctx, gen.MarkCancelledSubscriptionsSupersededParams{
		TenantSubjectID: tsid,
		ProductID:       productID,
		SupersededBy:    supersededBy,
		ExcludeID:       exclude,
	})
	if err != nil {
		return 0, fmt.Errorf("mark cancelled subscriptions superseded: %w", err)
	}
	return int(rows), nil
}
