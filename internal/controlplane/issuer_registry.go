package controlplane

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrDelegatedIssuerUnknown indicates a presented token's validated `iss` is not
// a registered AuthKit remote_application mapped (via tenant ownership) to an
// active merchant. Fail closed: the token is rejected even if well-formed.
var ErrDelegatedIssuerUnknown = errors.New("controlplane: delegated token issuer maps to no active merchant")

// loadRemoteApplications loads AuthKit's ACTIVE remote_applications into the
// delegated verifier (#481): standalone JWKS/issuer trust is AuthKit's
// remote_application registry (#74), NOT an OpenRails-owned table (the
// tenant_delegated_issuers registry was dropped in #480). The verifier's
// in-house JWKS fetch/refresh handles keys; this is also re-callable to pick up
// store changes, and the verifier lazy-loads any single issuer on first use.
func (c *ControlPlane) loadRemoteApplications(ctx context.Context) error {
	if c == nil || c.delegatedVerifier == nil {
		return ErrDelegatedNotConfigured
	}
	return c.delegatedVerifier.LoadRemoteApplications(ctx, c.Core(), c.delegatedAudiences)
}

// ReloadRemoteApplications re-syncs the verifier's in-memory issuer registry with
// AuthKit's remote_application store, picking up newly registered/disabled
// principals (the verifier also lazy-loads any single issuer on first use, so
// this is for deterministic reloads — e.g. after an inbound registration).
func (c *ControlPlane) ReloadRemoteApplications(ctx context.Context) error {
	return c.loadRemoteApplications(ctx)
}

// merchantForIssuer resolves the OpenRails MERCHANT a VALIDATED token issuer may
// act on (#481). The chain is role-based, never identity: validated `iss` ->
// AuthKit remote_application -> the org(s) it controls (its org memberships) ->
// the merchant namespace owned by that org (merchants.owner_org_id).
// A remote_application controls an org and an org owns merchant namespaces, so this is
// the runtime billing-authz path: the remote_application is authorized to bill
// the merchant namespace its owning org owns.
//
// Returns ErrDelegatedIssuerUnknown when the issuer is unregistered, controls no
// org, or that org owns no active merchant (fail closed).
func (c *ControlPlane) merchantForIssuer(ctx context.Context, issuer string) (merchantID merchant.ID, merchantSlug, ownerOrgID, ownerOrgSlug, remoteApplicationID string, err error) {
	issuer = strings.TrimSpace(issuer)
	if issuer == "" {
		return merchant.ID{}, "", "", "", "", ErrDelegatedIssuerUnknown
	}
	if c == nil || c.Core() == nil || c.pool == nil {
		return merchant.ID{}, "", "", "", "", errors.New("controlplane: control plane unavailable for issuer resolution")
	}

	ra, err := c.Core().GetRemoteApplication(ctx, issuer)
	if err != nil || ra == nil || !ra.Enabled {
		return merchant.ID{}, "", "", "", "", ErrDelegatedIssuerUnknown
	}

	// The org(s) the remote_application controls (its memberships). Resolve the
	// first one that owns exactly one active merchant namespace.
	memberships, err := c.Core().RemoteApplicationOrgRoles(ctx, ra.ID)
	if err != nil {
		return merchant.ID{}, "", "", "", "", err
	}
	for _, m := range memberships {
		t, terr := c.Core().ResolveOrgBySlug(ctx, m.Org)
		if terr != nil || t == nil {
			continue
		}
		mid, mslug, merr := c.merchantDirectoryRow(ctx, `owner_org_id = $1`, t.ID)
		if merr == nil {
			return mid, mslug, t.ID, t.Slug, ra.ID, nil
		}
		if !errors.Is(merr, pgx.ErrNoRows) {
			return merchant.ID{}, "", "", "", "", merr
		}
	}
	return merchant.ID{}, "", "", "", "", ErrDelegatedIssuerUnknown
}
