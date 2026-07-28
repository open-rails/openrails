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

// ErrInvalidSlug wraps pkg/merchant's slug-validation error (errors.Is-able)
// so a host can map a bad slug to 400 without string-matching the error
// text. The validation detail (pkg/merchant.ValidateSlug's own message) is
// preserved in the wrap.
var ErrInvalidSlug = errors.New("control plane provision: invalid slug")

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
		return nil, fmt.Errorf("%w: %w", ErrInvalidSlug, err)
	}
	owner := strings.TrimSpace(req.OwnerUserID)

	// Root group + declared containment first (idempotent, concurrent-boot
	// tolerant — #844), as bootstrap does.
	if err := controlplane.EnsureRootContainment(ctx, core); err != nil {
		return nil, fmt.Errorf("control plane provision: %w", err)
	}

	// Resolve-or-create the merchant permission-group — the merchant IS the
	// group (#567): type=merchant, resourceRef=slug, parent=root.
	//
	// Race handling (#750): two concurrent first-creates of the same fresh
	// slug both observe ErrGroupNotFound and race CreatePermissionGroup; the
	// loser hits the DB's uniqueness constraint on (persona, instance_slug),
	// which authkit's PermissionGroupStore.CreateGroup wraps in a bare
	// fmt.Errorf (no stable "already exists" sentinel to errors.Is against —
	// confirmed against authkit v0.80.0). Rather than string-matching that
	// error, the loser re-resolves once: if the winner's row is now visible,
	// the loser proceeds exactly like any other already-provisioned merchant
	// (groupCreated stays false) — the SAME retry-resolve pattern
	// EnsureCustomerPermissionGroup already uses for the identical race on
	// customer groups (customer_group.go). Only when the re-resolve ALSO
	// fails do we conclude CreatePermissionGroup's error was a genuine
	// failure (DB outage, etc.), not a slug race, and return it wrapped as
	// before (hosts 500 it).
	//
	// Deliberately no ErrSlugTaken sentinel: the retry-resolve converts every
	// race loser into the ordinary Created=false idempotent path callers
	// already handle — a genuine slug conflict (a pre-existing merchant owned
	// by someone else) is ALSO Created=false, and hosts distinguish "mine"
	// vs. "taken" by checking ownership afterward (see
	// openrails-saas/internal/api/accounts.go's handleCreateMerchant). If a
	// future authkit release exposes a stable duplicate-group sentinel,
	// mapping it to a typed ErrSlugTaken here — skipping the extra
	// round-trip — is a non-breaking follow-up.
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
			createErr := err
			if resolvedID, rerr := core.ResolveGroupIDForSlug(ctx, controlplane.MerchantType, slug); rerr == nil {
				groupID = resolvedID
			} else {
				return nil, fmt.Errorf("control plane provision: create merchant group %q: %w", slug, createErr)
			}
		} else {
			groupCreated = true
		}
	case err != nil:
		return nil, fmt.Errorf("control plane provision: resolve merchant group %q: %w", slug, err)
	}

	// Create/link the directory row (permission_group_id) via the merchants
	// lifecycle slice — the same row bootstrap requires to pre-exist (#480);
	// runtime provisioning creates it. Provision's own `rowCreated` return
	// (derived from its INSERT ... RETURNING, not a separate existence check)
	// is what stays race-safe under concurrent first-provisions of the same
	// slug — a pre-check-then-insert here would let both racers observe
	// "did not yet exist" and both wrongly report Created=true.
	dir, err := merchants.NewDirectoryService(cp.Pool())
	if err != nil {
		return nil, fmt.Errorf("control plane provision: build merchant directory service: %w", err)
	}
	m, rowCreated, err := dir.Provision(ctx, merchants.ProvisionRequest{Slug: slug, PermissionGroupID: groupID})
	if err != nil {
		return nil, fmt.Errorf("control plane provision: provision merchant directory row %q: %w", slug, err)
	}

	return &ProvisionMerchantResult{
		MerchantID: m.ID,
		GroupID:    groupID,
		Created:    groupCreated || rowCreated,
	}, nil
}
