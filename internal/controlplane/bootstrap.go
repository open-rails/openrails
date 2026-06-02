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
	OperatorOrgSlug string
	OperatorOrgID   string
	// OrgCreated is true if CreateOrg ran (false if the org already existed).
	OrgCreated bool
	// OATMinted is true if an initial operator OAT was minted on this run
	// (false if one already existed). When true, OATSecret holds the one-time
	// plaintext token — it is NOT persisted by AuthKit and cannot be recovered.
	OATMinted bool
	OATSecret string
	OATKeyID  string
}

// BootstrapOptions parameterizes the control-plane bootstrap.
type BootstrapOptions struct {
	// OperatorOrgSlug is the AuthKit org slug that operates the default tenant.
	// When empty, falls back to auth.operator_org_slug, then "operator".
	OperatorOrgSlug string

	// InitialAdminUserID, when set, is assigned the operator role in the
	// operator org (the user must already exist and be a member, or AddMember is
	// attempted first). Optional: self-hosted bootstrap may seed the operator OAT
	// alone and add an admin user later.
	InitialAdminUserID string

	// MintInitialOAT controls whether an operator OAT is minted when none exists.
	// Defaults to true.
	MintInitialOAT bool
}

// Bootstrap idempotently ensures the OpenRails control-plane state for the
// DEFAULT tenant (#223): the operator AuthKit org exists, the OpenRails operator
// role is defined and granted the full `openrails:*` permission catalog, the
// default tenant directory row records the operator org, and (optionally) an
// initial operator OAT is minted when the org has none.
//
// It runs AFTER migrations / at startup, exclusively through in-process AuthKit
// CORE calls (CreateOrg / DefineRole / SetRolePermissions / AssignRole /
// MintOrgAccessToken) — never raw AuthKit SQL or a private HTTP route. Re-running
// it is safe: org creation, role definition, and catalog seeding are upserts;
// the OAT is minted only when none already exists.
func (c *ControlPlane) Bootstrap(ctx context.Context, opts BootstrapOptions) (*BootstrapResult, error) {
	if c == nil {
		return nil, errors.New("controlplane: nil control plane")
	}
	core := c.Core()
	if core == nil {
		return nil, errors.New("controlplane: core service unavailable")
	}

	slug := strings.ToLower(strings.TrimSpace(opts.OperatorOrgSlug))
	if slug == "" && c.cfg != nil && c.cfg.Auth != nil {
		slug = strings.ToLower(strings.TrimSpace(c.cfg.Auth.OperatorOrgSlug))
	}
	if slug == "" {
		slug = "operator"
	}

	res := &BootstrapResult{OperatorOrgSlug: slug}

	// 1. Ensure the operator org exists (idempotent: resolve, else create).
	org, err := core.ResolveOrgBySlug(ctx, slug)
	if err != nil {
		if !errors.Is(err, authcore.ErrOrgNotFound) {
			return nil, fmt.Errorf("controlplane: resolve operator org %q: %w", slug, err)
		}
		created, cerr := core.CreateOrg(ctx, slug)
		if cerr != nil {
			return nil, fmt.Errorf("controlplane: create operator org %q: %w", slug, cerr)
		}
		org = created
		res.OrgCreated = true
		log.WithField("operator_org", slug).Info("controlplane: created operator org")
	}
	res.OperatorOrgID = org.ID

	// 2. Define the operator role and seed it the full OpenRails catalog
	//    (idempotent: DefineRole upserts, SetRolePermissions replaces).
	if err := core.DefineRole(ctx, slug, OperatorRole); err != nil {
		return nil, fmt.Errorf("controlplane: define operator role: %w", err)
	}
	if err := core.SetRolePermissions(ctx, slug, OperatorRole, OperatorRolePermissions()); err != nil {
		return nil, fmt.Errorf("controlplane: seed operator role permissions: %w", err)
	}

	// 3. Optionally assign the operator role to an initial admin user.
	if adminID := strings.TrimSpace(opts.InitialAdminUserID); adminID != "" {
		// AddMember is idempotent (ON CONFLICT DO NOTHING semantics in AuthKit);
		// AssignRole only succeeds once the user is a member.
		if err := core.AddMember(ctx, slug, adminID); err != nil {
			return nil, fmt.Errorf("controlplane: add initial admin to operator org: %w", err)
		}
		if err := core.AssignRole(ctx, slug, adminID, OperatorRole); err != nil {
			return nil, fmt.Errorf("controlplane: assign operator role to initial admin: %w", err)
		}
		log.WithFields(log.Fields{"operator_org": slug, "user_id": adminID}).
			Info("controlplane: assigned operator role to initial admin")
	}

	// 4. Record the operator org on the DEFAULT tenant directory row so tenant
	//    resolution / admin policy can map the default tenant -> operator org.
	if err := c.recordOperatorOrgOnDefaultTenant(ctx, tenant.DefaultID, org.ID, slug); err != nil {
		return nil, err
	}

	// 5. Mint an initial operator OAT only when the org has none yet.
	if opts.MintInitialOAT {
		existing, lerr := core.ListOrgAccessTokens(ctx, slug)
		if lerr != nil {
			return nil, fmt.Errorf("controlplane: list operator OATs: %w", lerr)
		}
		if !anyLiveOAT(existing) {
			name := "openrails-bootstrap-operator"
			if c.cfg != nil && c.cfg.Auth != nil && c.cfg.Auth.ControlPlane != nil {
				if n := strings.TrimSpace(c.cfg.Auth.ControlPlane.OperatorOATName); n != "" {
					name = n
				}
			}
			oat, secret, merr := core.MintOrgAccessToken(ctx, slug, name, OperatorRolePermissions(), "", nil)
			if merr != nil {
				return nil, fmt.Errorf("controlplane: mint initial operator OAT: %w", merr)
			}
			res.OATMinted = true
			res.OATSecret = secret
			res.OATKeyID = oat.KeyID
			log.WithFields(log.Fields{"operator_org": slug, "oat_key_id": oat.KeyID}).
				Warn("controlplane: minted initial operator OAT (secret shown once)")
		}
	}

	return res, nil
}

// anyLiveOAT reports whether the org already has at least one non-revoked OAT,
// so bootstrap does not mint a duplicate on re-run.
func anyLiveOAT(toks []authcore.OrgAccessToken) bool {
	for _, t := range toks {
		if t.RevokedAt == nil {
			return true
		}
	}
	return false
}

// recordOperatorOrgOnDefaultTenant writes the operator org id/slug onto the
// default tenant directory row (billing.tenants). billing.* is OpenRails-owned
// control-plane state, so this is a direct, idempotent UPDATE — not AuthKit SQL.
func (c *ControlPlane) recordOperatorOrgOnDefaultTenant(ctx context.Context, tenantID tenant.ID, orgID, orgSlug string) error {
	if c.pool == nil {
		return errors.New("controlplane: pgx pool unavailable for tenant directory update")
	}
	_, err := c.pool.Exec(ctx, `
		UPDATE billing.tenants
		   SET authkit_org_id   = $2,
		       authkit_org_slug = $3,
		       updated_at       = current_timestamp
		 WHERE id = $1::uuid
		   AND (authkit_org_id IS DISTINCT FROM $2 OR authkit_org_slug IS DISTINCT FROM $3)
	`, tenantID.String(), orgID, orgSlug)
	if err != nil {
		return fmt.Errorf("controlplane: record operator org on default tenant: %w", err)
	}
	return nil
}
