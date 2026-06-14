package controlplane

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	authcore "github.com/open-rails/authkit/core"

	"github.com/open-rails/openrails/pkg/merchant"
)

const (
	// ResourceKindMerchant scopes a service token to one OpenRails merchant. Resource
	// IDs are canonical openrails.merchants UUID strings.
	ResourceKindMerchant = "openrails.merchant"
	// ResourceKindMerchantSubject scopes a service token to one OpenRails payable
	// subject. Resource IDs are canonical openrails.merchant_subjects UUID strings.
	ResourceKindMerchantSubject = "openrails.merchant_subject"
)

// ResolvedServiceToken is the result of validating an OpenRails-issued service
// token against live AuthKit + merchant-directory state. It carries everything
// route authorization needs: the calling AuthKit tenant (org) that owns the
// merchant, the resolved OpenRails merchant, and the granted permission strings.
//
// #481: the merchant is NOT the caller's identity. The caller is an AuthKit
// principal (user OR remote_application); its authority over the merchant comes
// from a ROLE on the merchant's owner_tenant_id (the owning org), never from an
// identity-equation with the merchant.
type ResolvedServiceToken struct {
	// OwnerTenantID is the AuthKit tenant (org) uuid that owns/administers the
	// merchant — the caller's authority anchor (#481). Resolved by AuthKit's
	// opaque service-token lookup (the token's owning tenant) or the issuer ->
	// remote_application -> tenant mapping on the service-JWT path.
	OwnerTenantID string
	// OwnerTenantSlug is that owning tenant's slug (presentation/audit only).
	OwnerTenantSlug string
	// MerchantID is the OpenRails merchant (#480) the service token administers.
	MerchantID merchant.ID
	// MerchantSlug is the resolved merchant's slug.
	MerchantSlug string
	// Permissions is the service token's granted OpenRails permission catalog
	// entries (colon-form, e.g. "openrails:credits:write").
	Permissions []string
	// Resources is the service token's AuthKit-stored OpenRails resource scope.
	// OpenRails interprets only ResourceKindMerchant and ResourceKindMerchantSubject;
	// unknown kinds fail closed during service token resolution.
	Resources []authcore.ServiceTokenResource
}

// HasPermission reports whether the resolved service token grants perm. The broad
// PermAdmin grant satisfies any permission check.
func (r *ResolvedServiceToken) HasPermission(perm string) bool {
	perm = strings.TrimSpace(perm)
	for _, p := range r.Permissions {
		if p == perm || p == PermAdmin {
			return true
		}
	}
	return false
}

// AllowsMerchantSubject reports whether this service token may act for a payable
// subject inside its resolved merchant. Merchant-wide tokens carry only
// ResourceKindMerchant; subject-scoped tokens additionally carry the exact
// ResourceKindMerchantSubject id.
func (r *ResolvedServiceToken) AllowsMerchantSubject(subject uuid.UUID) bool {
	if r == nil || subject == uuid.Nil {
		return false
	}
	if !hasResource(r.Resources, ResourceKindMerchant, r.MerchantID.String()) {
		return false
	}
	if !hasAnyResourceKind(r.Resources, ResourceKindMerchantSubject) {
		return true
	}
	return hasResource(r.Resources, ResourceKindMerchantSubject, subject.String())
}

func MerchantResource(id merchant.ID) authcore.ServiceTokenResource {
	return authcore.ServiceTokenResource{Kind: ResourceKindMerchant, ID: id.String()}
}

func MerchantSubjectResource(id uuid.UUID) authcore.ServiceTokenResource {
	return authcore.ServiceTokenResource{Kind: ResourceKindMerchantSubject, ID: id.String()}
}

// MerchantScope resolves a merchant reference (slug or UUID) to the canonical
// OpenRails merchant id/slug and AuthKit service token merchant resource.
func (c *ControlPlane) MerchantScope(ctx context.Context, ref string) (merchant.ID, string, authcore.ServiceTokenResource, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		// No default merchant (#480): the merchant to scope to must be named explicitly.
		return merchant.ID{}, "", authcore.ServiceTokenResource{}, ErrServiceTokenMerchantUnresolved
	}
	if c == nil || c.pool == nil {
		return merchant.ID{}, "", authcore.ServiceTokenResource{}, errors.New("controlplane: pgx pool unavailable for merchant resolution")
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
			return merchant.ID{}, "", authcore.ServiceTokenResource{}, ErrServiceTokenMerchantUnresolved
		}
		return merchant.ID{}, "", authcore.ServiceTokenResource{}, err
	}
	if status != "active" {
		return merchant.ID{}, "", authcore.ServiceTokenResource{}, ErrServiceTokenMerchantUnresolved
	}
	mid, err := merchant.ParseID(idStr)
	if err != nil {
		return merchant.ID{}, "", authcore.ServiceTokenResource{}, err
	}
	return mid, slug, MerchantResource(mid), nil
}

// TokenPrefix returns the configured service token brand prefix (e.g. "openrails"), used to
// recognize and parse presented service tokens. Empty -> bare "st_" marker.
func (c *ControlPlane) TokenPrefix() string {
	if c == nil || c.cfg == nil || c.cfg.Auth == nil || c.cfg.Auth.ControlPlane == nil {
		return ""
	}
	return strings.TrimSpace(c.cfg.Auth.ControlPlane.TokenPrefix)
}

// LooksLikeServiceToken reports whether token carries this deployment's service token marker. Used
// by the service token middleware to route a Bearer credential to service token validation rather
// than JWT verification.
func (c *ControlPlane) LooksLikeServiceToken(token string) bool {
	return authcore.HasServiceTokenPrefix(c.TokenPrefix(), strings.TrimSpace(token))
}

// ResolveServiceToken validates a presented service token string end-to-end:
//
//   - parses the <prefix>_st_<key_id>_<secret> shape,
//   - resolves key id + secret hash via AuthKit core (owning tenant, permissions,
//     expiry, revocation) — returns authcore.ErrAccessTokenExpired /
//     ErrAccessTokenRevoked / ErrInvalidAccessToken on those conditions,
//   - resolves the merchant the caller administers by ROLE (#481): the merchant
//     whose owner_tenant_id is the caller's AuthKit tenant. Ownership (who
//     administers), not identity — the merchant is never "equal to" the org.
//
// A caller whose AuthKit tenant owns no active merchant yields
// ErrServiceTokenMerchantUnresolved so the caller can reject the request.
func (c *ControlPlane) ResolveServiceToken(ctx context.Context, token string) (*ResolvedServiceToken, error) {
	if c == nil || c.Core() == nil {
		return nil, ErrNoControlPlane
	}
	keyID, secret, ok := authcore.ParseServiceToken(c.TokenPrefix(), strings.TrimSpace(token))
	if !ok {
		return nil, authcore.ErrInvalidAccessToken
	}

	resolved, err := c.Core().ResolveServiceTokenWithResources(ctx, keyID, secret)
	if err != nil {
		return nil, err
	}

	// #481: authorize by the caller's role on the merchant's owner_tenant_id. The
	// owning AuthKit tenant administers its merchant(s); resolve the merchant via
	// that ownership link, never via an identity-equation.
	mid, mslug, err := c.merchantForOwnerTenant(ctx, resolved.TenantID)
	if err != nil {
		return nil, err
	}
	if err := validateServiceTokenResources(mid, resolved.Resources); err != nil {
		return nil, err
	}

	return &ResolvedServiceToken{
		OwnerTenantID:   resolved.TenantID,
		OwnerTenantSlug: resolved.TenantSlug,
		MerchantID:      mid,
		MerchantSlug:    mslug,
		Permissions:     resolved.Permissions,
		Resources:       resolved.Resources,
	}, nil
}

// ErrServiceTokenMerchantUnresolved indicates the caller's owning AuthKit tenant
// owns no active OpenRails merchant (no openrails.merchants row with that
// owner_tenant_id, or the merchant is suspended/deleted). Treated as an
// authorization failure: the caller cannot act on any merchant surface.
var ErrServiceTokenMerchantUnresolved = errors.New("controlplane: service token caller owns no active merchant")

// ErrServiceTokenScopeDenied indicates an otherwise valid service token lacks the required
// OpenRails merchant resource scope or carries unknown OpenRails resource kinds.
var ErrServiceTokenScopeDenied = errors.New("controlplane: service token resource scope denied")

func validateServiceTokenResources(mid merchant.ID, resources []authcore.ServiceTokenResource) error {
	if !hasResource(resources, ResourceKindMerchant, mid.String()) {
		return ErrServiceTokenScopeDenied
	}
	for _, r := range resources {
		switch strings.TrimSpace(r.Kind) {
		case ResourceKindMerchant, ResourceKindMerchantSubject:
		default:
			return ErrServiceTokenScopeDenied
		}
	}
	return nil
}

func hasResource(resources []authcore.ServiceTokenResource, kind, id string) bool {
	kind = strings.TrimSpace(kind)
	id = strings.TrimSpace(id)
	for _, r := range resources {
		if strings.TrimSpace(r.Kind) == kind && strings.TrimSpace(r.ID) == id {
			return true
		}
	}
	return false
}

func hasAnyResourceKind(resources []authcore.ServiceTokenResource, kind string) bool {
	kind = strings.TrimSpace(kind)
	for _, r := range resources {
		if strings.TrimSpace(r.Kind) == kind {
			return true
		}
	}
	return false
}

// merchantForOwnerTenant resolves the OpenRails merchant a caller administers, by
// ROLE on the owning AuthKit tenant (#481): the merchant whose owner_tenant_id is
// the caller's authenticated AuthKit tenant uuid. This is OWNERSHIP (who may
// administer), not IDENTITY — one tenant owns many merchants; the merchant is
// never "equal to" the org. Suspended/deleted merchants are rejected.
//
// (When a tenant owns several merchants, the request names which via MerchantScope
// + AuthorizeMerchant; this single-merchant resolution serves the common
// one-merchant-per-tenant service-token path and fails closed otherwise.)
func (c *ControlPlane) merchantForOwnerTenant(ctx context.Context, ownerTenantID string) (merchant.ID, string, error) {
	ownerTenantID = strings.TrimSpace(ownerTenantID)
	if c.pool == nil {
		return merchant.ID{}, "", errors.New("controlplane: pgx pool unavailable for merchant resolution")
	}
	if ownerTenantID == "" {
		return merchant.ID{}, "", ErrServiceTokenMerchantUnresolved
	}
	mid, slug, err := c.merchantDirectoryRow(ctx, `owner_tenant_id = $1`, ownerTenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return merchant.ID{}, "", ErrServiceTokenMerchantUnresolved
	}
	return mid, slug, err
}

// AuthorizeMerchant checks that the caller's owning AuthKit tenant owns the named
// merchant (#481 role-based authz): the merchant's owner_tenant_id must equal the
// caller's AuthKit tenant. Use it when a request NAMES a merchant (a tenant owning
// many merchants) rather than relying on the single-merchant resolution.
func (c *ControlPlane) AuthorizeMerchant(ctx context.Context, ownerTenantID string, mid merchant.ID) error {
	ownerTenantID = strings.TrimSpace(ownerTenantID)
	if c == nil || c.pool == nil {
		return errors.New("controlplane: pgx pool unavailable for merchant authorization")
	}
	if ownerTenantID == "" || mid.IsZero() {
		return ErrServiceTokenMerchantUnresolved
	}
	var owner *string
	var status string
	err := c.pool.QueryRow(ctx, `
		SELECT owner_tenant_id, status
		  FROM openrails.merchants
		 WHERE id = $1 AND deleted_at IS NULL
		 LIMIT 1
	`, mid.UUID()).Scan(&owner, &status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrServiceTokenMerchantUnresolved
		}
		return err
	}
	if status != "active" || owner == nil || strings.TrimSpace(*owner) != ownerTenantID {
		return ErrServiceTokenScopeDenied
	}
	return nil
}

// merchantDirectoryRow runs one openrails.merchants directory lookup with the
// given WHERE predicate (which must reference exactly one $1 argument). It returns
// pgx.ErrNoRows untouched so callers can decide whether a fallback applies; an
// inactive (suspended) merchant is rejected with ErrServiceTokenMerchantUnresolved
// and never falls through to another key.
func (c *ControlPlane) merchantDirectoryRow(ctx context.Context, where, arg string) (merchant.ID, string, error) {
	var (
		idStr  string
		slug   string
		status string
	)
	err := c.pool.QueryRow(ctx, `
		SELECT id::text, slug, status
		  FROM openrails.merchants
		 WHERE `+where+`
		   AND deleted_at IS NULL
		 LIMIT 1
	`, arg).Scan(&idStr, &slug, &status)
	if err != nil {
		return merchant.ID{}, "", err
	}
	if status != "active" {
		return merchant.ID{}, "", ErrServiceTokenMerchantUnresolved
	}

	mid, err := merchant.ParseID(idStr)
	if err != nil {
		return merchant.ID{}, "", err
	}
	return mid, slug, nil
}
