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
	// Slug selects a merchant name. Required unless ExistingGroupID is supplied.
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
	// ExistingGroupID carries a group already selected for owner repair. This
	// path checks live ownership and never resolves or creates another group.
	// It can complete a missing billing attachment for that same group UUID.
	ExistingGroupID string
}

// ErrInvalidSlug wraps pkg/merchant's slug-validation error (errors.Is-able)
// so a host can map a bad slug to 400 without string-matching the error
// text. The validation detail (pkg/merchant.ValidateSlug's own message) is
// preserved in the wrap.
var ErrInvalidSlug = errors.New("control plane provision: invalid slug")

// ErrSlugReserved / ErrCreationRefused re-export the or#914 in-process
// creation-policy refusals (errors.Is-able) so hosts can map a reserved slug
// or an admission-gate refusal onto 4xx without importing internals.
var (
	ErrSlugReserved    = controlplane.ErrMerchantSlugReserved
	ErrCreationRefused = controlplane.ErrMerchantCreationRefused
)

// ProvisionMerchantResult reports what ProvisionMerchant ensured.
type ProvisionMerchantResult struct {
	// MerchantID is the openrails.merchants directory row id.
	MerchantID merchant.ID
	// GroupID is the merchant permission-group's internal AuthKit id (#567).
	GroupID string
	// Created reports whether THIS call brought the merchant into existence,
	// arbitrated solely by the openrails.merchants directory row insert — the
	// one step whose winner the database decides (#898). The permission group
	// is resolve-or-create and its winner can be a DIFFERENT concurrent caller,
	// so counting it here would let two racers both report true. Attaching a
	// missing billing row to an existing group reports true; repeating an
	// already-attached group reports false.
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
	if strings.TrimSpace(req.ExistingGroupID) == "" {
		if err := merchant.ValidateSlug(slug); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidSlug, err)
		}
	}
	owner := strings.TrimSpace(req.OwnerUserID)

	// Root group + declared containment first (idempotent, concurrent-boot
	// tolerant — #844), as bootstrap does.
	if err := controlplane.EnsureRootContainment(ctx, core); err != nil {
		return nil, fmt.Errorf("control plane provision: %w", err)
	}

	groupID := strings.TrimSpace(req.ExistingGroupID)
	if groupID != "" {
		group, err := core.GroupInstanceByID(ctx, groupID)
		if err != nil {
			return nil, err
		}
		if group.Persona != controlplane.MerchantType || owner == "" {
			return nil, authkit.ErrInsufficientRoleAuthority
		}
		allowed, err := core.CanOnGroup(ctx, owner, authcore.SubjectKindUser, groupID, authcore.OwnerGrant(controlplane.MerchantType))
		if err != nil {
			return nil, err
		}
		if !allowed {
			return nil, authkit.ErrInsufficientRoleAuthority
		}
	} else {
		// Capture an existing group before any host admission callback. A rename
		// while that callback runs must not turn an existing group into creation.
		var err error
		groupID, err = core.ResolveGroupIDForSlug(ctx, controlplane.MerchantType, slug)
		if err != nil && !errors.Is(err, authkit.ErrGroupNotFound) {
			return nil, fmt.Errorf("control plane provision: resolve merchant group %q: %w", slug, err)
		}
		if err := cp.EnforceMerchantCreationPolicy(ctx, slug, owner); err != nil {
			return nil, fmt.Errorf("control plane provision: %w", err)
		}
		if groupID == "" {
			groupID, err = core.CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{
				Persona: controlplane.MerchantType, InstanceSlug: slug,
				ParentPersona: authcore.RootPersona, OwnerSubjectID: owner,
			})
			if err != nil {
				// A concurrent first creation may have won. Adopt its identity;
				// existing roles remain untouched and callers verify ownership.
				createdID, resolveErr := core.ResolveGroupIDForSlug(ctx, controlplane.MerchantType, slug)
				if resolveErr != nil {
					return nil, fmt.Errorf("control plane provision: create merchant group %q: %w", slug, err)
				}
				groupID = createdID
			}
		}
	}

	// Create/link the directory row (permission_group_id) via the merchants
	// lifecycle slice — the same row bootstrap requires to pre-exist (#480);
	// runtime provisioning creates it. Provision's own `rowCreated` return
	// (derived from its INSERT ... RETURNING, not a separate existence check)
	// is what stays race-safe under concurrent first-provisions of the same
	// slug — a pre-check-then-insert here would let both racers observe
	// "did not yet exist" and both wrongly report Created=true. It is the SOLE
	// input to Created for the same reason (#898).
	dir, err := merchants.NewDirectoryService(cp.Pool())
	if err != nil {
		return nil, fmt.Errorf("control plane provision: build merchant directory service: %w", err)
	}
	canonical, err := core.GroupInstanceByID(ctx, groupID)
	if err != nil {
		return nil, err
	}
	m, rowCreated, err := dir.Provision(ctx, merchants.ProvisionRequest{Slug: canonical.InstanceSlug, PermissionGroupID: groupID})
	if err != nil {
		return nil, fmt.Errorf("control plane provision: provision merchant directory row %q: %w", slug, err)
	}

	// Provision also sees historical bindings so it cannot allocate a second
	// billing identity. A deleted row is never a successful active repair.
	if _, err := dir.Get(ctx, m.ID); err != nil {
		return nil, err
	}
	return &ProvisionMerchantResult{
		MerchantID: m.ID,
		GroupID:    groupID,
		Created:    rowCreated,
	}, nil
}
