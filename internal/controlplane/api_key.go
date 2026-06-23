package controlplane

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/authbase"
	authcore "github.com/open-rails/authkit/core"

	"github.com/open-rails/openrails/pkg/merchant"
)

const (
	// APIKeyPrefix is the fixed OpenRails shared-secret API-key marker.
	APIKeyPrefix = "openrails"

	// ResourceKindMerchant scopes a service credential to one OpenRails merchant.
	// Resource IDs are canonical openrails.merchants UUID strings.
	ResourceKindMerchant = "openrails.merchant"
	// ResourceKindCustomer scopes a service credential to one OpenRails payable
	// subject. Resource IDs are canonical openrails.customers UUID strings.
	ResourceKindCustomer = "openrails.customer"
)

// ResolvedServiceCredential is the common authorization result for
// merchant-scoped programmatic credentials: OpenRails-issued shared-secret API
// keys, first-party service JWTs, and AuthKit remote-application self tokens.
// It carries everything route authorization needs: the resolved OpenRails
// merchant and the granted permission strings.
//
// #567: a merchant IS a top-level permission-group (`type=merchant`,
// `resourceRef=merchant-slug`). A programmatic credential is minted/nested under
// that merchant group; its authority over the merchant is its assigned group
// role (resolved to `merchant:*` perms), never an org identity-equation.
type ResolvedServiceCredential struct {
	// OwnerGroupID is the internal id of the merchant permission-group the
	// credential is nested under (#567) — the caller's authority anchor. For API
	// keys this is the group the key resolves to; for JWT paths it is the merchant
	// group resolved from the issuer's controlling group.
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
	// Resources is the credential's OpenRails resource scope.
	// OpenRails interprets only ResourceKindMerchant and ResourceKindCustomer;
	// unknown kinds fail closed during credential resolution.
	Resources []authcore.APIKeyResource
}

// HasPermission reports whether the resolved credential grants perm. Glob-aware,
// identical to every other credential type (#565): a granted token covers perm
// via AuthKit's namespace-anchored glob semantics (`merchant:*` covers
// `merchant:catalog:update`; an exact grant still matches exactly).
func (r *ResolvedServiceCredential) HasPermission(perm string) bool {
	for _, grant := range r.Permissions {
		if authbase.PermissionTokenCovers(grant, perm) {
			return true
		}
	}
	return false
}

// AllowsCustomer reports whether this credential may act for a payable subject
// inside its resolved merchant. Merchant-wide credentials carry only
// ResourceKindMerchant; subject-scoped credentials additionally carry the exact
// ResourceKindCustomer id.
func (r *ResolvedServiceCredential) AllowsCustomer(subject uuid.UUID) bool {
	if r == nil || subject == uuid.Nil {
		return false
	}
	if !hasResource(r.Resources, ResourceKindMerchant, r.MerchantID.String()) {
		return false
	}
	if !hasAnyResourceKind(r.Resources, ResourceKindCustomer) {
		return true
	}
	return hasResource(r.Resources, ResourceKindCustomer, subject.String())
}

func MerchantResource(id merchant.ID) authcore.APIKeyResource {
	return authcore.APIKeyResource{Kind: ResourceKindMerchant, ID: id.String()}
}

func CustomerResource(id uuid.UUID) authcore.APIKeyResource {
	return authcore.APIKeyResource{Kind: ResourceKindCustomer, ID: id.String()}
}

// MerchantScope resolves a merchant reference (slug or UUID) to the canonical
// OpenRails merchant id/slug and AuthKit resource scope for that merchant.
func (c *ControlPlane) MerchantScope(ctx context.Context, ref string) (merchant.ID, string, authcore.APIKeyResource, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		// No default merchant (#480): the merchant to scope to must be named explicitly.
		return merchant.ID{}, "", authcore.APIKeyResource{}, ErrServiceCredentialMerchantUnresolved
	}
	if c == nil || c.pool == nil {
		return merchant.ID{}, "", authcore.APIKeyResource{}, errors.New("controlplane: pgx pool unavailable for merchant resolution")
	}
	var (
		idStr  string
		slug   string
		status string
	)
	err := c.pool.QueryRow(ctx, `
		SELECT id::text, slug, status
		  FROM openrails.merchants
		 WHERE (id::text = $1 OR lower(slug) = lower($1))
		   AND deleted_at IS NULL
		 LIMIT 1
	`, ref).Scan(&idStr, &slug, &status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return merchant.ID{}, "", authcore.APIKeyResource{}, ErrServiceCredentialMerchantUnresolved
		}
		return merchant.ID{}, "", authcore.APIKeyResource{}, err
	}
	if status != "active" {
		return merchant.ID{}, "", authcore.APIKeyResource{}, ErrServiceCredentialMerchantUnresolved
	}
	mid, err := merchant.ParseID(idStr)
	if err != nil {
		return merchant.ID{}, "", authcore.APIKeyResource{}, err
	}
	return mid, slug, MerchantResource(mid), nil
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
	return authcore.HasAPIKeyPrefix(c.TokenPrefix(), strings.TrimSpace(token))
}

// ResolveAPIKey validates a presented shared-secret API key end-to-end:
//
//   - parses the <prefix>_st_<key_id>_<secret> shape,
//   - resolves key id + secret hash via AuthKit core (controlling permission
//     group, role-resolved permissions, expiry, revocation) — returns
//     authcore.ErrAccessTokenExpired / ErrAccessTokenRevoked /
//     ErrInvalidAccessToken on those conditions,
//   - resolves the merchant the credential administers from its OpenRails
//     resource scope (#567): the key is minted under the merchant permission-group
//     carrying a ResourceKindMerchant resource = the merchant id. The merchant is
//     read from that resource, never from an org identity-equation.
//
// A key carrying no merchant resource scope yields
// ErrServiceCredentialMerchantUnresolved so the caller can reject the request.
func (c *ControlPlane) ResolveAPIKey(ctx context.Context, token string) (*ResolvedServiceCredential, error) {
	if c == nil || c.Core() == nil {
		return nil, ErrNoControlPlane
	}
	keyID, secret, ok := authcore.ParseAPIKey(c.TokenPrefix(), strings.TrimSpace(token))
	if !ok {
		return nil, authcore.ErrInvalidAccessToken
	}

	resolved, err := c.Core().ResolveAPIKeyWithResources(ctx, keyID, secret)
	if err != nil {
		return nil, err
	}

	// #567: the merchant is the key's ResourceKindMerchant scope. Resolve and
	// validate it against the live directory (active merchant) so a stale/deleted
	// merchant fails closed; this also rejects any cross-merchant or unknown-kind
	// resource the key might carry.
	mid, mslug, err := c.merchantFromResources(ctx, resolved.Resources)
	if err != nil {
		return nil, err
	}
	if err := validateAPIKeyResources(mid, resolved.Resources); err != nil {
		return nil, err
	}

	return &ResolvedServiceCredential{
		OwnerGroupID:  resolved.PermissionGroupID,
		OwnerGroupRef: mslug,
		MerchantID:    mid,
		MerchantSlug:  mslug,
		Permissions:   resolved.Permissions,
		Resources:     resolved.Resources,
	}, nil
}

// merchantFromResources resolves the single ResourceKindMerchant id carried by a
// credential's resource scope to the live OpenRails merchant directory row
// (active only). Fails closed when no merchant resource is present or the
// merchant is missing/inactive.
func (c *ControlPlane) merchantFromResources(ctx context.Context, resources []authcore.APIKeyResource) (merchant.ID, string, error) {
	var ref string
	for _, r := range resources {
		if strings.TrimSpace(r.Kind) == ResourceKindMerchant {
			ref = strings.TrimSpace(r.ID)
			break
		}
	}
	if ref == "" {
		return merchant.ID{}, "", ErrServiceCredentialMerchantUnresolved
	}
	mid, mslug, _, err := c.MerchantScope(ctx, ref)
	if errors.Is(err, ErrServiceCredentialMerchantUnresolved) {
		return merchant.ID{}, "", ErrServiceCredentialMerchantUnresolved
	}
	if err != nil {
		return merchant.ID{}, "", err
	}
	return mid, mslug, nil
}

// ErrServiceCredentialMerchantUnresolved indicates the caller's permission group
// backs no active OpenRails merchant (no openrails.merchants row with that
// permission_group_id, or the merchant is deleted). Treated as an
// authorization failure: the caller cannot act on any merchant surface.
var ErrServiceCredentialMerchantUnresolved = errors.New("controlplane: API key caller owns no active merchant")

// ErrServiceCredentialScopeDenied indicates an otherwise valid service credential
// lacks the required OpenRails merchant resource scope or carries unknown
// OpenRails resource kinds.
var ErrServiceCredentialScopeDenied = errors.New("controlplane: API key resource scope denied")

func validateAPIKeyResources(mid merchant.ID, resources []authcore.APIKeyResource) error {
	if !hasResource(resources, ResourceKindMerchant, mid.String()) {
		return ErrServiceCredentialScopeDenied
	}
	for _, r := range resources {
		switch strings.TrimSpace(r.Kind) {
		case ResourceKindMerchant, ResourceKindCustomer:
		default:
			return ErrServiceCredentialScopeDenied
		}
	}
	return nil
}

func hasResource(resources []authcore.APIKeyResource, kind, id string) bool {
	kind = strings.TrimSpace(kind)
	id = strings.TrimSpace(id)
	for _, r := range resources {
		if strings.TrimSpace(r.Kind) == kind && strings.TrimSpace(r.ID) == id {
			return true
		}
	}
	return false
}

func hasAnyResourceKind(resources []authcore.APIKeyResource, kind string) bool {
	kind = strings.TrimSpace(kind)
	for _, r := range resources {
		if strings.TrimSpace(r.Kind) == kind {
			return true
		}
	}
	return false
}

// merchantForGroupID resolves the OpenRails merchant a caller administers from
// its authenticated authkit permission-group id: the merchant whose
// permission_group_id equals it (#567 — a merchant IS its own group; was the
// #527 owner_org 1:1 lookup). With merchant.slug == group slug (#548), the group
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
// and the merchant must be active (#567; was the #527 owner_org check).
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
