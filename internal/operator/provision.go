package operator

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ProvisionMerchantRequest parameterizes runtime merchant provisioning (#738).
type ProvisionMerchantRequest struct {
	// Slug is the merchant name to claim.
	Slug string
	// OwnerUserID is an optional AuthKit user uuid seeded as the merchant
	// permission-group's owner (auto-holds `merchant:*`, #567) — ONLY when this
	// call creates the merchant. An existing merchant's roles are never touched
	// (a registration wrapper can safely pass any authenticated user with a
	// user-chosen name and branch on Created; a squatter cannot be granted
	// ownership of someone else's merchant). A user claim answers to the
	// declared creation policy (reserved names, pattern, admission).
	OwnerUserID string
}

// ErrInvalidSlug wraps pkg/merchant's slug-validation error (errors.Is-able)
// so a host can map a bad slug to 400 without string-matching the error
// text. The validation detail (pkg/merchant.ValidateSlug's own message) is
// preserved in the wrap.
var ErrInvalidSlug = errors.New("control plane provision: invalid slug")

// ErrSlugReserved / ErrCreationRefused / ErrMerchantNameTaken re-export the
// or#914 creation-policy refusals and a taken name (errors.Is-able) so hosts
// can map them onto 4xx without importing internals.
var (
	ErrSlugReserved      = controlplane.ErrMerchantSlugReserved
	ErrCreationRefused   = controlplane.ErrMerchantCreationRefused
	ErrMerchantNameTaken = merchants.ErrMerchantNameTaken
)

// ProvisionMerchantResult reports what ProvisionMerchant ensured.
type ProvisionMerchantResult struct {
	// MerchantID is the billing.merchants directory row id.
	MerchantID merchant.ID
	// GroupID is the merchant permission-group's internal AuthKit id (#567);
	// empty for an existing host-owned merchant without one.
	GroupID string
	// Created reports whether THIS call claimed the name (#898): the merchant
	// row insert is the one step whose winner the database decides.
	Created bool
}

// ProvisionMerchant idempotently provisions a merchant at runtime through the
// attached control plane (#738): the live merchant the name resolves to, or a
// new one claiming it, bound to a new merchant permission-group (persona
// merchant, parent root, owner req.OwnerUserID). Safe to re-run.
//
// This is the engine mechanism behind a hosted wrapper's "registration is
// provisioning" flow. Calling it without an attached control plane is a wiring
// error (call Attach/AttachWithOptions first).
func ProvisionMerchant(ctx context.Context, a *app.App, req ProvisionMerchantRequest) (*ProvisionMerchantResult, error) {
	cp := Get(a)
	if cp == nil || cp.Core() == nil {
		return nil, fmt.Errorf("control plane provision: no control plane attached (call Attach first)")
	}
	slug := merchant.NormalizeSlug(req.Slug)
	if err := merchant.ValidateSlug(slug); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidSlug, err)
	}
	if err := cp.EnforceMerchantCreationPolicy(ctx, slug, req.OwnerUserID); err != nil {
		return nil, fmt.Errorf("control plane provision: %w", err)
	}
	m, created, err := cp.ProvisionMerchant(ctx, slug, req.OwnerUserID)
	if err != nil {
		return nil, fmt.Errorf("control plane provision %q: %w", slug, err)
	}
	return &ProvisionMerchantResult{MerchantID: m.ID, GroupID: m.PermissionGroupID, Created: created}, nil
}
