package controlplane

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrDelegatedIssuerUnknown indicates a presented token's validated `iss` is not
// a registered AuthKit remote_application mapped via permission-group ownership to an
// active merchant. Fail closed: the token is rejected even if well-formed.
var ErrDelegatedIssuerUnknown = errors.New("controlplane: delegated token issuer maps to no active merchant")

// loadRemoteApplications loads AuthKit's ACTIVE remote_applications into the
// delegated verifier (#481): standalone JWKS/issuer trust is AuthKit's
// remote_application registry (#74), NOT an OpenRails-owned table (the
// old delegated-issuer registry was dropped in #480). The verifier's
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

// BrowserCORSOrigins returns the default browser Origin allow-list for standalone
// CORS. AuthKit no longer owns remote-application origins, so the control plane
// has no AuthKit-derived browser allow-list; hosts that want browser CORS should
// wire an OpenRails-owned source at the server layer.
func (c *ControlPlane) BrowserCORSOrigins(ctx context.Context) ([]string, error) {
	_ = ctx
	if c == nil || c.delegatedVerifier == nil {
		return nil, ErrDelegatedNotConfigured
	}
	return nil, nil
}

// merchantForIssuer resolves the OpenRails MERCHANT a VALIDATED token issuer may
// act on (#567). The chain is group-based, never identity: validated `iss` ->
// AuthKit remote_application -> its controlling permission-group id (the merchant
// group) -> the merchant directory row whose recorded controlling group id
// matches (`merchants.permission_group_id`, repurposed under #567 to hold the merchant
// permission-group's internal id).
//
// Returns ErrDelegatedIssuerUnknown when the issuer is unregistered, is attached
// to no group, or that group is no active merchant (fail closed). The returned
// groupID/groupRef identify the merchant permission-group.
func (c *ControlPlane) merchantForIssuer(ctx context.Context, issuer string) (merchantID merchant.ID, merchantSlug, groupID, groupRef, remoteApplicationID string, err error) {
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
	// RemoteApplication.PermissionGroupID carries the controlling permission_group_id (#111):
	// the merchant group this issuer signs for. Empty => attached to nothing.
	groupID = strings.TrimSpace(ra.PermissionGroupID)
	if groupID == "" {
		return merchant.ID{}, "", "", "", "", ErrDelegatedIssuerUnknown
	}
	mid, mslug, err := c.merchantDirectoryRow(ctx, `permission_group_id = $1`, groupID)
	if errors.Is(err, pgx.ErrNoRows) {
		return merchant.ID{}, "", "", "", "", ErrDelegatedIssuerUnknown
	}
	if err != nil {
		return merchant.ID{}, "", "", "", "", err
	}
	return mid, mslug, groupID, mslug, ra.ID, nil
}
