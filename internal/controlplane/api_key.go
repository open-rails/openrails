package controlplane

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"

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
// #567/#569: a merchant IS a top-level permission-group (`type=merchant`,
// `resourceRef=merchant-slug`). A programmatic credential is minted/nested under
// that merchant group; its merchant identity is THE GROUP it belongs to — never a
// resource scope — and its authority over the merchant is its assigned group role
// (resolved to `merchant:*` perms).
type ResolvedServiceCredential struct {
	// OwnerGroupID is the internal id of the merchant permission-group the
	// credential is nested under (#567) — the caller's authority anchor.
	OwnerGroupID string
	// OwnerGroupRef is that merchant group's resource ref (the merchant slug),
	// presentation/audit only.
	OwnerGroupRef string
	// MerchantID is the OpenRails merchant (#480) the credential administers.
	MerchantID merchant.ID
	// MerchantSlug is the resolved merchant's slug.
	MerchantSlug string
	// Permissions is the credential's granted OpenRails permission set
	// (`merchant:` permissions resolved from the group role).
	Permissions []string
}

// HasPermission reports whether the resolved credential grants perm. Glob-aware,
// identical to every other credential type (#565): a granted token covers perm
// via AuthKit's namespace-anchored glob semantics (`merchant:*` covers
// `merchant:catalog:update`; an exact grant still matches exactly).
func (r *ResolvedServiceCredential) HasPermission(perm string) bool {
	for _, grant := range r.Permissions {
		if authkit.PermMatches(grant, perm) {
			return true
		}
	}
	return false
}

// AllowsCustomer reports whether this credential may act for a payable subject
// inside its resolved merchant. #569 (hard cut): merchant credentials are
// merchant-wide — there is no "merchant key scoped to one customer" concept — so
// a resolved merchant credential may act for any payable subject within its
// merchant. (Customer-side delegation, bounded by budget windows, is a separate
// customer-permission-group credential, enforced OpenRails-side, not an authkit
// resource scope.)
func (r *ResolvedServiceCredential) AllowsCustomer(subject uuid.UUID) bool {
	return r != nil && !r.MerchantID.IsZero() && subject != uuid.Nil
}

// MerchantScope resolves an external name through AuthKit before selecting its
// immutable billing binding. A local display projection never owns the name.
func (c *ControlPlane) MerchantScope(ctx context.Context, ref string) (merchant.ID, string, error) {
	if c == nil || c.pool == nil {
		return merchant.ID{}, "", ErrServiceCredentialMerchantUnresolved
	}
	if strings.TrimSpace(ref) == "" {
		return merchant.ID{}, "", ErrServiceCredentialMerchantUnresolved
	}
	core := c.Core()
	if core == nil {
		return merchant.ID{}, "", ErrServiceCredentialMerchantUnresolved
	}
	gi, err := core.GroupInstanceForSlug(ctx, MerchantType, strings.ToLower(strings.TrimSpace(ref)))
	if errors.Is(err, authkit.ErrGroupNotFound) {
		return merchant.ID{}, "", ErrServiceCredentialMerchantUnresolved
	}
	if err != nil {
		return merchant.ID{}, "", err
	}
	mid, slug, err := c.merchantForGroupID(ctx, gi.ID)
	if err != nil {
		return merchant.ID{}, "", err
	}
	if current := strings.TrimSpace(gi.InstanceSlug); current != "" && current != slug {
		slug = current
	}
	return mid, slug, nil
}

// TokenPrefix returns the fixed shared-secret API-key brand prefix used to
// recognize and parse presented OpenRails API keys.
func (c *ControlPlane) TokenPrefix() string {
	return APIKeyPrefix
}

// LooksLikeAPIKey reports whether token carries this deployment's
// shared-secret API-key marker. Used by the service credential middleware to
// route a Bearer credential to API-key validation rather than JWT verification.
func (c *ControlPlane) LooksLikeAPIKey(token string) bool {
	return authkit.HasAPIKeyPrefix(c.TokenPrefix(), strings.TrimSpace(token))
}

// ResolveAPIKey validates a presented shared-secret API key end-to-end:
//
//   - parses the <prefix>_st_<key_id>_<secret> shape,
//   - resolves key id + secret hash via AuthKit core (controlling permission
//     group, role-resolved permissions, expiry, revocation) — returns
//     authkit.ErrAccessTokenExpired / ErrAccessTokenRevoked /
//     ErrInvalidAccessToken on those conditions,
//   - resolves the merchant the credential administers from the permission GROUP
//     the key was minted under (#567/#569: a merchant IS its group). A key whose
//     group backs no active merchant yields ErrServiceCredentialMerchantUnresolved.
//   - rejects a resolved merchant that differs from the request Host merchant.
func (c *ControlPlane) ResolveAPIKey(ctx context.Context, token string) (*ResolvedServiceCredential, error) {
	if c == nil || c.Core() == nil {
		return nil, ErrNoControlPlane
	}
	keyID, secret, ok := authkit.ParseAPIKey(c.TokenPrefix(), strings.TrimSpace(token))
	if !ok {
		return nil, authkit.ErrInvalidAccessToken
	}

	groupID, permissions, err := c.Core().ResolveAPIKey(ctx, keyID, secret)
	if err != nil {
		return nil, err
	}

	// #569 (hard cut): the merchant identity is the permission group the key was
	// minted under — resolved with the same group-based resolver the service-JWT
	// path uses — never a resource scope. Fails closed when the group backs no
	// active merchant (stale/deleted), so cross-merchant or orphaned keys are denied.
	mid, mslug, err := c.merchantForGroupID(ctx, groupID)
	if err != nil {
		return nil, err
	}

	if hostMID, ok := merchant.HostMerchant(ctx); ok && hostMID != mid {
		return nil, ErrServiceCredentialHostMismatch
	}

	return &ResolvedServiceCredential{
		OwnerGroupID:  groupID,
		OwnerGroupRef: mslug,
		MerchantID:    mid,
		MerchantSlug:  mslug,
		Permissions:   permissions,
	}, nil
}

// ErrServiceCredentialHostMismatch rejects a valid API key presented against
// another merchant's canonical host.
var ErrServiceCredentialHostMismatch = errors.New("controlplane: API key merchant does not match request host")

// ErrServiceCredentialMerchantUnresolved indicates the caller's permission group
// backs no active OpenRails merchant (no openrails.merchants row with that
// permission_group_id, or the merchant is deleted). Treated as an
// authorization failure: the caller cannot act on any merchant surface.
var ErrServiceCredentialMerchantUnresolved = errors.New("controlplane: API key caller owns no active merchant")

// ErrServiceCredentialScopeDenied indicates an otherwise valid service credential
// lacks the required OpenRails merchant authority.
var ErrServiceCredentialScopeDenied = errors.New("controlplane: API key resource scope denied")

// merchantForGroupID resolves the OpenRails merchant a caller administers from
// its authenticated authkit permission-group id: the merchant whose
// permission_group_id equals it (#567 — a merchant IS its own group; was the
// the old owner-link lookup). With merchant.slug == group slug (#548), the group
// is effectively the merchant's authkit identity. Suspended/deleted merchants
// are rejected.
func (c *ControlPlane) merchantForGroupID(ctx context.Context, groupID string) (merchant.ID, string, error) {
	groupID = strings.TrimSpace(groupID)
	if c.pool == nil {
		return merchant.ID{}, "", errors.New("controlplane: pgx pool unavailable for merchant resolution")
	}
	if groupID == "" {
		return merchant.ID{}, "", ErrServiceCredentialMerchantUnresolved
	}
	mid, slug, err := c.merchantDirectoryRow(ctx, `permission_group_id = $1`, groupID)
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
	var owner *string
	var status string
	err := c.pool.QueryRow(ctx, `
		SELECT permission_group_id, status
		  FROM openrails.merchants
		 WHERE id = $1 AND deleted_at IS NULL
		 LIMIT 1
	`, mid.UUID()).Scan(&owner, &status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrServiceCredentialMerchantUnresolved
		}
		return err
	}
	if status != "active" || owner == nil || strings.TrimSpace(*owner) != groupID {
		return ErrServiceCredentialScopeDenied
	}
	return nil
}

// merchantDirectoryRow runs one openrails.merchants directory lookup with the
// given WHERE predicate (which must reference exactly one $1 argument). It
// returns pgx.ErrNoRows untouched so callers can decide whether a fallback
// applies. If the predicate matches multiple active merchants, the caller must
// name a merchant explicitly and authorize it with AuthorizeMerchant.
func (c *ControlPlane) merchantDirectoryRow(ctx context.Context, where, arg string) (merchant.ID, string, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT id::text, slug, status
		  FROM openrails.merchants
		 WHERE `+where+`
		   AND deleted_at IS NULL
		 LIMIT 2
	`, arg)
	if err != nil {
		return merchant.ID{}, "", err
	}
	defer rows.Close()

	type row struct {
		idStr  string
		slug   string
		status string
	}
	var matches []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.idStr, &r.slug, &r.status); err != nil {
			return merchant.ID{}, "", err
		}
		matches = append(matches, r)
	}
	if err := rows.Err(); err != nil {
		return merchant.ID{}, "", err
	}
	if len(matches) == 0 {
		return merchant.ID{}, "", pgx.ErrNoRows
	}
	if len(matches) > 1 {
		return merchant.ID{}, "", ErrServiceCredentialMerchantUnresolved
	}
	idStr, slug, status := matches[0].idStr, matches[0].slug, matches[0].status
	if status != "active" {
		return merchant.ID{}, "", ErrServiceCredentialMerchantUnresolved
	}

	mid, err := merchant.ParseID(idStr)
	if err != nil {
		return merchant.ID{}, "", err
	}
	return mid, slug, nil
}
