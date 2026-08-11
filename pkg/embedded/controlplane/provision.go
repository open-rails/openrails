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
	// so counting it here would let two racers both report true. A call that
	// only (re)created the group for an already-listed merchant is a repair of
	// an existing merchant and reports false.
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
	// the loser proceeds exactly like any other already-provisioned merchant —
	// the SAME retry-resolve pattern EnsureCustomerPermissionGroup already uses
	// for the identical race on customer groups (customer_group.go). Only when
	// the re-resolve ALSO fails do we conclude CreatePermissionGroup's error was
	// a genuine failure (DB outage, etc.), not a slug race, and return it
	// wrapped as before (hosts 500 it).
	//
	// No orphaned group can survive this race: CreateGroup runs inside authkit's
	// own transaction and the loser's INSERT is rejected by
	// permission_groups_persona_instance_uidx, so nothing is committed to clean
	// up — both callers end up holding the winner's single group id (#898).
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
	// or#914: a USER-claimed slug (owner set) on a creation-enabled deployment
	// answers to the SAME declared policy authkit enforces on its generated
	// POST /merchant route — the reserved-slug list, the extra slug pattern
	// and the host admission (cost) gate. Refusals are typed
	// (controlplane re-exports below: ErrSlugReserved / ErrCreationRefused)
	// for hosts to map onto 4xx. The generated route is the canonical hosted
	// creation path (it additionally rate-limits per IP/user and attaches the
	// directory row itself); this keeps the in-process API from being a side
	// door around the deployment's own policy. Operator paths (owner == "",
	// Bootstrap, manifests) are not user claims and stay ungated.
	if err := cp.EnforceMerchantCreationPolicy(ctx, slug, owner); err != nil {
		return nil, fmt.Errorf("control plane provision: %w", err)
	}

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
			resolvedID, rerr := core.ResolveGroupIDForSlug(ctx, controlplane.MerchantType, slug)
			if rerr != nil {
				return nil, fmt.Errorf("control plane provision: create merchant group %q: %w", slug, createErr)
			}
			groupID = resolvedID
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
	// "did not yet exist" and both wrongly report Created=true. It is the SOLE
	// input to Created for the same reason (#898).
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
		Created:    rowCreated,
	}, nil
}
