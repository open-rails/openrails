package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	authcore "github.com/open-rails/authkit/core"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/pkg/merchant"
)

const BootstrapAdminServiceTokenName = "openrails-bootstrap-admin"

// BootstrapResult reports what the idempotent bootstrap did/ensured.
type BootstrapResult struct {
	BootstrapOrgSlug string
	BootstrapOrgID   string
	// OrgCreated is true if CreateOrg ran (false if the AuthKit org already existed).
	OrgCreated bool
	// ServiceTokenMinted is true if an initial admin service token was minted on this run
	// (false if one already existed). When true, ServiceTokenSecret holds the one-time
	// plaintext token — it is NOT persisted by AuthKit and cannot be recovered.
	ServiceTokenMinted bool
	ServiceTokenSecret string
	ServiceTokenKeyID  string
}

// BootstrapOptions parameterizes the control-plane bootstrap.
type BootstrapOptions struct {
	// BootstrapOrgSlug is the AuthKit org slug under which this merchant's
	// admin role + deployment admin service token are seeded (#312). It is also
	// the OpenRails org slug used to locate the directory row to link the org
	// onto (#336): there is NO default merchant, so an empty slug is an error and a
	// matching openrails.merchants row must already exist. There is NO separate
	// "operator" AuthKit org.
	BootstrapOrgSlug string

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
// DEFAULT tenant (#223/#312): the default merchant's AuthKit org exists, the
// OpenRails admin role is defined and granted the full `openrails:*` permission
// catalog, the default merchant directory row records its AuthKit org, and
// (optionally) an initial deployment admin service token is minted when the org
// has none. HARDCUT (#312): there is NO separate "operator" AuthKit org —
// admin authority is the openrails:admin permission held under the default
// merchant's own org (or carried on the minted admin service token).
//
// It runs AFTER migrations / at startup, exclusively through in-process AuthKit
// CORE calls (CreateOrg / DefineRole / SetRolePermissions / AssignRole /
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

	slug := strings.ToLower(strings.TrimSpace(opts.BootstrapOrgSlug))
	if slug == "" {
		// No default merchant (#336): bootstrap must name the org slug to seed.
		return nil, errors.New("controlplane: bootstrap requires an org slug (BootstrapOrgSlug)")
	}

	res := &BootstrapResult{BootstrapOrgSlug: slug}

	// 1. Ensure the bootstrap (default-merchant) AuthKit org exists (idempotent:
	//    resolve, else create).
	authkitOrg, err := core.ResolveOrgBySlug(ctx, slug)
	if err != nil {
		if !errors.Is(err, authcore.ErrOrgNotFound) {
			return nil, fmt.Errorf("controlplane: resolve bootstrap org %q: %w", slug, err)
		}
		created, cerr := core.CreateOrg(ctx, slug)
		if cerr != nil {
			if !errors.Is(cerr, authcore.ErrOwnerSlugTaken) {
				return nil, fmt.Errorf("controlplane: create bootstrap org %q: %w", slug, cerr)
			}
			created, cerr = core.ResolveOrgBySlug(ctx, slug)
			if cerr != nil {
				return nil, fmt.Errorf("controlplane: resolve concurrently-created bootstrap org %q: %w", slug, cerr)
			}
		} else {
			res.OrgCreated = true
			log.WithField("bootstrap_org", slug).Info("controlplane: created bootstrap org org")
		}
		authkitOrg = created
	}
	res.BootstrapOrgID = authkitOrg.ID

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
			return nil, fmt.Errorf("controlplane: add initial admin to bootstrap org: %w", err)
		}
		if err := core.AssignRole(ctx, slug, adminID, OperatorRole); err != nil {
			return nil, fmt.Errorf("controlplane: assign admin role to initial admin: %w", err)
		}
		log.WithFields(log.Fields{"bootstrap_org": slug, "user_id": adminID}).
			Info("controlplane: assigned admin role to initial admin")
	}

	// 4. Record the owning AuthKit org on the bootstrap merchant's directory row
	//    (openrails.merchants.owner_org_id, keyed by slug) so role-based authz can
	//    map this merchant -> owning org (#481 ownership link, NOT identity). The
	//    resolved OpenRails merchant id is what the admin service token below is
	//    resource-scoped to (no default merchant; #480).
	bootstrapMerchantID, err := c.recordOwnerOrgBySlug(ctx, slug, authkitOrg.ID)
	if err != nil {
		return nil, err
	}

	// 5. Mint an initial deployment admin service token only when the org has none yet.
	if opts.MintInitialServiceToken {
		existing, lerr := core.ListServiceTokens(ctx, slug)
		if lerr != nil {
			return nil, fmt.Errorf("controlplane: list admin service tokens: %w", lerr)
		}
		if !anyLiveServiceToken(existing) {
			serviceToken, secret, merr := core.MintServiceTokenWithOptions(ctx, slug, authcore.ServiceTokenMintOptions{
				Name:        BootstrapAdminServiceTokenName,
				Permissions: OperatorRolePermissions(),
				Resources:   []authcore.ServiceTokenResource{MerchantResource(bootstrapMerchantID)},
			})
			if merr != nil {
				return nil, fmt.Errorf("controlplane: mint initial admin service token: %w", merr)
			}
			res.ServiceTokenMinted = true
			res.ServiceTokenSecret = secret
			res.ServiceTokenKeyID = serviceToken.KeyID
			log.WithFields(log.Fields{"bootstrap_org": slug, "service_token_key_id": serviceToken.KeyID}).
				Warn("controlplane: minted initial admin service token (secret shown once)")
		}
	}

	return res, nil
}

// PlatformBootstrapResult reports what the platform-superadmin bootstrap did.
type PlatformBootstrapResult struct {
	PlatformOrgSlug string
	PlatformOrgID   string
	OrgCreated      bool
	AdminAssigned   bool
}

// BootstrapPlatform is intentionally inert in the source-available OpenRails
// control plane. Cross-merchant platform administration belongs in the private
// openrails-saas deployment layer, not in self-hosted OpenRails config.
func (c *ControlPlane) BootstrapPlatform(ctx context.Context) (*PlatformBootstrapResult, error) {
	if c == nil {
		return nil, errors.New("controlplane: nil control plane")
	}
	return nil, nil
}

// anyLiveServiceToken reports whether the AuthKit org already has at least one non-revoked service token,
// so bootstrap does not mint a duplicate on re-run.
func anyLiveServiceToken(toks []authcore.ServiceToken) bool {
	for _, t := range toks {
		if t.RevokedAt == nil {
			return true
		}
	}
	return false
}

// recordOwnerOrgBySlug writes the owning AuthKit org uuid onto the bootstrap
// merchant's directory row (openrails.merchants.owner_org_id), keyed by the
// bootstrap slug, and returns the resolved OpenRails merchant id. This is the
// #481 OWNERSHIP link (who administers), not identity — openrails.* is
// OpenRails-owned control-plane state, so this is a direct, idempotent
// UPDATE ... RETURNING. There is no default merchant the row could fall back to
// (#480), so a missing directory row is an error the caller must surface (register
// the merchant before bootstrap).
func (c *ControlPlane) recordOwnerOrgBySlug(ctx context.Context, slug, ownerOrgID string) (merchant.ID, error) {
	if c.pool == nil {
		return merchant.ID{}, errors.New("controlplane: pgx pool unavailable for merchant directory update")
	}
	var idStr string
	err := c.pool.QueryRow(ctx, `
		UPDATE openrails.merchants
		   SET owner_org_id = $2,
		       updated_at      = current_timestamp
		 WHERE lower(slug) = lower($1)
		   AND deleted_at IS NULL
		RETURNING id::text
	`, slug, ownerOrgID).Scan(&idStr)
	if errors.Is(err, pgx.ErrNoRows) {
		return merchant.ID{}, fmt.Errorf("controlplane: no openrails merchant directory row for bootstrap slug %q (register the merchant before bootstrap)", slug)
	}
	if err != nil {
		return merchant.ID{}, fmt.Errorf("controlplane: record owner org on merchant %q: %w", slug, err)
	}
	return merchant.ParseID(idStr)
}
