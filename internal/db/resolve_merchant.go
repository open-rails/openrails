package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ResolveMerchantSlug resolves a public merchant SLUG to the internal merchant.ID
// (#480). The slug is the public-surface identifier (library options, config,
// HTTP); the UUID is an internal detail resolved exactly once at bootstrap.
//
// An empty slug yields the zero id and no error — no merchant is pinned, so
// merchant-owned operations hard-fail downstream (merchant.Require →
// ErrNoMerchant), by design (there is no default merchant). A non-empty slug that
// does not match a live merchant is a configuration error the caller should fail
// boot on.
func ResolveMerchantSlug(ctx context.Context, qx gen.DBTX, slug string) (merchant.ID, error) {
	if slug == "" {
		return merchant.ID{}, nil
	}
	row, err := gen.New(qx).GetMerchantBySlug(ctx, slug)
	if err != nil {
		// A no-rows error is the unprovisioned-merchant case: the slug is
		// configured but no merchant row exists. Standalone/remote must not
		// auto-create — surface a CLEAR, actionable error (not a panic, #478).
		// Embedded boot calls RegisterMerchant before resolving, so it never
		// reaches this branch for its own bound merchant.
		if errors.Is(err, pgx.ErrNoRows) {
			return merchant.ID{}, fmt.Errorf("configured merchant slug %q is not provisioned — register the merchant (embedded: db.RegisterMerchant at boot; standalone: provision via the control plane)", slug)
		}
		return merchant.ID{}, fmt.Errorf("resolve configured merchant slug %q: %w", slug, err)
	}
	return merchant.ID(row.ID), nil
}

// RegisterMerchant registers a merchant (a billing bucket) from config,
// idempotently (#480). It carries ONLY billing/rail state and NO auth. Embedded
// boot calls it after migrations. Returns the canonical merchant id. An empty
// slug is a no-op.
func RegisterMerchant(ctx context.Context, qx gen.DBTX, opts RegisterMerchantOptions) (merchant.ID, error) {
	slug := merchant.NormalizeSlug(opts.Slug)
	if slug == "" {
		return merchant.ID{}, nil
	}
	// #567: a merchant slug must be a legal AuthKit permission-group instance
	// slug. Embedded never creates the group, so validate here too.
	if err := merchant.ValidateSlug(slug); err != nil {
		return merchant.ID{}, err
	}
	id, err := gen.New(qx).RegisterMerchant(ctx, slug)
	if err != nil {
		return merchant.ID{}, fmt.Errorf("register merchant slug %q: %w", slug, err)
	}
	return merchant.ID(id), nil
}

// RegisterMerchantOptions is the billing-only descriptor for RegisterMerchant.
// It carries NO auth/issuer/JWKS — auth is the host's (embedded) or AuthKit's
// (standalone). Provider account identity is owned by provider_accounts, not
// merchants.
type RegisterMerchantOptions struct {
	Slug string
}
