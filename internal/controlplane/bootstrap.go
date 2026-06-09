package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	authcore "github.com/open-rails/authkit/core"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/pkg/tenant"
)

// BootstrapResult reports what the idempotent bootstrap did/ensured.
type BootstrapResult struct {
	BootstrapTenantSlug string
	BootstrapTenantID   string
	// TenantCreated is true if CreateTenant ran (false if the AuthKit tenant already existed).
	TenantCreated bool
	// ServiceTokenMinted is true if an initial admin service token was minted on this run
	// (false if one already existed). When true, ServiceTokenSecret holds the one-time
	// plaintext token — it is NOT persisted by AuthKit and cannot be recovered.
	ServiceTokenMinted bool
	ServiceTokenSecret string
	ServiceTokenKeyID  string
}

// BootstrapOptions parameterizes the control-plane bootstrap.
type BootstrapOptions struct {
	// BootstrapTenantSlug is the AuthKit tenant slug under which the default
	// tenant's admin role + deployment admin service token are seeded (#312). When
	// empty, defaults to the default tenant slug ("default") — there is NO
	// separate "operator" AuthKit tenant.
	BootstrapTenantSlug string

	// InitialAdminUserID, when set, is assigned the admin role in the bootstrap
	// tenant (the user must already exist and be a member, or AddMember is
	// attempted first). Optional: self-hosted bootstrap may seed the admin service
	// token alone and add an admin user later.
	InitialAdminUserID string

	// MintInitialServiceToken controls whether a deployment admin service token is
	// minted when none exists. Defaults to true.
	MintInitialServiceToken bool
}

// Bootstrap idempotently ensures the OpenRails control-plane state for the
// DEFAULT tenant (#223/#312): the default tenant's AuthKit org exists, the
// OpenRails admin role is defined and granted the full `openrails:*` permission
// catalog, the default tenant directory row records its AuthKit org, and
// (optionally) an initial deployment admin service token is minted when the org
// has none. HARDCUT (#312): there is NO separate "operator" AuthKit tenant —
// admin authority is the openrails:admin permission held under the default
// tenant's own org (or carried on the minted admin service token).
//
// It runs AFTER migrations / at startup, exclusively through in-process AuthKit
// CORE calls (CreateTenant / DefineRole / SetRolePermissions / AssignRole /
// MintServiceToken) — never raw AuthKit SQL or a private HTTP route. Re-running
// it is safe: tenant creation, role definition, and catalog seeding are upserts;
// the service token is minted only when none already exists.
func (c *ControlPlane) Bootstrap(ctx context.Context, opts BootstrapOptions) (*BootstrapResult, error) {
	if c == nil {
		return nil, errors.New("controlplane: nil control plane")
	}
	core := c.Core()
	if core == nil {
		return nil, errors.New("controlplane: core service unavailable")
	}

	slug := strings.ToLower(strings.TrimSpace(opts.BootstrapTenantSlug))
	if slug == "" {
		slug = tenant.DefaultSlug
	}

	res := &BootstrapResult{BootstrapTenantSlug: slug}

	// 1. Ensure the bootstrap (default-tenant) AuthKit org exists (idempotent:
	//    resolve, else create).
	authkitTenant, err := core.ResolveTenantBySlug(ctx, slug)
	if err != nil {
		if !errors.Is(err, authcore.ErrTenantNotFound) {
			return nil, fmt.Errorf("controlplane: resolve bootstrap tenant %q: %w", slug, err)
		}
		created, cerr := core.CreateTenant(ctx, slug)
		if cerr != nil {
			if !errors.Is(cerr, authcore.ErrOwnerSlugTaken) {
				return nil, fmt.Errorf("controlplane: create bootstrap tenant %q: %w", slug, cerr)
			}
			created, cerr = core.ResolveTenantBySlug(ctx, slug)
			if cerr != nil {
				return nil, fmt.Errorf("controlplane: resolve concurrently-created bootstrap tenant %q: %w", slug, cerr)
			}
		} else {
			res.TenantCreated = true
			log.WithField("bootstrap_tenant", slug).Info("controlplane: created bootstrap tenant org")
		}
		authkitTenant = created
	}
	res.BootstrapTenantID = authkitTenant.ID

	// 2. Define the admin role and seed it the full OpenRails catalog
	//    (idempotent: DefineRole upserts, SetRolePermissions replaces).
	if err := core.DefineRole(ctx, slug, OperatorRole); err != nil {
		return nil, fmt.Errorf("controlplane: define admin role: %w", err)
	}
	if err := core.SetRolePermissions(ctx, slug, OperatorRole, OperatorRolePermissions()); err != nil {
		return nil, fmt.Errorf("controlplane: seed admin role permissions: %w", err)
	}

	// 3. Optionally assign the admin role to an initial admin user.
	if adminID := strings.TrimSpace(opts.InitialAdminUserID); adminID != "" {
		// AddMember is idempotent (ON CONFLICT DO NOTHING semantics in AuthKit);
		// AssignRole only succeeds once the user is a member.
		if err := core.AddMember(ctx, slug, adminID); err != nil {
			return nil, fmt.Errorf("controlplane: add initial admin to bootstrap tenant: %w", err)
		}
		if err := core.AssignRole(ctx, slug, adminID, OperatorRole); err != nil {
			return nil, fmt.Errorf("controlplane: assign admin role to initial admin: %w", err)
		}
		log.WithFields(log.Fields{"bootstrap_tenant": slug, "user_id": adminID}).
			Info("controlplane: assigned admin role to initial admin")
	}

	// 4. Record the default tenant's AuthKit org on the DEFAULT tenant directory
	//    row so tenant resolution / admin policy can map the default tenant -> org.
	if err := c.recordAuthKitTenantOnDefaultTenant(ctx, tenant.DefaultID, authkitTenant.ID, slug); err != nil {
		return nil, err
	}

	// 5. Mint an initial deployment admin service token only when the org has none yet.
	if opts.MintInitialServiceToken {
		existing, lerr := core.ListServiceTokens(ctx, slug)
		if lerr != nil {
			return nil, fmt.Errorf("controlplane: list admin service tokens: %w", lerr)
		}
		if !anyLiveServiceToken(existing) {
			name := "openrails-bootstrap-admin"
			if c.cfg != nil && c.cfg.Auth != nil && c.cfg.Auth.ControlPlane != nil {
				if n := strings.TrimSpace(c.cfg.Auth.ControlPlane.BootstrapAdminServiceTokenName); n != "" {
					name = n
				}
			}
			serviceToken, secret, merr := core.MintServiceTokenWithOptions(ctx, slug, authcore.ServiceTokenMintOptions{
				Name:        name,
				Permissions: OperatorRolePermissions(),
				Resources:   []authcore.ServiceTokenResource{TenantResource(tenant.DefaultID)},
			})
			if merr != nil {
				return nil, fmt.Errorf("controlplane: mint initial admin service token: %w", merr)
			}
			res.ServiceTokenMinted = true
			res.ServiceTokenSecret = secret
			res.ServiceTokenKeyID = serviceToken.KeyID
			log.WithFields(log.Fields{"bootstrap_tenant": slug, "service_token_key_id": serviceToken.KeyID}).
				Warn("controlplane: minted initial admin service token (secret shown once)")
		}
	}

	return res, nil
}

// PlatformBootstrapResult reports what the platform-superadmin bootstrap did.
type PlatformBootstrapResult struct {
	PlatformTenantSlug string
	PlatformTenantID   string
	TenantCreated      bool
	AdminAssigned      bool
}

// BootstrapPlatform idempotently ensures the managed-hosting PLATFORM org
// (issue #226), DISTINCT from any tenant operator tenant: the platform AuthKit tenant
// exists, the openrails-platform-superadmin role is defined and granted ONLY the
// openrails:platform:superadmin permission, and (optionally) an initial platform
// admin user is assigned that role.
//
// It runs through in-process AuthKit CORE calls (CreateTenant / DefineRole /
// SetRolePermissions / AssignRole) — never raw SQL. Re-running is safe (upserts).
//
// No-op (returns nil, nil) when no platform tenant is configured: a single-tenant
// or non-managed deployment never grows a cross-tenant superadmin.
func (c *ControlPlane) BootstrapPlatform(ctx context.Context) (*PlatformBootstrapResult, error) {
	if c == nil {
		return nil, errors.New("controlplane: nil control plane")
	}
	slug := c.PlatformTenantSlug()
	if slug == "" {
		return nil, nil
	}
	core := c.Core()
	if core == nil {
		return nil, errors.New("controlplane: core service unavailable")
	}

	res := &PlatformBootstrapResult{PlatformTenantSlug: slug}

	authkitTenant, err := core.ResolveTenantBySlug(ctx, slug)
	if err != nil {
		if !errors.Is(err, authcore.ErrTenantNotFound) {
			return nil, fmt.Errorf("controlplane: resolve platform tenant %q: %w", slug, err)
		}
		created, cerr := core.CreateTenant(ctx, slug)
		if cerr != nil {
			if !errors.Is(cerr, authcore.ErrOwnerSlugTaken) {
				return nil, fmt.Errorf("controlplane: create platform tenant %q: %w", slug, cerr)
			}
			created, cerr = core.ResolveTenantBySlug(ctx, slug)
			if cerr != nil {
				return nil, fmt.Errorf("controlplane: resolve concurrently-created platform tenant %q: %w", slug, cerr)
			}
		} else {
			res.TenantCreated = true
			log.WithField("platform_tenant", slug).Info("controlplane: created platform superadmin org")
		}
		authkitTenant = created
	}
	res.PlatformTenantID = authkitTenant.ID

	if err := core.DefineRole(ctx, slug, PlatformRole); err != nil {
		return nil, fmt.Errorf("controlplane: define platform superadmin role: %w", err)
	}
	if err := core.SetRolePermissions(ctx, slug, PlatformRole, PlatformRolePermissions()); err != nil {
		return nil, fmt.Errorf("controlplane: seed platform superadmin permissions: %w", err)
	}

	if c.cfg != nil && c.cfg.Auth != nil && c.cfg.Auth.ControlPlane != nil {
		if adminID := strings.TrimSpace(c.cfg.Auth.ControlPlane.PlatformAdminUserID); adminID != "" {
			if err := core.AddMember(ctx, slug, adminID); err != nil {
				return nil, fmt.Errorf("controlplane: add platform admin to platform tenant: %w", err)
			}
			if err := core.AssignRole(ctx, slug, adminID, PlatformRole); err != nil {
				return nil, fmt.Errorf("controlplane: assign platform superadmin role: %w", err)
			}
			res.AdminAssigned = true
			log.WithFields(log.Fields{"platform_tenant": slug, "user_id": adminID}).
				Info("controlplane: assigned platform superadmin role to initial platform admin")
		}
	}

	return res, nil
}

// anyLiveServiceToken reports whether the AuthKit tenant already has at least one non-revoked service token,
// so bootstrap does not mint a duplicate on re-run.
func anyLiveServiceToken(toks []authcore.ServiceToken) bool {
	for _, t := range toks {
		if t.RevokedAt == nil {
			return true
		}
	}
	return false
}

// recordAuthKitTenantOnDefaultTenant writes the default tenant's AuthKit org
// id/slug onto the default tenant directory row (billing.tenants). billing.* is
// OpenRails-owned control-plane state, so this is a direct, idempotent UPDATE —
// not AuthKit SQL.
func (c *ControlPlane) recordAuthKitTenantOnDefaultTenant(ctx context.Context, tenantID tenant.ID, authKitTenantID, authKitTenantSlug string) error {
	if c.pool == nil {
		return errors.New("controlplane: pgx pool unavailable for tenant directory update")
	}
	_, err := c.pool.Exec(ctx, `
		UPDATE billing.tenants
		   SET authkit_tenant_id   = $2,
		       authkit_tenant_slug = $3,
		       updated_at       = current_timestamp
		 WHERE id = $1::uuid
		   AND (authkit_tenant_id IS DISTINCT FROM $2 OR authkit_tenant_slug IS DISTINCT FROM $3)
	`, tenantID.String(), authKitTenantID, authKitTenantSlug)
	if err != nil {
		return fmt.Errorf("controlplane: record authkit tenant on default tenant: %w", err)
	}
	return nil
}
