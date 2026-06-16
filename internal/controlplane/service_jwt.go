package controlplane

import (
	"context"
	"strings"

	authcore "github.com/open-rails/authkit/core"
	authhttp "github.com/open-rails/authkit/http"
)

// ResolveServiceJWT validates a first-party OIDC service JWT and resolves the
// effective OpenRails service principal.
//
// Authorization model: registering an issuer to a tenant IS the authorization.
// A tenant has full authority over its OWN resources, so a validly-signed token
// from a registered issuer is trusted; the token's permission claims are
// authoritative (self-assigned least-privilege markers chosen by the caller for
// the step it is performing), NOT a request intersected against a separate
// server-side grant. The ONLY hard boundary is that every resource the token
// acts on must belong to the issuer's own tenant — a tenant can never reach
// another tenant's resources.
func (c *ControlPlane) ResolveServiceJWT(ctx context.Context, token string) (*ResolvedServiceToken, error) {
	if c == nil || c.delegatedVerifier == nil {
		return nil, ErrNoControlPlane
	}
	_, principal, err := c.delegatedVerifier.VerifyServiceJWT(ctx, strings.TrimSpace(token), authhttp.WithServiceJWTMaxLifetime(authcore.DefaultServiceJWTLifetime))
	if err != nil {
		return nil, err
	}

	// The validated issuer (remote_application) resolves, via the org it
	// controls, to the merchant namespace that org owns (#500). That merchant is
	// the token's only reachable resource scope.
	mid, mslug, ownerOrgID, ownerOrgSlug, err := c.merchantForIssuer(ctx, principal.Issuer)
	if err != nil {
		return nil, err
	}

	permissions := cleanPermissionList(principal.Permissions)
	if len(permissions) == 0 {
		return nil, ErrServiceTokenScopeDenied
	}

	// Self-assigned resources, defaulting to the resolved merchant when the token
	// declares none. The wall: every resource must belong to this merchant
	// (validateServiceTokenResources rejects any cross-merchant or unknown-kind
	// resource).
	resources := principal.Resources
	if len(resources) == 0 {
		resources = []authcore.ServiceTokenResource{MerchantResource(mid)}
	}
	if err := validateServiceTokenResources(mid, resources); err != nil {
		return nil, err
	}

	return &ResolvedServiceToken{
		OwnerOrgID:   ownerOrgID,
		OwnerOrgSlug: ownerOrgSlug,
		MerchantID:   mid,
		MerchantSlug: mslug,
		Permissions:  permissions,
		Resources:    resources,
	}, nil
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
