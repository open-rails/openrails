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
// boot calls it after migrations (the host owns the embed + DB, so registering the
// configured merchant is safe); standalone provisioning also routes through it.
// Re-registering an existing slug refreshes the billing-only fields. Returns the
// canonical (self-owned) merchant id. An empty slug is a no-op.
func RegisterMerchant(ctx context.Context, qx gen.DBTX, opts RegisterMerchantOptions) (merchant.ID, error) {
	if opts.Slug == "" {
		return merchant.ID{}, nil
	}
	name := opts.Name
	if name == "" {
		name = opts.Slug
	}
	id, err := gen.New(qx).RegisterMerchant(ctx, gen.RegisterMerchantParams{
		Slug:            opts.Slug,
		Name:            name,
		StripeAccountID: opts.StripeAccountID,
		WebhookHost:     opts.WebhookHost,
		WebhookPath:     opts.WebhookPath,
	})
	if err != nil {
		return merchant.ID{}, fmt.Errorf("register merchant slug %q: %w", opts.Slug, err)
	}
	return merchant.ID(id), nil
}

// RegisterMerchantOptions is the billing-only descriptor for RegisterMerchant.
// It carries NO auth/issuer/JWKS — auth is the host's (embedded) or AuthKit's
// (standalone). Processor refs are optional; nil leaves an existing value intact.
type RegisterMerchantOptions struct {
	Slug            string
	Name            string
	StripeAccountID *string
	WebhookHost     *string
	WebhookPath     *string
}
