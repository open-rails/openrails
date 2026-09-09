package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// RegisterUnboundMerchant registers a merchant (a billing bucket) from config,
// idempotently (#480). It carries ONLY billing/rail state and NO auth. Embedded
// boot calls it after migrations. Returns the canonical merchant id. An empty
// slug is a no-op.
func RegisterUnboundMerchant(ctx context.Context, qx gen.DBTX, opts RegisterUnboundMerchantOptions) (merchant.ID, error) {
	slug := merchant.NormalizeSlug(opts.Slug)
	if slug == "" {
		return merchant.ID{}, nil
	}
	// #567: a merchant slug must be a legal AuthKit permission-group instance
	// slug. Embedded never creates the group, so validate here too.
	if err := merchant.ValidateSlug(slug); err != nil {
		return merchant.ID{}, err
	}
	var displayName *string
	if dn := strings.TrimSpace(opts.DisplayName); dn != "" {
		displayName = &dn
	}
	id, err := gen.New(qx).RegisterUnboundMerchant(ctx, gen.RegisterUnboundMerchantParams{Slug: slug, DisplayName: displayName})
	if err != nil {
		return merchant.ID{}, fmt.Errorf("register merchant slug %q: %w", slug, err)
	}
	return merchant.ID(id), nil
}

// RegisterUnboundMerchantOptions is the billing-only descriptor for RegisterUnboundMerchant.
// It carries NO auth/issuer/JWKS — auth is the host's (embedded) or AuthKit's
// (standalone). PSP identity is owned by psps, not
// merchants.
type RegisterUnboundMerchantOptions struct {
	Slug string
	// DisplayName is the human-readable merchant name (end-user display / invoices).
	// Optional; empty leaves any existing name untouched on re-register.
	DisplayName string
}

// RequireMerchantID verifies an explicit internal UUID without interpreting any
// public name. Missing or deleted identities cannot report empty successful work.
func (d *DB) RequireMerchantID(ctx context.Context, id merchant.ID) error {
	if id.IsZero() {
		return fmt.Errorf("merchant_id is required")
	}
	if _, err := d.Gen(ctx).GetMerchantDirectoryByID(ctx, id.UUID()); err != nil {
		return fmt.Errorf("merchant %s is not available: %w", id, err)
	}
	return nil
}
