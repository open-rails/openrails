package controlplane

// Merchant self-serve API keys (#757): the control-plane surface behind
// /v1/merchant/api-keys. Everything goes through AuthKit CORE calls
// (MintAPIKeyWithOptions / ListAPIKeys / RevokeAPIKey) — never raw AuthKit SQL
// (bootstrap.go doctrine). Keys are minted under the merchant permission-group
// against one of the FIXED merchant catalog roles (#567): owner / support /
// viewer. `viewer` is the metrics/read-only role for LLM agents.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/open-rails/authkit"

	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrUnknownMerchantRole indicates a requested API-key role outside the fixed
// merchant catalog (#567: owner/support/viewer — merchants have no custom roles).
var ErrUnknownMerchantRole = errors.New("controlplane: unknown merchant role")

// MerchantAPIKey is the non-secret view of a merchant API key served by the
// self-serve surface. The secret exists only in the mint response; Prefix is
// the non-secret leading token part ("openrails_st_<key_id>") a holder can
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

// MerchantRoles returns the fixed merchant catalog roles (#567), least privilege
// first. The one source of truth for the role vocabulary shared by API keys
// (#757) and team membership (#760).
func MerchantRoles() []string {
	return []string{MerchantRoleViewer, MerchantRoleSupport, MerchantRoleOwner}
}

// MerchantAPIKeyRoles returns the fixed catalog roles a merchant API key may
// hold, least privilege first.
func MerchantAPIKeyRoles() []string {
	return MerchantRoles()
}

// MerchantRolePermissions resolves a fixed merchant catalog role to its
// effective permission set (owner = the merchant:* owner grant). ok is false
// for anything outside the catalog.
func MerchantRolePermissions(role string) ([]string, bool) {
	role = strings.ToLower(strings.TrimSpace(role))
	if role == MerchantRoleOwner {
		return []string{string(MerchantType.OwnerGrant())}, true
	}
	for _, gt := range Groups() {
		if gt.Name != MerchantType {
			continue
		}
		for _, r := range gt.Roles {
			if r.Name == authkit.Role(role) {
				return append([]string(nil), r.Permissions...), true
			}
		}
	}
	return nil, false
}

// MerchantRoleCoveredBy reports whether grants cover EVERY permission the given
// catalog role confers (authkit glob semantics): the no-escalation rule for
// non-user principals minting keys — you may only hand out authority you hold.
// Unknown roles are never covered.
func MerchantRoleCoveredBy(role string, grants []string) bool {
	perms, ok := MerchantRolePermissions(role)
	if !ok {
		return false
	}
	for _, perm := range perms {
		covered := false
		for _, grant := range grants {
			if authkit.Perm(perm).Matches(authkit.Perm(grant)) {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

// MintMerchantAPIKey mints a key under the merchant's permission-group against
// role (fixed catalog only). actorUserID is the acting AuthKit user: AuthKit
// core enforces the credential-management capability + no-escalation against
// it. Empty actorUserID (non-user principals: admin API key, in-process host,
// delegated token) falls back to the genesis owner actor — the operator-CLI
// idiom — so the CALLER must have already enforced owner-level authority (the
// route gate + MerchantRoleCoveredBy). Returns the metadata and the one-time
// plaintext secret: it is never stored and never retrievable again.
func (c *ControlPlane) MintMerchantAPIKey(ctx context.Context, mid merchant.ID, name, role, actorUserID string) (MerchantAPIKey, string, error) {
	if c == nil || c.Core() == nil {
		return MerchantAPIKey{}, "", ErrNoControlPlane
	}
	role = strings.ToLower(strings.TrimSpace(role))
	if _, ok := MerchantRolePermissions(role); !ok {
		return MerchantAPIKey{}, "", ErrUnknownMerchantRole
	}
	ctx, slug, err := c.merchantGroupScopeForID(ctx, mid)
	if err != nil {
		return MerchantAPIKey{}, "", err
	}
	actorUserID = strings.TrimSpace(actorUserID)
	if actorUserID == "" {
		actorUserID, err = c.ensureMerchantAPIKeyActor(ctx, slug)
		if err != nil {
			return MerchantAPIKey{}, "", err
		}
	}
	key, secret, err := c.Core().MintAPIKeyWithOptions(ctx, MerchantGroup(slug), authkit.APIKeyMintOptions{
		Name:      strings.TrimSpace(name),
		Role:      authkit.Role(role),
		CreatedBy: actorUserID,
	})
	if err != nil {
		return MerchantAPIKey{}, "", err
	}
	return c.merchantAPIKeyView(key), secret, nil
}

// ListMerchantAPIKeys returns every key of the merchant's permission-group —
// live, expired, and revoked (status is part of the audit view). NEVER any
// secret material: AuthKit stores only the secret hash.
func (c *ControlPlane) ListMerchantAPIKeys(ctx context.Context, mid merchant.ID) ([]MerchantAPIKey, error) {
	if c == nil || c.Core() == nil {
		return nil, ErrNoControlPlane
	}
	ctx, slug, err := c.merchantGroupScopeForID(ctx, mid)
	if err != nil {
		return nil, err
	}
	keys, err := c.Core().ListAPIKeys(ctx, MerchantGroup(slug))
	if err != nil {
		return nil, err
	}
	out := make([]MerchantAPIKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, c.merchantAPIKeyView(k))
	}
	return out, nil
}

// RevokeMerchantAPIKey revokes the key by internal id, scoped to the merchant's
// group (a key can never be revoked across merchants). Returns false when no
// live key with that id exists in this merchant's group.
func (c *ControlPlane) RevokeMerchantAPIKey(ctx context.Context, mid merchant.ID, id string) (bool, error) {
	if c == nil || c.Core() == nil {
		return false, ErrNoControlPlane
	}
	ctx, slug, err := c.merchantGroupScopeForID(ctx, mid)
	if err != nil {
		return false, err
	}
	return c.Core().RevokeAPIKey(ctx, MerchantGroup(slug), strings.TrimSpace(id))
}

func (c *ControlPlane) merchantAPIKeyView(k authkit.APIKey) MerchantAPIKey {
	return MerchantAPIKey{
		ID:         k.ID,
		Name:       k.Name,
		Role:       string(k.Role),
		Prefix:     authkit.APIKeyMarker(c.TokenPrefix()) + k.KeyID,
		CreatedAt:  k.CreatedAt,
		LastUsedAt: k.LastUsedAt,
		ExpiresAt:  k.ExpiresAt,
		RevokedAt:  k.RevokedAt,
	}
}
