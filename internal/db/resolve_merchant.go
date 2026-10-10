package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
)

// RegisterUnboundMerchant registers a merchant (a billing bucket) from config,
// idempotently (#480). It carries ONLY billing/rail state and NO auth. Embedded
// boot calls it after migrations. Returns the canonical merchant id. An empty
// slug is a no-op.
func RegisterUnboundMerchant(ctx context.Context, qx gen.DBTX, opts RegisterUnboundMerchantOptions) (billing.MerchantID, error) {
	slug := billing.NormalizeMerchantSlug(opts.Slug)
	if slug == "" {
		return billing.MerchantID{}, nil
	}
	// #567: a merchant slug must be a legal AuthKit permission-group instance
	// slug. Embedded never creates the group, so validate here too.
	if err := billing.ValidateMerchantSlug(slug); err != nil {
		return billing.MerchantID{}, err
	}
	id, err := gen.New(qx).RegisterUnboundMerchant(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return billing.MerchantID{}, fmt.Errorf("register merchant slug %q: the name belongs to a group-bound merchant", slug)
	}
	if err != nil {
		return billing.MerchantID{}, fmt.Errorf("register merchant slug %q: %w", slug, err)
	}
	return billing.MerchantID(id), nil
}

// RegisterUnboundMerchantOptions is the billing-only descriptor for
// RegisterUnboundMerchant. It carries NO auth/issuer/JWKS — auth is the host's
// (embedded) or AuthKit's (standalone) — and no configuration.
type RegisterUnboundMerchantOptions struct {
	Slug string
}

// RequireMerchantID verifies an explicit internal UUID without interpreting any
// public name. Missing or deleted identities cannot report empty successful work.
func (d *DB) RequireMerchantID(ctx context.Context, id billing.MerchantID) error {
	if id.IsZero() {
		return fmt.Errorf("merchant_id is required")
	}
	if _, err := d.Gen(ctx).GetMerchantDirectoryByID(ctx, id.UUID()); err != nil {
		return fmt.Errorf("merchant %s is not available: %w", id, err)
	}
	return nil
}
