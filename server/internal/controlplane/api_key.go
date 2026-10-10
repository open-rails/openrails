package controlplane

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/credential"
	"github.com/open-rails/openrails/internal/db/gen"

	"github.com/open-rails/openrails/internal/merchant"
)

const (
	// APIKeyPrefix is the fixed OpenRails shared-secret API-key marker.
	APIKeyPrefix = "openrails"
)

// ResolvedServiceCredential is a resolved merchant API key: the merchant bound
// to the key's permission group (never a resource scope) and the permissions
// its role there grants.
type ResolvedServiceCredential = credential.ResolvedServiceCredential

// MerchantScope resolves a current or former merchant name to its bound
// merchant and current name.
func (c *ControlPlane) MerchantScope(ctx context.Context, ref string) (billing.MerchantID, string, error) {
	if c == nil || c.pool == nil || c.Core() == nil || strings.TrimSpace(ref) == "" {
		return billing.MerchantID{}, "", ErrServiceCredentialMerchantUnresolved
	}
	groupID, err := c.merchantGroupByName(ctx, ref)
	if errors.Is(err, billing.ErrMerchantUnresolved) {
		return billing.MerchantID{}, "", ErrServiceCredentialMerchantUnresolved
	}
	if err != nil {
		return billing.MerchantID{}, "", err
	}
	return c.merchantForGroupID(ctx, groupID)
}

// TokenPrefix returns the fixed shared-secret API-key brand prefix used to
// recognize and parse presented OpenRails API keys.
func (c *ControlPlane) TokenPrefix() string {
	return APIKeyPrefix
}

// LooksLikeAPIKey reports whether token carries this deployment's API-key
// marker, "<prefix>_st_". The middleware routes such a Bearer credential to
// API-key validation rather than JWT verification.
func (c *ControlPlane) LooksLikeAPIKey(token string) bool {
	return strings.HasPrefix(strings.TrimSpace(token), c.TokenPrefix()+"_st_")
}

// ResolveAPIKey validates a presented API key: AuthKit resolves it live (its
// group, its role's permissions, expiry and revocation: iam.ErrAPIKeyInvalid,
// ErrAPIKeyExpired, ErrAPIKeyRevoked), and the merchant is the one bound to
// that group. A key whose group backs no active merchant is
// ErrServiceCredentialMerchantUnresolved, and one presented against another
// merchant's Host is ErrServiceCredentialHostMismatch.
func (c *ControlPlane) ResolveAPIKey(ctx context.Context, token string) (*ResolvedServiceCredential, error) {
	if c == nil || c.Core() == nil {
		return nil, ErrNoControlPlane
	}
	key, err := c.client.ResolveAPIKey(ctx, strings.TrimSpace(token))
	if err != nil {
		return nil, err
	}
	mid, slug, err := c.merchantForGroupID(ctx, key.Group.ID)
	if err != nil {
		return nil, err
	}
	if hostMID, ok := merchant.HostMerchant(ctx); ok && hostMID != mid {
		return nil, ErrServiceCredentialHostMismatch
	}
	permissions := make([]string, len(key.Permissions))
	for i, perm := range key.Permissions {
		permissions[i] = perm.String()
	}
	return &ResolvedServiceCredential{
		KeyID:         key.ID,
		Issuer:        key.Issuer,
		OwnerGroupID:  key.Group.ID,
		OwnerGroupRef: slug,
		MerchantID:    mid,
		MerchantSlug:  slug,
		Permissions:   permissions,
	}, nil
}

// ErrServiceCredentialHostMismatch rejects a valid API key presented against
// another merchant's canonical host.
var ErrServiceCredentialHostMismatch = credential.ErrServiceCredentialHostMismatch

// ErrServiceCredentialMerchantUnresolved indicates the caller's permission group
// backs no active OpenRails merchant (no billing.merchants row with that
// permission_group_id, or the merchant is deleted). Treated as an
// authorization failure: the caller cannot act on any merchant surface.
var ErrServiceCredentialMerchantUnresolved = credential.ErrServiceCredentialMerchantUnresolved

// ErrServiceCredentialScopeDenied indicates an otherwise valid service credential
// lacks the required OpenRails merchant authority.
var ErrServiceCredentialScopeDenied = credential.ErrServiceCredentialScopeDenied

// merchantForGroupID resolves the active merchant bound to an AuthKit group (a
// merchant is its own group).
func (c *ControlPlane) merchantForGroupID(ctx context.Context, groupID string) (billing.MerchantID, string, error) {
	groupID = strings.TrimSpace(groupID)
	if c.pool == nil {
		return billing.MerchantID{}, "", errors.New("controlplane: pgx pool unavailable for merchant resolution")
	}
	if groupID == "" {
		return billing.MerchantID{}, "", ErrServiceCredentialMerchantUnresolved
	}
	mid, slug, err := c.merchantDirectoryRow(gen.New(c.pool).ListLiveMerchantsByGroupID(ctx, groupID))
	if errors.Is(err, pgx.ErrNoRows) {
		return billing.MerchantID{}, "", ErrServiceCredentialMerchantUnresolved
	}
	return mid, slug, err
}

// AuthorizeMerchant is the fail-closed gate for a path that names a merchant
// id: the merchant must be active and bound to the caller's permission group.
func (c *ControlPlane) AuthorizeMerchant(ctx context.Context, groupID string, mid billing.MerchantID) error {
	groupID = strings.TrimSpace(groupID)
	if c == nil || c.pool == nil {
		return errors.New("controlplane: pgx pool unavailable for merchant authorization")
	}
	if groupID == "" || mid.IsZero() {
		return ErrServiceCredentialMerchantUnresolved
	}
	row, err := gen.New(c.pool).GetMerchantDirectoryByID(ctx, mid.UUID())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrServiceCredentialMerchantUnresolved
		}
		return err
	}
	if row.Status != "active" || row.PermissionGroupID == nil || strings.TrimSpace(*row.PermissionGroupID) != groupID {
		return ErrServiceCredentialScopeDenied
	}
	return nil
}

// merchantDirectoryRow is the one active merchant a directory lookup matched:
// pgx.ErrNoRows for none (the caller picks the error), and an ambiguous or
// inactive match is ErrServiceCredentialMerchantUnresolved.
func (c *ControlPlane) merchantDirectoryRow(matches []gen.BillingMerchant, err error) (billing.MerchantID, string, error) {
	if err != nil {
		return billing.MerchantID{}, "", err
	}
	if len(matches) == 0 {
		return billing.MerchantID{}, "", pgx.ErrNoRows
	}
	if len(matches) > 1 {
		return billing.MerchantID{}, "", ErrServiceCredentialMerchantUnresolved
	}
	if matches[0].Status != "active" {
		return billing.MerchantID{}, "", ErrServiceCredentialMerchantUnresolved
	}
	return billing.MerchantID(matches[0].ID), matches[0].Slug, nil
}
