package controlplane

import (
	"context"
	"fmt"
	"strings"

	authcore "github.com/open-rails/authkit/core"
	authhttp "github.com/open-rails/authkit/http"
)

// ResolveServiceJWT validates a first-party OIDC service JWT and resolves the
// effective OpenRails service principal.
//
// Authorization model: the token's cryptographic signature proves issuer identity;
// the issuer's STORED authority (direct permission grants + org-role expansion in
// AuthKit) is the source of truth for what that issuer MAY do. The token's
// self-asserted `permissions` claim is treated as a REQUESTED SUBSET — it is
// intersected against stored authority so a compromised or malicious issuer
// cannot escalate by self-claiming permissions (including `org:*`) that
// were never explicitly granted.
func (c *ControlPlane) ResolveServiceJWT(ctx context.Context, token string) (*ResolvedServiceCredential, error) {
	if c == nil || c.delegatedVerifier == nil {
		return nil, ErrNoControlPlane
	}
	_, principal, err := c.delegatedVerifier.VerifyServiceJWT(ctx, strings.TrimSpace(token), authhttp.WithServiceJWTMaxLifetime(authcore.DefaultServiceJWTLifetime))
	if err != nil {
		return nil, err
	}

	// The validated issuer (remote_application) resolves, via the org it
	// controls, to the merchant namespace that org owns (#500). That merchant is
	// the token's only reachable resource scope. raID is the AuthKit
	// remote_application UUID used to look up stored authority below.
	mid, mslug, ownerOrgID, ownerOrgSlug, raID, err := c.merchantForIssuer(ctx, principal.Issuer)
	if err != nil {
		return nil, err
	}

	// BND-C1: intersect the token's self-asserted permissions against the
	// remote_application's STORED authority. A service JWT may only DOWN-SCOPE
	// its stored grants, never widen them. This prevents a merchant from minting
	// a token that claims `org:*` (or any other permission not explicitly
	// granted to their remote_application).
	_, storedPerms, err := c.Core().ResolveRemoteApplicationAuthority(ctx, raID)
	if err != nil {
		return nil, fmt.Errorf("controlplane: cannot resolve authority for issuer %s: %w", principal.Issuer, err)
	}
	permissions := intersectPermissions(cleanPermissionList(principal.Permissions), storedPerms)
	if len(permissions) == 0 {
		return nil, ErrServiceCredentialScopeDenied
	}

	// Self-assigned resources, defaulting to the resolved merchant when the token
	// declares none. The wall: every resource must belong to this merchant
	// (validateAPIKeyResources rejects any cross-merchant or unknown-kind
	// resource).
	resources := principal.Resources
	if len(resources) == 0 {
		resources = []authcore.APIKeyResource{MerchantResource(mid)}
	}
	if err := validateAPIKeyResources(mid, resources); err != nil {
		return nil, err
	}

	return &ResolvedServiceCredential{
		OwnerOrgID:   ownerOrgID,
		OwnerOrgSlug: ownerOrgSlug,
		MerchantID:   mid,
		MerchantSlug: mslug,
		Permissions:  permissions,
		Resources:    resources,
	}, nil
}

// intersectPermissions returns the elements of claimed that also appear in
// stored. Order follows claimed so the caller gets its preferred subset.
func intersectPermissions(claimed, stored []string) []string {
	if len(stored) == 0 || len(claimed) == 0 {
		return nil
	}
	grant := make(map[string]bool, len(stored))
	for _, p := range stored {
		grant[p] = true
	}
	out := make([]string, 0, len(claimed))
	for _, p := range claimed {
		if grant[p] {
			out = append(out, p)
		}
	}
	return out
}

func cleanPermissionList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}
