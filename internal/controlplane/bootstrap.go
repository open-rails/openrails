package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/authkit/iam"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
)

const BootstrapAdminAPIKeyName = "openrails-bootstrap-admin"

// BootstrapResult reports what the idempotent bootstrap did/ensured.
type BootstrapResult struct {
	BootstrapMerchantSlug string
	// BootstrapMerchantGroupID is the merchant permission-group's internal id (#567).
	BootstrapMerchantGroupID string
	// MerchantGroupCreated is true if this run created and bound the merchant's
	// group (false if it was already bound).
	MerchantGroupCreated bool
	// APIKeyMinted is true if an initial admin API key was minted on this run
	// (false if one already existed). When true, APIKeySecret holds the one-time
	// token — it is NOT persisted by AuthKit and cannot be recovered.
	APIKeyMinted bool
	APIKeySecret string
	APIKeyID     string
}

// BootstrapOptions parameterizes the control-plane bootstrap.
type BootstrapOptions struct {
	// BootstrapMerchantSlug names the merchant under whose group the admin
	// owner + deployment admin API key are seeded. There is NO default merchant,
	// so an empty slug is an error and the merchant must already exist.
	BootstrapMerchantSlug string

	// InitialAdminUserID, when set, is assigned the merchant `owner` role
	// (= `merchant:*`). Optional: self-hosted bootstrap may seed the admin API key
	// alone and add an admin user later.
	InitialAdminUserID string

	// MintInitialAPIKey requests minting the merchant's first deployment admin
	// API key. Defaults to false (the zero value) — Bootstrap never mints
	// unless a caller explicitly asks (#747).
	//
	// Even when true, a key is minted ONLY if this merchant group has NEVER
	// had one: eligibility is checked against the FULL key history (live AND
	// revoked), not just the currently-live count. A merchant group with zero
	// LIVE keys because an operator revoked all of them (e.g. after a
	// suspected compromise) is NOT a first-run state, and is never
	// auto-healed with a silently-minted replacement — including by a
	// standalone caller that passes true on every boot as a routine "ensure a
	// key exists" idiom. That idiom now gets exactly one mint, ever, per
	// merchant group. A deliberate replacement key after a revocation is a
	// separate, explicit operator action, not a Bootstrap side effect.
	MintInitialAPIKey bool
}

// Bootstrap idempotently ensures the OpenRails control-plane state for the
// bootstrap merchant (#567): the named merchant (which must already exist) is
// bound to its AuthKit group (a top-level child of `root`), the initial admin
// is its `owner` (auto-holds `merchant:*`), and an initial deployment admin API
// key is optionally minted under the group when none ever existed.
//
// It runs AFTER migrations / at startup, through in-process AuthKit Client calls
// — never raw AuthKit SQL or a private HTTP route. Re-running it is safe: group
// creation and owner assignment are idempotent; the API key is minted only
// when none ever existed.
func (c *ControlPlane) Bootstrap(ctx context.Context, opts BootstrapOptions) (*BootstrapResult, error) {
	if c == nil || c.Core() == nil {
		return nil, errors.New("controlplane: core service unavailable")
	}
	slug := billing.NormalizeMerchantSlug(opts.BootstrapMerchantSlug)
	if slug == "" {
		// No default merchant (#336): bootstrap must name the merchant slug to seed.
		return nil, errors.New("controlplane: bootstrap requires a merchant slug (BootstrapMerchantSlug)")
	}
	directory, err := c.directory()
	if err != nil {
		return nil, err
	}
	m, err := directory.GetBySlug(ctx, slug)
	if err != nil {
		return nil, fmt.Errorf("controlplane: no merchant %q to bootstrap: %w", slug, err)
	}
	admin := strings.TrimSpace(opts.InitialAdminUserID)
	res := &BootstrapResult{BootstrapMerchantSlug: m.Slug}

	// 1. Bind the merchant to its AuthKit group, created with the initial
	//    admin (if any) as owner, in one transaction. Concurrent boots create
	//    the same group (keyed by the merchant id); one binds it.
	if m.PermissionGroupID == "" {
		if res.MerchantGroupCreated, err = c.bindMerchantGroup(ctx, m.ID, admin); err != nil {
			return nil, fmt.Errorf("controlplane: bind merchant %q to its group: %w", slug, err)
		}
		if m, err = directory.Get(ctx, m.ID); err != nil {
			return nil, err
		}
		if res.MerchantGroupCreated {
			log.WithField("merchant", slug).Info("controlplane: created merchant permission-group")
		}
	}
	res.BootstrapMerchantGroupID = m.PermissionGroupID
	group := iam.GroupByID(m.PermissionGroupID)
	if admin != "" {
		if _, err := c.client.SetGroupRole(ctx, iam.SystemActor(), group, iam.UserSubject(admin), MerchantOwner); err != nil {
			return nil, fmt.Errorf("controlplane: assign merchant owner to initial admin: %w", err)
		}
	}

	// 2. Mint an initial deployment admin API key only when explicitly
	//    requested AND this merchant group has NEVER had one before (#747).
	//    "Never had one" is the FULL key history (live and revoked) — a
	//    merchant with zero LIVE keys because an operator revoked all of them
	//    is not a first-run state, and a routine re-Bootstrap must not
	//    auto-heal that revocation with a fresh, silently-minted replacement.
	//    The system issues it: a deployment key has no creator.
	if opts.MintInitialAPIKey {
		existing, err := c.client.ListAPIKeys(ctx, group, iam.PageRequest{Limit: 1})
		if err != nil {
			return nil, fmt.Errorf("controlplane: list admin API keys: %w", err)
		}
		if len(existing.Items) == 0 {
			created, err := c.client.CreateAPIKey(ctx, iam.SystemActor(), group, iam.NewAPIKey{Name: BootstrapAdminAPIKeyName, Role: MerchantOwner})
			if err != nil {
				return nil, fmt.Errorf("controlplane: mint initial admin API key: %w", err)
			}
			res.APIKeyMinted, res.APIKeySecret, res.APIKeyID = true, created.Secret, created.APIKey.ID
			log.WithFields(log.Fields{"merchant": slug, "api_key_id": created.APIKey.ID}).
				Warn("controlplane: minted initial admin API key (secret shown once)")
		}
	}
	return res, nil
}

// bindMerchantGroup creates an unbound merchant's group and binds the
// merchant to it in one transaction. bound is false when a concurrent caller
// bound it first.
func (c *ControlPlane) bindMerchantGroup(ctx context.Context, mid billing.MerchantID, ownerUserID string) (bound bool, err error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	group, err := c.createMerchantGroup(ctx, tx, mid, ownerUserID)
	if err != nil {
		return false, err
	}
	rows, err := gen.New(tx).BindUnboundMerchantGroup(ctx, gen.BindUnboundMerchantGroupParams{ID: mid.UUID(), GroupID: group.ID()})
	if err != nil {
		return false, err
	}
	return rows == 1, tx.Commit(ctx)
}
