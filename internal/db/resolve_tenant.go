package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
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
		// A no-rows error is the unprovisioned-tenant case: the slug is
		// configured but no tenant row exists. Standalone/remote must not
		// auto-create — surface a CLEAR, actionable error (not a panic, #478).
		// Embedded boot calls EnsureTenantBySlug before resolving, so it never
		// reaches this branch for its own bound tenant.
		if errors.Is(err, pgx.ErrNoRows) {
			return tenant.ID{}, fmt.Errorf("configured tenant slug %q is not provisioned — create the tenant and register its JWKS via the control plane", slug)
		}
		return tenant.ID{}, fmt.Errorf("resolve configured tenant slug %q: %w", slug, err)
	}
	return tenant.ID(row.ID), nil
}

// EnsureTenantBySlug idempotently creates the bound tenant row for an EMBEDDED
// host (#478). The host owns the embed + DB, so auto-creating the configured
// tenant slug after migrations is safe; it is a no-op when the row already
// exists. Standalone/remote MUST NOT call this — an unprovisioned slug there is
// a configuration error (ResolveTenantSlug returns the actionable message).
// An empty slug is a no-op.
func EnsureTenantBySlug(ctx context.Context, qx gen.DBTX, slug string) error {
	if slug == "" {
		return nil
	}
	if err := gen.New(qx).EnsureTenantBySlug(ctx, slug); err != nil {
		return fmt.Errorf("ensure embedded tenant slug %q: %w", slug, err)
	}
	return nil
}
