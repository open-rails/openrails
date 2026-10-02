package controlplane

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/auth/policy"
	"github.com/open-rails/openrails/internal/credential"
	"github.com/open-rails/openrails/internal/db/gen"

	"github.com/open-rails/openrails/pkg/merchant"
)

const (
	// APIKeyPrefix is the fixed OpenRails shared-secret API-key marker.
	APIKeyPrefix = "openrails"
)

// ResolvedServiceCredential is the common authorization result for
// merchant-scoped programmatic credentials: OpenRails-issued shared-secret API
// keys, first-party service JWTs, and AuthKit remote-application self tokens.
// It carries everything route authorization needs: the resolved OpenRails
// merchant and the granted permission strings.
//
// #567/#569: a merchant IS a merchant permission group. A programmatic
// credential belongs to that group; its merchant identity is THE GROUP, never a
// resource scope, and its authority is its role there.
type ResolvedServiceCredential = credential.ResolvedServiceCredential

// MerchantScope resolves a current or former merchant name to its bound
// merchant and current name.
func (c *ControlPlane) MerchantScope(ctx context.Context, ref string) (merchant.ID, string, error) {
	if c == nil || c.pool == nil || c.Core() == nil || strings.TrimSpace(ref) == "" {
		return merchant.ID{}, "", ErrServiceCredentialMerchantUnresolved
	}
	groupID, err := c.merchantGroupByName(ctx, ref)
	if errors.Is(err, policy.ErrMerchantUnresolved) {
		return merchant.ID{}, "", ErrServiceCredentialMerchantUnresolved
	}
	if err != nil {
		return merchant.ID{}, "", err
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
// that group (#567/#569), never a resource scope. A key whose group backs no
// active merchant is ErrServiceCredentialMerchantUnresolved, and one presented
// against another merchant's Host is ErrServiceCredentialHostMismatch.
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
// backs no active OpenRails merchant (no openrails.merchants row with that
// permission_group_id, or the merchant is deleted). Treated as an
// authorization failure: the caller cannot act on any merchant surface.
var ErrServiceCredentialMerchantUnresolved = credential.ErrServiceCredentialMerchantUnresolved

// ErrServiceCredentialScopeDenied indicates an otherwise valid service credential
// lacks the required OpenRails merchant authority.
var ErrServiceCredentialScopeDenied = credential.ErrServiceCredentialScopeDenied

// merchantForGroupID resolves the OpenRails merchant bound to an AuthKit group
// (#567: a merchant IS its own group). Suspended and deleted merchants are
// rejected.
func (c *ControlPlane) merchantForGroupID(ctx context.Context, groupID string) (merchant.ID, string, error) {
	groupID = strings.TrimSpace(groupID)
	if c.pool == nil {
		return merchant.ID{}, "", errors.New("controlplane: pgx pool unavailable for merchant resolution")
	}
	if groupID == "" {
		return merchant.ID{}, "", ErrServiceCredentialMerchantUnresolved
	}
	mid, slug, err := c.merchantDirectoryRow(gen.New(c.pool).ListLiveMerchantsByGroupID(ctx, groupID))
	if errors.Is(err, pgx.ErrNoRows) {
		return merchant.ID{}, "", ErrServiceCredentialMerchantUnresolved
	}
	return mid, slug, err
}

// AuthorizeMerchant checks that the caller's permission group backs the named
// merchant: the merchant's permission_group_id must equal the caller's group id,
// and the merchant must be active (#567).
// merchantForGroupID resolves the group's merchant; AuthorizeMerchant is the
// explicit fail-closed gate for any path that NAMES a merchant id directly.
func (c *ControlPlane) AuthorizeMerchant(ctx context.Context, groupID string, mid merchant.ID) error {
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

// merchantDirectoryRow resolves one live directory lookup (LIMIT 2). It
// returns pgx.ErrNoRows untouched so callers can decide whether a fallback
// applies. If the lookup matches multiple active merchants, the caller must
// name a merchant explicitly and authorize it with AuthorizeMerchant.
func (c *ControlPlane) merchantDirectoryRow(matches []gen.OpenrailsMerchant, err error) (merchant.ID, string, error) {
	if err != nil {
		return merchant.ID{}, "", err
	}
	if len(matches) == 0 {
		return merchant.ID{}, "", pgx.ErrNoRows
	}
	if len(matches) > 1 {
		return merchant.ID{}, "", ErrServiceCredentialMerchantUnresolved
	}
	if matches[0].Status != "active" {
		return merchant.ID{}, "", ErrServiceCredentialMerchantUnresolved
	}
	return merchant.ID(matches[0].ID), matches[0].Slug, nil
}
