package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/db/gen"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/pkg/merchant"
)

const BootstrapAdminAPIKeyName = "openrails-bootstrap-admin"
const (
	bootstrapAPIKeyActorEmail    = "openrails-bootstrap@localhost.invalid"
	bootstrapAPIKeyActorUsername = "openrailsbootstrap"
)

// BootstrapResult reports what the idempotent bootstrap did/ensured.
type BootstrapResult struct {
	BootstrapMerchantSlug string
	// BootstrapMerchantGroupID is the merchant permission-group's internal id (#567).
	BootstrapMerchantGroupID string
	// MerchantGroupCreated is true if the merchant permission-group was created on this run
	// (false if it already existed).
	MerchantGroupCreated bool
	// APIKeyMinted is true if an initial admin API key was minted on this run
	// (false if one already existed). When true, APIKeySecret holds the one-time
	// plaintext key — it is NOT persisted by AuthKit and cannot be recovered.
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
// It runs AFTER migrations / at startup, exclusively through in-process AuthKit
// Client calls — never raw AuthKit SQL or a private HTTP route. Re-running it is
// safe: group creation and owner assignment are idempotent; the API key is
// minted only when none ever existed.
func (c *ControlPlane) Bootstrap(ctx context.Context, opts BootstrapOptions) (*BootstrapResult, error) {
	if c == nil || c.Core() == nil {
		return nil, errors.New("controlplane: core service unavailable")
	}
	core := c.Core()
	slug := merchant.NormalizeSlug(opts.BootstrapMerchantSlug)
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
	//    admin (if any) as owner. Concurrent boots create the same group.
	groupID := m.PermissionGroupID
	if groupID == "" {
		groupID, res.MerchantGroupCreated, err = c.ensureMerchantGroup(ctx, m.ID, admin)
		if err != nil {
			return nil, err
		}
		if _, err := gen.New(c.pool).BindUnboundMerchantGroup(ctx, gen.BindUnboundMerchantGroupParams{ID: m.ID.UUID(), GroupID: groupID}); err != nil {
			return nil, fmt.Errorf("controlplane: bind merchant %q to its group: %w", slug, err)
		}
		if m, err = directory.Get(ctx, m.ID); err != nil {
			return nil, err
		}
		if m.PermissionGroupID != groupID {
			return nil, fmt.Errorf("controlplane: merchant %q is bound to another group", slug)
		}
		if res.MerchantGroupCreated {
			log.WithField("merchant", slug).Info("controlplane: created merchant permission-group")
		}
	}
	res.BootstrapMerchantGroupID = groupID
	ctx, group := MerchantGroupRef(ctx, groupID)
	if !res.MerchantGroupCreated && admin != "" {
		// The group already existed: ensure the admin holds the owner role.
		if err := core.OperatorAssignGroupRole(ctx, group, authkit.UserSubject(admin), MerchantRoleOwner); err != nil {
			return nil, fmt.Errorf("controlplane: assign merchant owner to initial admin: %w", err)
		}
		log.WithFields(log.Fields{"merchant": slug, "user_id": admin}).
			Info("controlplane: assigned merchant owner to initial admin")
	}

	// 2. Mint an initial deployment admin API key only when explicitly
	//    requested AND this merchant group has NEVER had one before (#747).
	//    "Never had one" is the FULL key history (live and revoked) — a
	//    merchant with zero LIVE keys because an operator revoked all of them
	//    is not a first-run state, and a routine re-Bootstrap must not
	//    auto-heal that revocation with a fresh, silently-minted replacement.
	if opts.MintInitialAPIKey {
		existing, lerr := core.ListAPIKeys(ctx, group)
		if lerr != nil {
			return nil, fmt.Errorf("controlplane: list admin API keys: %w", lerr)
		}
		if len(existing) == 0 {
			createdBy := admin
			if createdBy == "" {
				createdBy, err = c.ensureBootstrapAPIKeyActor(ctx)
				if err != nil {
					return nil, err
				}
				if aerr := core.OperatorAssignGroupRole(ctx, group, authkit.UserSubject(createdBy), MerchantRoleOwner); aerr != nil {
					return nil, fmt.Errorf("controlplane: assign bootstrap api-key actor owner: %w", aerr)
				}
			}
			apiKey, secret, merr := core.MintAPIKeyWithOptions(ctx, group, authkit.APIKeyMintOptions{
				Name: BootstrapAdminAPIKeyName,
				Role: MerchantRoleOwner,
				// AuthKit v0.62 enforces no-escalation on API-key mints. Bootstrap
				// supplies a genesis owner actor instead of weakening runtime authz.
				CreatedBy: createdBy,
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

func (c *ControlPlane) ensureBootstrapAPIKeyActor(ctx context.Context) (string, error) {
	core := c.Core()
	if core == nil {
		return "", errors.New("controlplane: core service unavailable")
	}
	if u, err := core.GetUserByUsername(ctx, bootstrapAPIKeyActorUsername); err == nil {
		return u.ID, nil
	} else if !errors.Is(err, authkit.ErrUserNotFound) && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("controlplane: lookup bootstrap api-key actor: %w", err)
	}
	u, err := core.CreateUser(ctx, bootstrapAPIKeyActorEmail, bootstrapAPIKeyActorUsername)
	if err != nil {
		if existing, gerr := core.GetUserByEmail(ctx, bootstrapAPIKeyActorEmail); gerr == nil {
			return existing.ID, nil
		}
		return "", fmt.Errorf("controlplane: create bootstrap api-key actor: %w", err)
	}
	return u.ID, nil
}

// ensureMerchantAPIKeyActor returns the genesis actor used as the CreatedBy
// fallback when MintMerchantAPIKey (#757) is called with no resolvable
// AuthKit user (operator CLI, admin API key, in-process host, delegated
// token), and ensures that actor holds the owner role in group.
func (c *ControlPlane) ensureMerchantAPIKeyActor(ctx context.Context, group authkit.GroupRef) (string, error) {
	createdBy, err := c.ensureBootstrapAPIKeyActor(ctx)
	if err != nil {
		return "", err
	}
	if err := c.Core().OperatorAssignGroupRole(ctx, group, authkit.UserSubject(createdBy), MerchantRoleOwner); err != nil {
		return "", fmt.Errorf("controlplane: assign api-key actor merchant owner: %w", err)
	}
	return createdBy, nil
}
