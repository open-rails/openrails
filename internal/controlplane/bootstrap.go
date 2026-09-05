package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"
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
	// BootstrapMerchantSlug is the merchant slug (the merchant permission-group's
	// resource ref, #567) under which the admin owner + deployment admin API key
	// are seeded. It is also the slug used to locate the openrails.merchants
	// directory row: there is NO default merchant, so an empty slug is an error and
	// a matching openrails.merchants row must already exist.
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
// bootstrap merchant (#567): the merchant permission-group exists (a top-level
// child of `root`), the initial admin is
// its `owner` (auto-holds `merchant:*`), the merchant directory row records the
// merchant group's internal id, and an initial deployment admin API key is
// optionally minted under the merchant group when none exists.
//
// It runs AFTER migrations / at startup, exclusively through in-process AuthKit
// CORE calls (EnsureRootGroup / CreatePermissionGroup / Genesis().AssignGroupRole /
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

	slug := strings.ToLower(strings.TrimSpace(opts.BootstrapMerchantSlug))
	if slug == "" {
		// No default merchant (#336): bootstrap must name the merchant slug to seed.
		return nil, errors.New("controlplane: bootstrap requires a merchant slug (BootstrapMerchantSlug)")
	}

	res := &BootstrapResult{BootstrapMerchantSlug: slug}

	// 0. Ensure the singleton root group exists and the declared containment is
	//    seeded (idempotent, concurrent-boot tolerant) before creating typed groups.
	if err := EnsureRootContainment(ctx, core); err != nil {
		return nil, fmt.Errorf("controlplane: %w", err)
	}

	// 1. Ensure the merchant permission-group exists (idempotent: resolve, else
	//    create). The merchant IS the group — `type=merchant`, `resourceRef=slug`,
	//    parent=root. The initial admin (if any) is seeded as owner at creation.
	groupID, err := core.ResolveGroupIDForSlug(ctx, MerchantType, slug)
	if errors.Is(err, authkit.ErrGroupNotFound) {
		groupID, err = core.CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{
			Persona:        MerchantType,
			InstanceSlug:   slug,
			ParentPersona:  authcore.RootPersona,
			OwnerSubjectID: strings.TrimSpace(opts.InitialAdminUserID),
		})
		if err != nil {
			// #844: concurrent first-boot loser — the winner created the group
			// between resolve and create. Re-read and adopt (same pattern as
			// EnsureCustomerPermissionGroup / embedded provision); the adopted
			// path re-ensures the owner assignment below.
			id, rerr := core.ResolveGroupIDForSlug(ctx, MerchantType, slug)
			if rerr != nil {
				return nil, fmt.Errorf("controlplane: create merchant group %q: %w", slug, err)
			}
			groupID = id
		} else {
			res.MerchantGroupCreated = true
			log.WithField("merchant", slug).Info("controlplane: created merchant permission-group")
		}
	} else if err != nil {
		return nil, fmt.Errorf("controlplane: resolve merchant group %q: %w", slug, err)
	}
	if !res.MerchantGroupCreated {
		if adminID := strings.TrimSpace(opts.InitialAdminUserID); adminID != "" {
			// Group already existed (or was adopted from a race winner): ensure
			// the admin holds the owner role (idempotent).
			if aerr := core.Genesis().AssignGroupRole(ctx, MerchantType, slug, adminID, authcore.SubjectKindUser, MerchantRoleOwner); aerr != nil {
				return nil, fmt.Errorf("controlplane: assign merchant owner to initial admin: %w", aerr)
			}
			log.WithFields(log.Fields{"merchant": slug, "user_id": adminID}).
				Info("controlplane: assigned merchant owner to initial admin")
		}
	}
	res.BootstrapMerchantGroupID = groupID

	// 2. Record the merchant group's internal id on the directory row
	//    (openrails.merchants.permission_group_id, repurposed under #567 to hold the
	//    controlling permission-group id, keyed by slug) so issuer/credential authz
	//    can map this merchant -> its group. Returns the OpenRails merchant id the
	//    admin API key below is resource-scoped to (no default merchant; #480).
	// Record the merchant group's permission_group_id on the directory row so
	// issuer/credential authz can map this merchant -> its group (#567). #569: the
	// returned merchant id is no longer needed at mint time (identity is the group,
	// not a resource scope).
	if _, err := c.recordMerchantGroupBySlug(ctx, slug, groupID); err != nil {
		return nil, err
	}

	// 3. Mint an initial deployment admin API key only when explicitly
	//    requested AND this merchant group has NEVER had one before (#747).
	//    "Never had one" is the FULL key history (live and revoked) — a
	//    merchant with zero LIVE keys because an operator revoked all of them
	//    is not a first-run state, and a routine re-Bootstrap must not
	//    auto-heal that revocation with a fresh, silently-minted replacement.
	if opts.MintInitialAPIKey {
		existing, lerr := core.ListAPIKeys(ctx, MerchantType, slug)
		if lerr != nil {
			return nil, fmt.Errorf("controlplane: list admin API keys: %w", lerr)
		}
		if len(existing) == 0 {
			createdBy := strings.TrimSpace(opts.InitialAdminUserID)
			if createdBy == "" {
				createdBy, err = c.ensureBootstrapAPIKeyActor(ctx)
				if err != nil {
					return nil, err
				}
				if aerr := core.Genesis().AssignGroupRole(ctx, MerchantType, slug, createdBy, authcore.SubjectKindUser, MerchantRoleOwner); aerr != nil {
					return nil, fmt.Errorf("controlplane: assign bootstrap api-key actor owner: %w", aerr)
				}
			}
			apiKey, secret, merr := core.MintAPIKeyWithOptions(ctx, MerchantType, slug, authkit.APIKeyMintOptions{
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

// EnsureRootContainment ensures the singleton root group exists and the
// declared containment schema is seeded. Tolerant of concurrent cold boots on
// an empty DB (#844): authkit's EnsureRootGroup is check-then-create, so two
// racing nodes both observe "no root" and the loser dies on the singleton
// unique index. Re-invoking the ensure re-reads and adopts the winner's row; a
// genuine failure fails again and the original error is returned.
func EnsureRootContainment(ctx context.Context, core *authcore.Client) error {
	if _, err := core.EnsureRootGroup(ctx); err != nil {
		if _, rerr := core.EnsureRootGroup(ctx); rerr != nil {
			return fmt.Errorf("ensure root group: %w", err)
		}
	}
	if err := core.SeedPermissionGroupContainment(ctx); err != nil {
		return fmt.Errorf("seed permission-group containment: %w", err)
	}
	return nil
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
// token), and ensures that actor holds the requested merchant role.
func (c *ControlPlane) ensureMerchantAPIKeyActor(ctx context.Context, merchantSlug string) (string, error) {
	merchantSlug = strings.ToLower(strings.TrimSpace(merchantSlug))
	if merchantSlug == "" {
		return "", errors.New("controlplane: merchant slug is required")
	}
	createdBy, err := c.ensureBootstrapAPIKeyActor(ctx)
	if err != nil {
		return "", err
	}
	if err := c.Core().Genesis().AssignGroupRole(ctx, MerchantType, merchantSlug, createdBy, authcore.SubjectKindUser, MerchantRoleOwner); err != nil {
		return "", fmt.Errorf("controlplane: assign api-key actor merchant owner: %w", err)
	}
	return createdBy, nil
}

// recordMerchantGroupBySlug writes the merchant permission-group's internal id onto
// the bootstrap merchant's directory row (openrails.merchants.permission_group_id,
// repurposed under #567 to hold the controlling group id), keyed by slug, and
// returns the resolved OpenRails merchant id. openrails.* is OpenRails-owned
// control-plane state, so this is a direct, idempotent UPDATE ... RETURNING.
// There is no default merchant the row could fall back to (#480), so a missing
// directory row is an error the caller must surface (register the merchant
// before bootstrap).
func (c *ControlPlane) recordMerchantGroupBySlug(ctx context.Context, slug, groupID string) (merchant.ID, error) {
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
           AND (permission_group_id IS NULL OR permission_group_id = $2)
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
