package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	authcore "github.com/open-rails/authkit/core"
	authhttp "github.com/open-rails/authkit/http"

	"github.com/open-rails/openrails/pkg/tenant"
)

// ServiceJWTGrant is OpenRails' server-side authorization for a caller-minted
// service JWT principal. JWT permissions are requests; this grant is authority.
type ServiceJWTGrant struct {
	TenantID    tenant.ID
	Issuer      string
	Subject     string
	Permissions []string
	Resources   []authcore.ServiceTokenResource
	Enabled     bool
}

// UpsertServiceJWTGrant creates or updates the server-side grant for one
// issuer+subject under a tenant. It is used by declarative bootstrap.
func (c *ControlPlane) UpsertServiceJWTGrant(ctx context.Context, grant ServiceJWTGrant) error {
	if c == nil || c.pool == nil {
		return ErrNoControlPlane
	}
	if (grant.TenantID == tenant.ID{}) {
		return ErrServiceTokenTenantUnresolved
	}
	issuer := strings.TrimSpace(grant.Issuer)
	subject := strings.TrimSpace(grant.Subject)
	if issuer == "" || subject == "" {
		return authcore.ErrInvalidServiceJWT
	}
	permissions := cleanPermissionList(grant.Permissions)
	if len(permissions) == 0 {
		return ErrServiceTokenScopeDenied
	}
	resources := grant.Resources
	if len(resources) == 0 {
		resources = []authcore.ServiceTokenResource{TenantResource(grant.TenantID)}
	}
	if err := validateServiceTokenResources(grant.TenantID, resources); err != nil {
		return err
	}
	rawResources, err := json.Marshal(resources)
	if err != nil {
		return err
	}
	_, err = c.pool.Exec(ctx, `
		INSERT INTO billing.service_jwt_grants (tenant_id, issuer, subject, permissions, resources, enabled)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, issuer, subject) DO UPDATE
		   SET permissions = EXCLUDED.permissions,
		       resources   = EXCLUDED.resources,
		       enabled     = EXCLUDED.enabled,
		       updated_at  = current_timestamp
	`, grant.TenantID.String(), issuer, subject, permissions, rawResources, grant.Enabled)
	return err
}

// ResolveServiceJWT validates a first-party OIDC service JWT and resolves the
// effective OpenRails service principal after intersecting requested JWT
// permissions/resources with the server-side grant.
func (c *ControlPlane) ResolveServiceJWT(ctx context.Context, token string) (*ResolvedServiceToken, error) {
	if c == nil || c.delegatedVerifier == nil {
		return nil, ErrNoControlPlane
	}
	_, principal, err := c.delegatedVerifier.VerifyServiceJWT(ctx, strings.TrimSpace(token), authhttp.WithServiceJWTMaxLifetime(authcore.DefaultServiceJWTLifetime))
	if err != nil {
		return nil, err
	}

	tid, tslug, err := c.tenantForIssuer(ctx, principal.Issuer)
	if err != nil {
		return nil, err
	}
	grant, err := c.serviceJWTGrant(ctx, tid, principal.Issuer, principal.Subject)
	if err != nil {
		return nil, err
	}

	permissions := intersectPermissions(principal.Permissions, grant.Permissions)
	if len(permissions) == 0 {
		return nil, ErrServiceTokenScopeDenied
	}
	resources, err := intersectResources(tid, principal.Resources, grant.Resources)
	if err != nil {
		return nil, err
	}
	if err := validateServiceTokenResources(tid, resources); err != nil {
		return nil, err
	}
	return &ResolvedServiceToken{
		AuthKitTenantSlug: tslug,
		TenantID:          tid,
		TenantSlug:        tslug,
		Permissions:       permissions,
		Resources:         resources,
	}, nil
}

func (c *ControlPlane) serviceJWTGrant(ctx context.Context, tid tenant.ID, issuer, subject string) (*ServiceJWTGrant, error) {
	var (
		permissions []string
		raw         []byte
		enabled     bool
	)
	err := c.pool.QueryRow(ctx, `
		SELECT permissions, resources, enabled
		  FROM billing.service_jwt_grants
		 WHERE tenant_id = $1
		   AND issuer = $2
		   AND subject = $3
		   AND enabled
		 LIMIT 1
	`, tid.String(), strings.TrimSpace(issuer), strings.TrimSpace(subject)).Scan(&permissions, &raw, &enabled)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrServiceTokenScopeDenied
		}
		return nil, err
	}
	var resources []authcore.ServiceTokenResource
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &resources); err != nil {
			return nil, fmt.Errorf("decode service JWT grant resources: %w", err)
		}
	}
	return &ServiceJWTGrant{
		TenantID:    tid,
		Issuer:      issuer,
		Subject:     subject,
		Permissions: cleanPermissionList(permissions),
		Resources:   resources,
		Enabled:     enabled,
	}, nil
}

func cleanPermissionList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

func intersectPermissions(requested, granted []string) []string {
	requested = cleanPermissionList(requested)
	granted = cleanPermissionList(granted)
	if len(requested) == 0 || len(granted) == 0 {
		return nil
	}
	grantAll := false
	grants := map[string]struct{}{}
	for _, p := range granted {
		if p == PermAdmin {
			grantAll = true
		}
		grants[p] = struct{}{}
	}
	out := make([]string, 0, len(requested))
	for _, p := range requested {
		if grantAll {
			out = append(out, p)
			continue
		}
		if _, ok := grants[p]; ok {
			out = append(out, p)
		}
	}
	return out
}

func intersectResources(tid tenant.ID, requested, granted []authcore.ServiceTokenResource) ([]authcore.ServiceTokenResource, error) {
	if len(granted) == 0 {
		return nil, ErrServiceTokenScopeDenied
	}
	if len(requested) == 0 {
		return granted, nil
	}
	out := make([]authcore.ServiceTokenResource, 0, len(requested))
	for _, r := range requested {
		if hasResource(granted, r.Kind, r.ID) {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil, ErrServiceTokenScopeDenied
	}
	if !hasResource(out, ResourceKindTenant, tid.String()) && hasResource(granted, ResourceKindTenant, tid.String()) {
		out = append(out, TenantResource(tid))
	}
	return out, nil
}
