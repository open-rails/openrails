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
	// BootstrapOrgID is the merchant permission-group's internal id (#567).
	BootstrapOrgID string
	// OrgCreated is true if the merchant permission-group was created on this run
	// (false if it already existed).
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
	// BootstrapOrgSlug is the merchant slug (the merchant permission-group's
	// resource ref, #567) under which the admin owner + deployment admin API key
	// are seeded. It is also the slug used to locate the openrails.merchants
	// directory row: there is NO default merchant, so an empty slug is an error and
	// a matching openrails.merchants row must already exist.
	BootstrapOrgSlug string

	// InitialAdminUserID, when set, is assigned the merchant `owner` role
	// (= `merchant:*`). Optional: self-hosted bootstrap may seed the admin API key
	// alone and add an admin user later.
	InitialAdminUserID string

	// MintInitialAPIKey controls whether a deployment admin API key is
	// minted when none exists. Defaults to true.
	MintInitialAPIKey bool
}

// Bootstrap idempotently ensures the OpenRails control-plane state for the
// bootstrap merchant (#567): the merchant permission-group exists (a top-level
// child of `root`, NO parent org, NO owner_org coupling), the initial admin is
// its `owner` (auto-holds `merchant:*`), the merchant directory row records the
// merchant group's internal id, and an initial deployment admin API key is
// optionally minted under the merchant group when none exists.
//
// It runs AFTER migrations / at startup, exclusively through in-process AuthKit
// CORE calls (EnsureRootGroup / CreatePermissionGroup / AssignGroupRole /
// MintAPIKeyWithOptions) — never raw AuthKit SQL or a private HTTP route.
// Re-running it is safe: group creation and owner assignment are idempotent; the
// API key is minted only when none already exists.
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
		// No default merchant (#336): bootstrap must name the merchant slug to seed.
		return nil, errors.New("controlplane: bootstrap requires a merchant slug (BootstrapOrgSlug)")
	}

	res := &BootstrapResult{BootstrapOrgSlug: slug}

	// 0. Ensure the singleton root group exists and the declared containment is
	//    seeded (idempotent) before creating typed groups.
	if _, err := core.EnsureRootGroup(ctx); err != nil {
		return nil, fmt.Errorf("controlplane: ensure root group: %w", err)
	}
	if err := core.SeedPermissionGroupContainment(ctx); err != nil {
		return nil, fmt.Errorf("controlplane: seed permission-group containment: %w", err)
	}

	// 1. Ensure the merchant permission-group exists (idempotent: resolve, else
	//    create). The merchant IS the group — `type=merchant`, `resourceRef=slug`,
	//    parent=root. No parent org. The initial admin (if any) is seeded as owner
	//    at creation.
	groupID, err := core.ResolveGroupIDForSlug(ctx, MerchantType, slug)
	if errors.Is(err, authcore.ErrGroupNotFound) {
		groupID, err = core.CreatePermissionGroup(ctx, authcore.CreatePermissionGroupRequest{
			Persona:        MerchantType,
			ResourceSlug:    slug,
			ParentPersona:   authcore.RootPersona,
			OwnerSubjectID: strings.TrimSpace(opts.InitialAdminUserID),
		})
		if err != nil {
			return nil, fmt.Errorf("controlplane: create merchant group %q: %w", slug, err)
		}
		res.OrgCreated = true
		log.WithField("merchant", slug).Info("controlplane: created merchant permission-group")
	} else if err != nil {
		return nil, fmt.Errorf("controlplane: resolve merchant group %q: %w", slug, err)
	} else if adminID := strings.TrimSpace(opts.InitialAdminUserID); adminID != "" {
		// Group already existed: ensure the admin holds the owner role (idempotent).
		if aerr := core.AssignGroupRole(ctx, MerchantType, slug, adminID, authcore.SubjectKindUser, MerchantRoleOwner); aerr != nil {
			return nil, fmt.Errorf("controlplane: assign merchant owner to initial admin: %w", aerr)
		}
		log.WithFields(log.Fields{"merchant": slug, "user_id": adminID}).
			Info("controlplane: assigned merchant owner to initial admin")
	}
	res.BootstrapOrgID = groupID

	// 2. Record the merchant group's internal id on the directory row
	//    (openrails.merchants.permission_group_id, repurposed under #567 to hold the
	//    controlling permission-group id, keyed by slug) so issuer/credential authz
	//    can map this merchant -> its group. Returns the OpenRails merchant id the
	//    admin API key below is resource-scoped to (no default merchant; #480).
	// Record the merchant group's permission_group_id on the directory row so
	// issuer/credential authz can map this merchant -> its group (#567). #569: the
	// returned merchant id is no longer needed at mint time (identity is the group,
	// not a resource scope).
	if _, err := c.recordOwnerOrgBySlug(ctx, slug, groupID); err != nil {
		return nil, err
	}

	// 3. Mint an initial deployment admin API key only when the merchant group has
	//    none yet. The key is minted under the merchant group against the `owner`
	//    role (resolves to `merchant:*`) and scoped to the merchant resource.
	if opts.MintInitialAPIKey {
		existing, lerr := core.ListAPIKeys(ctx, MerchantType, slug)
		if lerr != nil {
			return nil, fmt.Errorf("controlplane: list admin API keys: %w", lerr)
		}
		if !anyLiveAPIKey(existing) {
			apiKey, secret, merr := core.MintAPIKeyWithOptions(ctx, MerchantType, slug, authcore.APIKeyMintOptions{
				Name: BootstrapAdminAPIKeyName,
				Role: MerchantRoleOwner,
			})
			if merr != nil {
				return nil, fmt.Errorf("controlplane: mint initial admin API key: %w", merr)
			}
			res.APIKeyMinted = true
			res.APIKeySecret = secret
			res.APIKeyID = apiKey.KeyID
			log.WithFields(log.Fields{"merchant": slug, "api_key_id": apiKey.KeyID}).
				Warn("controlplane: minted initial admin API key (secret shown once)")
		}
	}

	return res, nil
}

// anyLiveAPIKey reports whether the merchant group already has at least one
// non-revoked API key, so bootstrap does not mint a duplicate on re-run.
func anyLiveAPIKey(toks []authcore.APIKey) bool {
	for _, t := range toks {
		if t.RevokedAt == nil {
			return true
		}
	}
	return false
}

// recordOwnerOrgBySlug writes the merchant permission-group's internal id onto
// the bootstrap merchant's directory row (openrails.merchants.permission_group_id,
// repurposed under #567 to hold the controlling group id), keyed by slug, and
// returns the resolved OpenRails merchant id. openrails.* is OpenRails-owned
// control-plane state, so this is a direct, idempotent UPDATE ... RETURNING.
// There is no default merchant the row could fall back to (#480), so a missing
// directory row is an error the caller must surface (register the merchant
// before bootstrap).
func (c *ControlPlane) recordOwnerOrgBySlug(ctx context.Context, slug, groupID string) (merchant.ID, error) {
	if c.pool == nil {
		return merchant.ID{}, errors.New("controlplane: pgx pool unavailable for merchant directory update")
	}
	var idStr string
	err := c.pool.QueryRow(ctx, `
		UPDATE openrails.merchants
		   SET permission_group_id = $2,
		       updated_at      = current_timestamp
		 WHERE lower(slug) = lower($1)
		   AND deleted_at IS NULL
		RETURNING id::text
	`, slug, groupID).Scan(&idStr)
	if errors.Is(err, pgx.ErrNoRows) {
		return merchant.ID{}, fmt.Errorf("controlplane: no openrails merchant directory row for bootstrap slug %q (register the merchant before bootstrap)", slug)
	}
	if err != nil {
		return merchant.ID{}, fmt.Errorf("controlplane: record merchant group id on merchant %q: %w", slug, err)
	}
	return merchant.ParseID(idStr)
}
