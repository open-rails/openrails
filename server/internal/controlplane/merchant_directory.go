package controlplane

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/pagination"
)

// ListActiveMerchantIDs returns one page of the live merchants, newest first,
// for privileged host orchestration that enters each merchant's scope
// independently.
func (c *ControlPlane) ListActiveMerchantIDs(ctx context.Context, page billing.PageRequest) (*billing.ListPage[billing.MerchantID], error) {
	if c == nil || c.pool == nil {
		return nil, errors.New("controlplane: pgx pool unavailable for merchant enumeration")
	}
	limit, err := pagination.Limit(page)
	if err != nil {
		return nil, err
	}
	afterAt, afterID, err := pagination.After(page.Cursor)
	if err != nil {
		return nil, err
	}
	status := "active"
	rows, err := gen.New(c.pool).ListPlatformMerchants(ctx, gen.ListPlatformMerchantsParams{
		Status: &status, AfterAt: afterAt, AfterID: afterID, RowLimit: pagination.Fetch(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("controlplane: list active merchant ids: %w", err)
	}
	cut := pagination.Cut(rows, limit, func(row gen.ListPlatformMerchantsRow) any { return pagination.TimeID{At: row.CreatedAt, ID: row.ID} })
	out := pagination.Map(cut, func(row gen.ListPlatformMerchantsRow) billing.MerchantID { return billing.MerchantID(row.ID) })
	return &out, nil
}
