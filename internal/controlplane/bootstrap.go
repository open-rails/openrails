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

const BootstrapAdminAPIKeyName = "openrails-bootstrap-admin"

// BootstrapResult reports what the idempotent bootstrap did/ensured.
type BootstrapResult struct {
	BootstrapOrgSlug string
	BootstrapOrgID   string
	// OrgCreated is true if CreateOrg ran (false if the AuthKit org already existed).
	OrgCreated bool
	// APIKeyMinted is true if an initial admin API key was minted on this run
	// (false if one already existed). When true, APIKeySecret holds the one-time
	// plaintext key — it is NOT persisted by AuthKit and cannot be recovered.
	APIKeyMinted bool
	APIKeySecret string
	APIKeyID     string
}

// BootstrapOptions parameterizes the control-plane bootstrap.
type BootstrapOptions struct {
	// BootstrapOrgSlug is the AuthKit org slug under which this merchant's
	// admin role + deployment admin API key are seeded (#312). It is also
	// the OpenRails org slug used to locate the directory row to link the org
	// onto (#336): there is NO default merchant, so an empty slug is an error and a
	// matching openrails.merchants row must already exist. There is NO separate
	// "operator" AuthKit org.
	BootstrapOrgSlug string

	// InitialAdminUserID, when set, is assigned the admin role in the bootstrap
	// org (the user must already exist and be a member, or AddMember is
	// attempted first). Optional: self-hosted bootstrap may seed the admin API
	// key alone and add an admin user later.
	InitialAdminUserID string

	// MintInitialAPIKey controls whether a deployment admin API key is
	// minted when none exists. Defaults to true.
	MintInitialAPIKey bool
}

// Bootstrap idempotently ensures the OpenRails control-plane state for the
// bootstrap merchant (#223/#312): the merchant's AuthKit org exists, the
// OpenRails operator role is defined and granted the concrete merchant-local
// `org:` permission catalog, the default merchant directory row records its
// AuthKit org, and an initial deployment admin API key is optionally minted
// when the org has none. HARDCUT (#312): there is NO separate "operator" AuthKit org -
// admin authority is merchant-local AuthKit `org:` permission state under the
// default merchant's own org (or carried on the minted admin API key).
//
// It runs AFTER migrations / at startup, exclusively through in-process AuthKit
// CORE calls (CreateOrg / AddMember / AssignRole / MintAPIKey) - never raw
// AuthKit SQL or a private HTTP route. OpenRails defines NO role of its own
// (#543): the admin is made the org `owner` (AuthKit's built-in `org:*` role),
// and the API key is direct permission-scoped. Re-running it is safe: org
// creation and owner assignment are idempotent; the API key is minted only when
// none already exists.
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

	// 2. Make the initial admin the org OWNER. OpenRails defines/seeds NO role of
	//    its own (#543): authority comes from AuthKit's built-in `owner` role,
	//    whose `org:*` grant (seeded by CreateOrg) already covers every OpenRails
	//    `org:` catalog permission. Server-to-server automation uses the
	//    direct permission-scoped admin API key minted below, never a role.
	if adminID := strings.TrimSpace(opts.InitialAdminUserID); adminID != "" {
		// AddMember is idempotent; AssignRole only succeeds once a member.
		// Assigning the reserved `owner` role does NOT alter its seeded `org:*`
		// grant — AssignRole's materialize step is a no-op for the owner role.
		if err := core.AddMember(ctx, slug, adminID); err != nil {
			return nil, fmt.Errorf("controlplane: add initial admin to bootstrap org: %w", err)
		}
		if err := core.AssignRole(ctx, slug, adminID, OwnerRole); err != nil {
			return nil, fmt.Errorf("controlplane: assign org owner to initial admin: %w", err)
		}
		log.WithFields(log.Fields{"bootstrap_org": slug, "user_id": adminID}).
			Info("controlplane: assigned org owner to initial admin")
	}

	// 4. Record the owning AuthKit org on the bootstrap merchant's directory row
	//    (openrails.merchants.owner_org_id, keyed by slug) so role-based authz can
	//    map this merchant -> owning org (#481 ownership link, NOT identity). The
	//    resolved OpenRails merchant id is what the admin API key below is
	//    resource-scoped to (no default merchant; #480).
	bootstrapMerchantID, err := c.recordOwnerOrgBySlug(ctx, slug, authkitOrg.ID)
	if err != nil {
		return nil, err
	}

	// 5. Mint an initial deployment admin API key only when the org has none yet.
	if opts.MintInitialAPIKey {
		existing, lerr := core.ListAPIKeys(ctx, slug)
		if lerr != nil {
			return nil, fmt.Errorf("controlplane: list admin API keys: %w", lerr)
		}
		if !anyLiveAPIKey(existing) {
			apiKey, secret, merr := core.MintAPIKeyWithOptions(ctx, slug, authcore.APIKeyMintOptions{
				Name:        BootstrapAdminAPIKeyName,
				Permissions: OperatorRolePermissions(),
				Resources:   []authcore.APIKeyResource{MerchantResource(bootstrapMerchantID)},
			})
			if merr != nil {
				return nil, fmt.Errorf("controlplane: mint initial admin API key: %w", merr)
			}
			res.APIKeyMinted = true
			res.APIKeySecret = secret
			res.APIKeyID = apiKey.KeyID
			log.WithFields(log.Fields{"bootstrap_org": slug, "api_key_id": apiKey.KeyID}).
				Warn("controlplane: minted initial admin API key (secret shown once)")
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

// anyLiveAPIKey reports whether the AuthKit org already has at least one non-revoked API key,
// so bootstrap does not mint a duplicate on re-run.
func anyLiveAPIKey(toks []authcore.APIKey) bool {
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
