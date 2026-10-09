package controlplane

// Merchant self-serve API keys (#757): the control-plane surface behind
// /v1/merchant/api-keys, through AuthKit's Client (CreateAPIKey, ListAPIKeys,
// RevokeAPIKey) — never raw AuthKit SQL. A key holds one of the merchant roles
// (#567): owner, support or viewer, the read-only role for LLM agents.

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/open-rails/authkit/iam"
	helpersauth "github.com/open-rails/helpers/auth"

	"github.com/open-rails/openrails/billing"
)

// ErrUnknownMerchantRole indicates a role outside the merchant roles (#567:
// merchants have no custom roles).
var ErrUnknownMerchantRole = errors.New("controlplane: unknown merchant role")

// MerchantAPIKey is the non-secret view of a merchant API key served by the
// self-serve surface. The secret exists only in the mint response; Prefix is
// the non-secret leading token part ("openrails_st_<lookup id>") a holder can
// match against a stored credential.
type MerchantAPIKey = billing.APIKey

// MintMerchantAPIKey mints a key under the merchant's group holding role, as
// actor: AuthKit requires merchant:credentials:manage and coverage of the
// role. A non-user principal mints as the system, after the caller enforced
// its no-escalation rule (the route gate plus RoleCoveredBy). The
// secret is returned once: it is never stored and never retrievable again.
func (c *ControlPlane) MintMerchantAPIKey(ctx context.Context, mid billing.MerchantID, name string, role iam.Role, actor helpersauth.Identity) (MerchantAPIKey, string, error) {
	if !slices.Contains(MerchantRoles(), role) {
		return MerchantAPIKey{}, "", ErrUnknownMerchantRole
	}
	group, err := c.merchantGroup(ctx, mid)
	if err != nil {
		return MerchantAPIKey{}, "", err
	}
	created, err := c.client.CreateAPIKey(ctx, actor, group, iam.NewAPIKey{Name: strings.TrimSpace(name), Role: role})
	if err != nil {
		return MerchantAPIKey{}, "", err
	}
	return c.merchantAPIKeyView(created.APIKey), created.Secret, nil
}

// ListMerchantAPIKeys returns every key of the merchant's group — live,
// expired and revoked (status is the audit view) — never secret material.
func (c *ControlPlane) ListMerchantAPIKeys(ctx context.Context, mid billing.MerchantID) ([]MerchantAPIKey, error) {
	group, err := c.merchantGroup(ctx, mid)
	if err != nil {
		return nil, err
	}
	var out []MerchantAPIKey
	for k, err := range iam.All(func(p iam.PageRequest) (iam.ListPage[iam.APIKey], error) {
		return c.client.ListAPIKeys(ctx, group, p)
	}) {
		if err != nil {
			return nil, err
		}
		out = append(out, c.merchantAPIKeyView(k))
	}
	return out, nil
}

// RevokeMerchantAPIKey revokes the merchant's key id as actor (a key is never
// revoked across merchants). It returns false when the merchant has no key
// with that id.
func (c *ControlPlane) RevokeMerchantAPIKey(ctx context.Context, mid billing.MerchantID, id string, actor helpersauth.Identity) (bool, error) {
	group, err := c.merchantGroup(ctx, mid)
	if err != nil {
		return false, err
	}
	err = c.client.RevokeAPIKey(ctx, actor, group, strings.TrimSpace(id))
	if errors.Is(err, iam.ErrAPIKeyNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (c *ControlPlane) merchantAPIKeyView(k iam.APIKey) MerchantAPIKey {
	return MerchantAPIKey{
		ID:         k.ID,
		Name:       k.Name,
		Role:       k.Role.Name(),
		Prefix:     c.TokenPrefix() + "_st_" + k.LookupID,
		CreatedAt:  k.CreatedAt,
		LastUsedAt: k.LastUsedAt,
		ExpiresAt:  k.ExpiresAt,
		RevokedAt:  k.RevokedAt,
	}
}
