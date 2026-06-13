package db

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/tenant"
)

// ResolveTenantSlug resolves a public tenant SLUG to the internal tenant.ID
// (#336). The slug is the public-surface identifier (library options, config,
// HTTP); the UUID is an internal detail resolved exactly once at bootstrap.
//
// An empty slug yields the zero id and no error — no tenant is pinned, so
// tenant-owned operations hard-fail downstream (tenant.Require → ErrNoTenant),
// by design (there is no default tenant). A non-empty slug that does not match
// a live tenant is a configuration error the caller should fail boot on.
func ResolveTenantSlug(ctx context.Context, qx gen.DBTX, slug string) (tenant.ID, error) {
	if slug == "" {
		return tenant.ID{}, nil
	}
	row, err := gen.New(qx).GetTenantBySlug(ctx, slug)
	if err != nil {
		return tenant.ID{}, fmt.Errorf("resolve configured tenant slug %q: %w", slug, err)
	}
	return tenant.ID(row.ID), nil
}
