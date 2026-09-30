package controlplane

import (
	"context"
	"errors"
	"strings"

	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/verify"
)

// ResolveServiceJWT validates a first-party service JWT and resolves the
// effective OpenRails service principal.
//
// The signature proves the issuer; the issuer's STORED authority (its remote
// application's role in the merchant group it controls) is what it may do.
// The token's `permissions` claim is a REQUESTED SUBSET, intersected with
// that authority, so an issuer cannot escalate by self-claiming permissions
// (for example `root:*`) it was never granted.
func (c *ControlPlane) ResolveServiceJWT(ctx context.Context, token string) (*ResolvedServiceCredential, error) {
	if c == nil || c.delegatedVerifier == nil {
		return nil, ErrNoControlPlane
	}
	claims, err := c.delegatedVerifier.VerifyServiceJWT(ctx, strings.TrimSpace(token), verify.WithServiceJWTMaxLifetime(iam.DefaultServiceJWTLifetime))
	if err != nil {
		return nil, err
	}
	app, err := c.client.RemoteApplication(ctx, iam.AppByIssuer(claims.Issuer))
	switch {
	case errors.Is(err, iam.ErrRemoteApplicationNotFound):
		return nil, ErrDelegatedIssuerUnknown
	case err != nil:
		return nil, err
	case !app.Enabled:
		return nil, ErrDelegatedIssuerUnknown
	}
	mid, slug, err := c.merchantForAppGroup(ctx, app.GroupID)
	if err != nil {
		return nil, err
	}
	stored := make([]string, len(app.Permissions))
	for i, perm := range app.Permissions {
		stored[i] = perm.String()
	}
	permissions := intersectPermissions(cleanPermissionList(claims.Permissions), stored)
	if len(permissions) == 0 {
		return nil, ErrServiceCredentialScopeDenied
	}
	return &ResolvedServiceCredential{
		OwnerGroupID:  app.GroupID,
		OwnerGroupRef: slug,
		MerchantID:    mid,
		MerchantSlug:  slug,
		Permissions:   permissions,
	}, nil
}

// intersectPermissions returns the claimed permissions some stored grant
// covers (iam.Perm.Matches: the owner's `merchant:*` covers every concrete
// merchant permission), in claimed order. A claim never widens beyond the
// stored grants.
func intersectPermissions(claimed, stored []string) []string {
	if len(stored) == 0 || len(claimed) == 0 {
		return nil
	}
	out := make([]string, 0, len(claimed))
	for _, text := range claimed {
		var perm iam.Perm
		if perm.UnmarshalText([]byte(text)) == nil && covered(perm, stored) {
			out = append(out, text)
		}
	}
	return out
}

func cleanPermissionList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, p := range in {
		p = strings.TrimSpace(p)
		if _, ok := seen[p]; ok || p == "" {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}
