package controlplane

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
)

const maxActiveMerchantPageSize = 200

// ListActiveMerchantIDs returns one directory page for privileged host
// orchestration that must enter each merchant's scope independently.
func (c *ControlPlane) ListActiveMerchantIDs(ctx context.Context, limit, offset int) ([]billing.MerchantID, error) {
	if c == nil || c.pool == nil {
		return nil, errors.New("controlplane: pgx pool unavailable for merchant enumeration")
	}
	if limit <= 0 || limit > maxActiveMerchantPageSize {
		limit = maxActiveMerchantPageSize
	}
	if offset < 0 {
		offset = 0
	}
	status := "active"
	rows, err := gen.New(c.pool).ListPlatformMerchants(ctx, gen.ListPlatformMerchantsParams{
		Status:     &status,
		PageOffset: int64(offset),
		PageLimit:  int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("controlplane: list active merchant ids: %w", err)
	}

	ids := make([]billing.MerchantID, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, billing.MerchantID(row.ID))
	}
	return ids, nil
}
