package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ProvisionMerchantRequest parameterizes runtime merchant provisioning (#738).
type ProvisionMerchantRequest struct {
	// Slug is the merchant slug (merchant.ValidateSlug rules). Required.
	Slug string
	// OwnerUserID is an optional AuthKit user uuid seeded as the merchant
	// permission-group's owner (auto-holds `merchant:*`, #567) — ONLY when this
	// call creates the group. A pre-existing merchant's roles are never touched
	// (a registration wrapper can safely pass any authenticated user with a
	// user-chosen slug and branch on Created; a slug squatter cannot be granted
	// ownership of someone else's merchant). Re-runs stay idempotent: the
	// creating call already seeded the owner. Later owner changes go through
	// Core().Genesis().AssignGroupRole explicitly (authkit v0.79.0, #241).
	OwnerUserID string
}

// ProvisionMerchantResult reports what ProvisionMerchant ensured.
type ProvisionMerchantResult struct {
	// MerchantID is the openrails.merchants directory row id.
	MerchantID merchant.ID
	// GroupID is the merchant permission-group's internal AuthKit id (#567).
	GroupID string
	// Created is false when the merchant already existed (both the permission
	// group and the directory row); true when this call created either.
	Created bool
}

// ProvisionMerchant idempotently provisions a merchant at runtime through the
// attached control plane (#738): ensures root-group containment, resolves or
// creates the merchant permission-group (persona=merchant, parent=root, owner =
// req.OwnerUserID), and creates/links the openrails.merchants directory row
// with the group's id. Safe to re-run.
//
// This is the engine mechanism behind a hosted wrapper's "registration is
// provisioning" flow (openrails-saas #2/#3): it works in both merchant_source
// modes but exists for merchant_source=api, where merchants are created over
// code paths instead of a manifest. Calling it without an attached control
// plane is a wiring error (call Attach/AttachWithOptions first).
func ProvisionMerchant(ctx context.Context, a *app.App, req ProvisionMerchantRequest) (*ProvisionMerchantResult, error) {
	cp := Get(a)
	if cp == nil {
		return nil, fmt.Errorf("control plane provision: no control plane attached (call Attach first)")
	}
	core := cp.Core()
	if core == nil {
		return nil, fmt.Errorf("control plane provision: core service unavailable")
	}
	slug := merchant.NormalizeSlug(req.Slug)
	if err := merchant.ValidateSlug(slug); err != nil {
		return nil, err
	}
	owner := strings.TrimSpace(req.OwnerUserID)

	// Root group + declared containment first (idempotent), as bootstrap does.
	if _, err := core.EnsureRootGroup(ctx); err != nil {
		return nil, fmt.Errorf("control plane provision: ensure root group: %w", err)
	}
	if err := core.SeedPermissionGroupContainment(ctx); err != nil {
		return nil, fmt.Errorf("control plane provision: seed permission-group containment: %w", err)
	}

	// Resolve-or-create the merchant permission-group — the merchant IS the
	// group (#567): type=merchant, resourceRef=slug, parent=root.
	groupCreated := false
	groupID, err := core.ResolveGroupIDForSlug(ctx, controlplane.MerchantType, slug)
	switch {
	case errors.Is(err, authkit.ErrGroupNotFound):
		groupID, err = core.CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{
			Persona:        controlplane.MerchantType,
			InstanceSlug:   slug,
			ParentPersona:  authcore.RootPersona,
			OwnerSubjectID: owner,
		})
		if err != nil {
			return nil, fmt.Errorf("control plane provision: create merchant group %q: %w", slug, err)
		}
		groupCreated = true
	case err != nil:
		return nil, fmt.Errorf("control plane provision: resolve merchant group %q: %w", slug, err)
	}

	// Create/link the directory row (permission_group_id) via the merchants
	// lifecycle slice — the same row bootstrap requires to pre-exist (#480);
	// runtime provisioning creates it.
	dir, err := merchants.NewDirectoryService(cp.Pool())
	if err != nil {
		return nil, fmt.Errorf("control plane provision: build merchant directory service: %w", err)
	}
	rowExisted := true
	if _, gerr := dir.GetBySlug(ctx, slug); errors.Is(gerr, merchants.ErrMerchantNotFound) {
		rowExisted = false
	} else if gerr != nil {
		return nil, fmt.Errorf("control plane provision: resolve merchant directory row %q: %w", slug, gerr)
	}
	m, err := dir.Provision(ctx, merchants.ProvisionRequest{Slug: slug, PermissionGroupID: groupID})
	if err != nil {
		return nil, fmt.Errorf("control plane provision: provision merchant directory row %q: %w", slug, err)
	}

	return &ProvisionMerchantResult{
		MerchantID: m.ID,
		GroupID:    groupID,
		Created:    groupCreated || !rowExisted,
	}, nil
}
