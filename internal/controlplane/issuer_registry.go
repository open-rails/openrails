package controlplane

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrDelegatedIssuerUnknown indicates a presented token's validated `iss` is not
// a registered AuthKit remote_application mapped (via org ownership) to an
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

// BrowserCORSOrigins returns the browser Origin allow-list for standalone CORS:
// the union of enabled AuthKit remote_application.allowed_origins values.
// Preflight has no JWT issuer, so this is process-wide browser policy only; API
// authorization remains JWT signature/issuer/audience/permissions plus merchant
// ownership checks.
func (c *ControlPlane) BrowserCORSOrigins(ctx context.Context) ([]string, error) {
	if c == nil || c.delegatedVerifier == nil {
		return nil, ErrDelegatedNotConfigured
	}
	return c.delegatedVerifier.RemoteApplicationAllowedOrigins(ctx)
}

// merchantForIssuer resolves the OpenRails MERCHANT a VALIDATED token issuer may
// act on (#481). The chain is role-based, never identity: validated `iss` ->
// AuthKit remote_application -> its owning org -> the merchant namespace owned by
// that org (`merchants.owner_org_id`). `owner_org_id` is unique when non-null, so
// this is a direct lookup rather than a membership scan.
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
	if strings.TrimSpace(ra.OrgID) == "" {
		return merchant.ID{}, "", "", "", "", ErrDelegatedIssuerUnknown
	}
	org, err := c.Core().ResolveOrgByID(ctx, ra.OrgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return merchant.ID{}, "", "", "", "", ErrDelegatedIssuerUnknown
		}
		return merchant.ID{}, "", "", "", "", err
	}
	if org == nil {
		return merchant.ID{}, "", "", "", "", ErrDelegatedIssuerUnknown
	}
	mid, mslug, err := c.merchantDirectoryRow(ctx, `owner_org_id = $1`, org.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return merchant.ID{}, "", "", "", "", ErrDelegatedIssuerUnknown
	}
	if err != nil {
		return merchant.ID{}, "", "", "", "", err
	}
	return mid, mslug, org.ID, org.Slug, ra.ID, nil
}
