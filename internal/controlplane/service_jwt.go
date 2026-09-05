package controlplane

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/verify"
)

// ResolveServiceJWT validates a first-party OIDC service JWT and resolves the
// effective OpenRails service principal.
//
// Authorization model: the token's cryptographic signature proves issuer identity;
// the issuer's STORED authority (merchant permission-group role grants in
// AuthKit) is the source of truth for what that issuer MAY do. The token's
// self-asserted `permissions` claim is treated as a REQUESTED SUBSET — it is
// intersected against stored authority so a compromised or malicious issuer
// cannot escalate by self-claiming permissions (for example `root:*`) that
// were never explicitly granted.
func (c *ControlPlane) ResolveServiceJWT(ctx context.Context, token string) (*ResolvedServiceCredential, error) {
	if c == nil || c.delegatedVerifier == nil {
		return nil, ErrNoControlPlane
	}
	c.refreshIssuerRegistryIfStale()
	principal, err := c.delegatedVerifier.VerifyServiceJWT(ctx, strings.TrimSpace(token), verify.WithServiceJWTMaxLifetime(authkit.DefaultServiceJWTLifetime))
	if err != nil {
		return nil, err
	}

	// The validated issuer (remote_application) resolves, via the merchant
	// permission-group it controls, to the merchant namespace (#567). That
	// merchant is the token's only reachable resource scope. raID is the AuthKit
	// remote_application UUID used to look up stored authority below.
	mid, mslug, ownerGroupID, ownerGroupRef, raID, err := c.merchantForIssuer(ctx, principal.Issuer)
	if err != nil {
		return nil, err
	}

	// BND-C1: intersect the token's self-asserted permissions against the
	// remote_application's STORED authority (the additive walk-up of its merchant
	// group roles, #567). A service JWT may only DOWN-SCOPE its stored grants,
	// never widen them — a merchant cannot mint a token claiming a permission its
	// remote_application was never granted.
	authority, err := c.Core().ResolveRemoteApplicationAuthority(ctx, raID)
	if err != nil {
		return nil, fmt.Errorf("controlplane: cannot resolve authority for issuer %s: %w", principal.Issuer, err)
	}
	permissions := intersectPermissions(cleanPermissionList(principal.Permissions), authority.Permissions)
	if len(permissions) == 0 {
		return nil, ErrServiceCredentialScopeDenied
	}

	// #569 (hard cut): merchant identity is the resolved permission group (mid);
	// there are no resource scopes — the token's self-asserted resources are ignored.
	return &ResolvedServiceCredential{
		OwnerGroupID:  ownerGroupID,
		OwnerGroupRef: ownerGroupRef,
		MerchantID:    mid,
		MerchantSlug:  mslug,
		Permissions:   permissions,
	}, nil
}

// intersectPermissions returns the elements of claimed that are COVERED by the
// stored authority, under AuthKit's namespace-anchored glob semantics
// (authkit.Perm.Matches). A stored grant may be a wildcard pattern — the merchant
// `owner` role resolves to the single token `merchant:*` (#567) — so a claimed
// concrete permission like `merchant:customer-settings:update` must be matched by
// COVERAGE, not exact string equality, or every owner-backed service JWT is
// wrongly denied (its stored authority is the wildcard, never the concrete perm).
//
// This is still strict down-scoping: a claim survives ONLY if some stored grant
// token covers it, so a token can never widen beyond its issuer's stored grants
// (a narrow stored grant can't cover a broader claim). Order follows claimed so
// the caller gets its preferred subset.
func intersectPermissions(claimed, stored []string) []string {
	if len(stored) == 0 || len(claimed) == 0 {
		return nil
	}
	out := make([]string, 0, len(claimed))
	for _, c := range claimed {
		for _, g := range stored {
			if authkit.Perm(c).Matches(authkit.Perm(g)) {
				out = append(out, c)
				break
			}
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
