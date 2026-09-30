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
	"time"

	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrUnknownMerchantRole indicates a role outside the merchant roles (#567:
// merchants have no custom roles).
var ErrUnknownMerchantRole = errors.New("controlplane: unknown merchant role")

// MerchantAPIKey is the non-secret view of a merchant API key served by the
// self-serve surface. The secret exists only in the mint response; Prefix is
// the non-secret leading token part ("openrails_st_<lookup id>") a holder can
// match against a stored credential.
type MerchantAPIKey struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Role       string     `json:"role"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// MintMerchantAPIKey mints a key under the merchant's group holding role, as
// actor: AuthKit requires merchant:credentials:manage and coverage of the
// role. A non-user principal mints as the system, after the caller enforced
// its no-escalation rule (the route gate plus MerchantRoleCoveredBy). The
// secret is returned once: it is never stored and never retrievable again.
func (c *ControlPlane) MintMerchantAPIKey(ctx context.Context, mid merchant.ID, name string, role iam.Role, actor iam.Actor) (MerchantAPIKey, string, error) {
	if !slices.Contains(MerchantAPIKeyRoles(), role) {
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
func (c *ControlPlane) ListMerchantAPIKeys(ctx context.Context, mid merchant.ID) ([]MerchantAPIKey, error) {
	group, err := c.merchantGroup(ctx, mid)
	if err != nil {
		return nil, err
	}
	var out []MerchantAPIKey
	page := iam.PageRequest{Limit: iam.MaxPageLimit}
	for {
		batch, err := c.client.ListAPIKeys(ctx, group, page)
		if err != nil {
			return nil, err
		}
		for _, k := range batch.Items {
			out = append(out, c.merchantAPIKeyView(k))
		}
		if batch.Next == "" {
			return out, nil
		}
		page.Cursor = batch.Next
	}
}

// RevokeMerchantAPIKey revokes the merchant's key id as actor (a key is never
// revoked across merchants). It returns false when the merchant has no key
// with that id.
func (c *ControlPlane) RevokeMerchantAPIKey(ctx context.Context, mid merchant.ID, id string, actor iam.Actor) (bool, error) {
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
