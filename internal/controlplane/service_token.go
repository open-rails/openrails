package controlplane

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	authcore "github.com/open-rails/authkit/core"

	"github.com/open-rails/openrails/pkg/tenant"
)

const (
	// ResourceKindTenant scopes a service token to one OpenRails tenant. Resource IDs are
	// canonical OpenRails tenant UUID strings.
	ResourceKindTenant = "openrails.tenant"
	// ResourceKindTenantSubject scopes a service token to one OpenRails payable subject.
	// Resource IDs are canonical billing.tenant_subjects UUID strings.
	ResourceKindTenantSubject = "openrails.tenant_subject"
)

// ResolvedServiceToken is the result of validating an OpenRails-issued tenant service token against
// live AuthKit + tenant-directory state (issue #222). It carries everything route
// authorization needs: the owning AuthKit tenant, the resolved OpenRails tenant, and
// the service token's granted OpenRails permission strings.
type ResolvedServiceToken struct {
	// AuthKitTenantID is the immutable AuthKit tenant uuid that owns the
	// service token (claim/source `tenant_id`). It is the canonical
	// cross-service identifier; empty for legacy credentials minted before
	// AuthKit emitted it, and on the issuer-pinned service-JWT path.
	AuthKitTenantID string
	// AuthKitTenantSlug is the AuthKit tenant slug that owns the service
	// token. Slugs are MUTABLE — presentation/audit only.
	AuthKitTenantSlug string
	// TenantID is the OpenRails tenant (#223) the service token administers.
	TenantID tenant.ID
	// TenantSlug is the resolved tenant's slug.
	TenantSlug string
	// Permissions is the service token's granted OpenRails permission catalog entries
	// (colon-form, e.g. "openrails:credits:write").
	Permissions []string
	// Resources is the service token's AuthKit-stored OpenRails resource scope. OpenRails
	// interprets only ResourceKindTenant and ResourceKindTenantSubject; unknown kinds
	// fail closed during service token resolution.
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

// AllowsTenantSubject reports whether this service token may act for a payable subject
// inside its resolved tenant. Tenant-wide tokens carry only ResourceKindTenant;
// subject-scoped tokens additionally carry the exact ResourceKindTenantSubject id.
func (r *ResolvedServiceToken) AllowsTenantSubject(subject uuid.UUID) bool {
	if r == nil || subject == uuid.Nil {
		return false
	}
	if !hasResource(r.Resources, ResourceKindTenant, r.TenantID.String()) {
		return false
	}
	if !hasAnyResourceKind(r.Resources, ResourceKindTenantSubject) {
		return true
	}
	return hasResource(r.Resources, ResourceKindTenantSubject, subject.String())
}

func TenantResource(id tenant.ID) authcore.ServiceTokenResource {
	return authcore.ServiceTokenResource{Kind: ResourceKindTenant, ID: id.String()}
}

func TenantSubjectResource(id uuid.UUID) authcore.ServiceTokenResource {
	return authcore.ServiceTokenResource{Kind: ResourceKindTenantSubject, ID: id.String()}
}

// TenantScope resolves a tenant reference (slug or UUID) to the canonical
// OpenRails tenant id/slug and AuthKit service token tenant resource.
func (c *ControlPlane) TenantScope(ctx context.Context, ref string) (tenant.ID, string, authcore.ServiceTokenResource, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		ref = tenant.DefaultSlug
	}
	if c == nil || c.pool == nil {
		return tenant.ID{}, "", authcore.ServiceTokenResource{}, errors.New("controlplane: pgx pool unavailable for tenant resolution")
	}
	var (
		idStr  string
		slug   string
		status string
	)
	err := c.pool.QueryRow(ctx, `
		SELECT id::text, slug, status
		  FROM billing.tenants
		 WHERE (id::text = $1 OR lower(slug) = lower($1))
		   AND deleted_at IS NULL
		 LIMIT 1
	`, ref).Scan(&idStr, &slug, &status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return tenant.ID{}, "", authcore.ServiceTokenResource{}, ErrServiceTokenTenantUnresolved
		}
		return tenant.ID{}, "", authcore.ServiceTokenResource{}, err
	}
	if status != "active" {
		return tenant.ID{}, "", authcore.ServiceTokenResource{}, ErrServiceTokenTenantUnresolved
	}
	tid, err := tenant.ParseID(idStr)
	if err != nil {
		return tenant.ID{}, "", authcore.ServiceTokenResource{}, err
	}
	return tid, slug, TenantResource(tid), nil
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
//     expiry, revocation, tenant-deleted) — returns authcore.ErrAccessTokenExpired /
//     ErrAccessTokenRevoked / ErrInvalidAccessToken on those conditions,
//   - maps the owning AuthKit tenant -> OpenRails tenant (#223) via the
//     billing.tenants directory, preferring the IMMUTABLE AuthKit tenant uuid
//     (authkit_tenant_id) and falling back to the mutable slug
//     (authkit_tenant_slug) only for legacy state (see tenantForAuthKitTenant).
//
// A cross-tenant or unknown service-token tenant (no tenant directory row) yields
// ErrServiceTokenTenantUnresolved so the caller can reject the request.
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

	tid, tslug, err := c.tenantForAuthKitTenant(ctx, resolved.TenantID, resolved.TenantSlug)
	if err != nil {
		return nil, err
	}
	if err := validateServiceTokenResources(tid, resolved.Resources); err != nil {
		return nil, err
	}

	return &ResolvedServiceToken{
		AuthKitTenantID:   resolved.TenantID,
		AuthKitTenantSlug: resolved.TenantSlug,
		TenantID:          tid,
		TenantSlug:        tslug,
		Permissions:       resolved.Permissions,
		Resources:         resolved.Resources,
	}, nil
}

// ErrServiceTokenTenantUnresolved indicates the service token's owning AuthKit tenant is not mapped to
// any OpenRails tenant (no billing.tenants row, or the tenant is suspended/
// deleted). Treated as an authorization failure: the service token cannot act on any
// tenant surface.
var ErrServiceTokenTenantUnresolved = errors.New("controlplane: service token owning tenant maps to no active tenant")

// ErrServiceTokenScopeDenied indicates an otherwise valid service token lacks the required
// OpenRails tenant resource scope or carries unknown OpenRails resource kinds.
var ErrServiceTokenScopeDenied = errors.New("controlplane: service token resource scope denied")

func validateServiceTokenResources(tid tenant.ID, resources []authcore.ServiceTokenResource) error {
	if !hasResource(resources, ResourceKindTenant, tid.String()) {
		return ErrServiceTokenScopeDenied
	}
	for _, r := range resources {
		switch strings.TrimSpace(r.Kind) {
		case ResourceKindTenant, ResourceKindTenantSubject:
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

// tenantForAuthKitTenant maps a credential's owning AuthKit tenant to its
// OpenRails tenant via the billing.tenants directory. The operator tenant
// administers the default tenant; future AuthKit tenants map to their own
// tenant rows. Suspended/deleted tenants are rejected.
//
// Resolution prefers the IMMUTABLE AuthKit tenant uuid (authkit_tenant_id);
// the MUTABLE slug (authkit_tenant_slug) is consulted only when:
//
//   - the credential carries no uuid (minted by a pre-`tenant_id` AuthKit —
//     such tokens circulate for months), or
//   - the uuid matched no directory row AND the slug row is itself unlinked
//     (authkit_tenant_id IS NULL, i.e. written before the uuid dual-write /
//     backfill). A row already linked to a DIFFERENT AuthKit tenant must
//     never be reachable through its mutable slug — that is exactly the
//     slug-reassignment confusion this resolution order exists to prevent.
func (c *ControlPlane) tenantForAuthKitTenant(ctx context.Context, authKitTenantID, authKitTenantSlug string) (tenant.ID, string, error) {
	authKitTenantID = strings.TrimSpace(authKitTenantID)
	authKitTenantSlug = strings.ToLower(strings.TrimSpace(authKitTenantSlug))
	if c.pool == nil {
		return tenant.ID{}, "", errors.New("controlplane: pgx pool unavailable for tenant resolution")
	}

	if authKitTenantID != "" {
		tid, slug, err := c.tenantDirectoryRow(ctx, `authkit_tenant_id = $1`, authKitTenantID)
		if err == nil {
			return tid, slug, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return tenant.ID{}, "", err
		}
		if authKitTenantSlug == "" {
			return tenant.ID{}, "", ErrServiceTokenTenantUnresolved
		}
		// Legacy directory row not yet linked by uuid: accept the slug match
		// only when the row carries NO AuthKit tenant uuid at all.
		tid, slug, err = c.tenantDirectoryRow(ctx,
			`lower(authkit_tenant_slug) = $1 AND authkit_tenant_id IS NULL`, authKitTenantSlug)
		if errors.Is(err, pgx.ErrNoRows) {
			return tenant.ID{}, "", ErrServiceTokenTenantUnresolved
		}
		return tid, slug, err
	}

	if authKitTenantSlug == "" {
		return tenant.ID{}, "", ErrServiceTokenTenantUnresolved
	}
	// Legacy credential without a `tenant_id` claim: mutable-slug lookup.
	tid, slug, err := c.tenantDirectoryRow(ctx, `lower(authkit_tenant_slug) = $1`, authKitTenantSlug)
	if errors.Is(err, pgx.ErrNoRows) {
		return tenant.ID{}, "", ErrServiceTokenTenantUnresolved
	}
	return tid, slug, err
}

// tenantDirectoryRow runs one billing.tenants directory lookup with the given
// WHERE predicate (which must reference exactly one $1 argument). It returns
// pgx.ErrNoRows untouched so callers can decide whether a fallback applies;
// an inactive (suspended) tenant is rejected with ErrServiceTokenTenantUnresolved
// and never falls through to another key.
func (c *ControlPlane) tenantDirectoryRow(ctx context.Context, where, arg string) (tenant.ID, string, error) {
	var (
		idStr  string
		slug   string
		status string
	)
	err := c.pool.QueryRow(ctx, `
		SELECT id::text, slug, status
		  FROM billing.tenants
		 WHERE `+where+`
		   AND deleted_at IS NULL
		 LIMIT 1
	`, arg).Scan(&idStr, &slug, &status)
	if err != nil {
		return tenant.ID{}, "", err
	}
	if status != "active" {
		return tenant.ID{}, "", ErrServiceTokenTenantUnresolved
	}

	tid, err := tenant.ParseID(idStr)
	if err != nil {
		return tenant.ID{}, "", err
	}
	return tid, slug, nil
}
