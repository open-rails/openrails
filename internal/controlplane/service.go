package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	authcore "github.com/open-rails/authkit/core"
	authhttp "github.com/open-rails/authkit/http"
	jwtkit "github.com/open-rails/authkit/jwt"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
)

// ControlPlane is OpenRails' in-process AuthKit control plane (issue #224). It
// wraps an AuthKit http.Service (which exposes selectable route groups) and its
// underlying core.Service (used for in-process org/role/API-key bootstrap calls).
//
// HARD CUT (#469): the control plane is mandatory in standalone mode — the
// standalone binary always constructs it at boot and a construction failure is
// fatal. pkg/embedded hosts opt in via pkg/embedded/controlplane.Attach.
type ControlPlane struct {
	cfg     *config.Config
	authSvc *authhttp.Service
	hosted  bool
	// pool is the schema-aware wrapper used for OpenRails' own control-plane
	// queries (openrails.* tables). AuthKit's own profiles.* queries go through
	// authSvc, which holds the raw pool (#471).
	pool *db.Pool

	// delegatedVerifier validates browser-direct delegated access tokens for the
	// self-service surface (issue #222 browser tier). It is built from the same
	// issuer/audience/keys the control plane signs with, plus a self-service
	// permissions validator. See delegated.go.
	delegatedVerifier *authhttp.Verifier

	// issuer is the control plane's token issuer (`iss`), stamped on minted tokens
	// so they verify against delegatedVerifier.
	issuer string
	// delegatedAudiences are the audiences (`aud`) stamped on minted delegated
	// access tokens. They are the control plane's accepted (expected) audiences, so
	// every minted token is accepted by delegatedVerifier (and the /v1/me gate).
	delegatedAudiences []string
}

type options struct {
	hosted bool
}

// Option configures the control plane for embedding hosts.
type Option func(*options)

// WithHostedPosture opens AuthKit registration and mounts the full AuthKit API.
// Standalone never passes this; hosted products such as openrails-saas opt in
// through pkg/embedded/controlplane.
func WithHostedPosture() Option {
	return func(o *options) {
		o.hosted = true
	}
}

func newOptions(opts []Option) options {
	var out options
	for _, opt := range opts {
		if opt != nil {
			opt(&out)
		}
	}
	return out
}

// registrationMode maps the lock flag to an AuthKit registration mode.
//
// Standalone passes locked=true, so registration is
// RegistrationModeAdminBootstrapOnly and no config/env knob can open it.
// Embedded hosts can opt into locked=false with WithHostedPosture.
func registrationMode(locked bool) authcore.RegistrationMode {
	if locked {
		return authcore.RegistrationModeAdminBootstrapOnly
	}
	return authcore.RegistrationModeOpen
}

// New builds the OpenRails-owned AuthKit control plane from config and a pgx
// pool. The pool must point at the database that holds AuthKit's `profiles.*`
// schema (in self-hosted mode this is the same database OpenRails uses). The
// caller owns the pool lifecycle.
//
// The control plane is mandatory in standalone mode (#469): every input is
// required and a failure here is a boot failure, never a silent downgrade.
func New(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool, opts ...Option) (*ControlPlane, error) {
	if cfg == nil || cfg.Auth == nil {
		return nil, errors.New("controlplane: auth.issuer is required (the control plane is mandatory in standalone mode, #469)")
	}
	if pool == nil {
		return nil, errors.New("controlplane: pgx pool is required")
	}

	issuer := strings.TrimSpace(cfg.Auth.Issuer)
	if issuer == "" {
		return nil, errors.New("controlplane: auth.issuer is required")
	}

	// (authkit issue 60) Orgs are always a supported primitive — no org-mode
	// config or core flag.

	// Build the signing KeySource ONCE and inject it into the core config, so the
	// active signer the control plane MINTS with and the public keys the delegated
	// verifier TRUSTS are guaranteed to be the same key. (When Keys is left nil,
	// core.NewFromConfig auto-discovers internally and we'd have no handle on the
	// active signer; in dev it could even generate a different key on a second
	// call.) Discovery priority is unchanged: env -> /vault/auth -> dev-generated.
	//
	// The signing key is OPTIONAL (#527/#87): when no key is discoverable (the
	// prod no-key case — dev still auto-generates), the control plane runs as a
	// pure VERIFIER instead of failing the boot. It verifies inbound host-app
	// delegated tokens and serves RBAC, but cannot MINT tokens (mint paths return
	// authkit ErrMissingSigner). This is the expected posture for a standalone
	// deployment with no login-capable users; mounting a key (env/vault) re-enables
	// minting automatically. Key presence is the enablement signal — no separate knob.
	keySource, keyErr := jwtkit.NewAutoKeySource()
	verifyOnly := false
	if keyErr != nil {
		log.WithError(keyErr).Warn("controlplane: no signing key discovered; running VERIFY-ONLY (token minting disabled)")
		keySource = nil
		verifyOnly = true
	}

	options := newOptions(opts)
	lockedRegistration := !options.hosted

	coreCfg := authcore.Config{
		Keys:              keySource,
		VerifyOnly:        verifyOnly,
		Issuer:            issuer,
		BaseURL:           issuer,
		IssuedAudiences:   []string{"openrails"},
		ExpectedAudiences: []string{"openrails"},
		APIKeyPrefix:      APIKeyPrefix,
		Environment:       strings.TrimSpace(cfg.Env),
		Permissions: Catalog(),
		// OpenRails declares NO org roles of its own (#543). Merchant-admin
		// authority comes from AuthKit's built-in `owner` role for human admins,
		// and from direct permission-scoped API keys for server-to-server
		// automation. OwnerOwnsAppResources makes the owner's apex grant cover
		// OpenRails' app-defined resource namespaces too (e.g. `merchant:*`), not
		// just `org:*` — owning the org owns the merchant (org⇄merchant 1:1, #554/#100).
		OwnerOwnsAppResources: true,
		DefaultRoles:          nil,
		// Private standalone posture: no public user self-registration, no public
		// org onboarding/management. Embedded bootstrap/core calls
		// (CreateOrg/AssignRole/MintAPIKey) are unaffected. Hosted products
		// opt in with WithHostedPosture; no config/env knob opens this in standalone.
		NativeUserRegistrationMode: registrationMode(lockedRegistration),
		OrgRegistrationMode:        registrationMode(lockedRegistration),
	}

	authSvc, err := authhttp.NewService(coreCfg)
	if err != nil {
		return nil, fmt.Errorf("controlplane: build authkit service: %w", err)
	}
	authSvc = authSvc.WithPostgres(pool)

	// Build the browser-direct delegated-access-token verifier (#222 browser
	// tier). It accepts registered delegated tokens with the canonical
	// `openrails` audience. Customer self-service tokens may be permissionless;
	// any supplied permissions must be browser-safe merchant-admin grants.
	delegatedVerifier, err := newDelegatedVerifier(authSvc.Core(), APIKeyPrefix)
	if err != nil {
		return nil, fmt.Errorf("controlplane: build delegated verifier: %w", err)
	}

	// Wire AuthKit's core as the verifier's enrichment + remote_application source
	// so it can lazy-load any single issuer on first use (#481).
	delegatedVerifier.WithService(authSvc.Core())

	cp2 := &ControlPlane{
		cfg:                cfg,
		authSvc:            authSvc,
		hosted:             options.hosted,
		pool:               db.WrapPool(pool, cfg.DB.SchemaName()),
		delegatedVerifier:  delegatedVerifier,
		issuer:             issuer,
		delegatedAudiences: []string{"openrails"},
	}

	// Load AuthKit's ACTIVE remote_applications into the multi-issuer verifier
	// (#481: standalone JWKS trust is AuthKit's remote_application registry, #74).
	// A load failure must NOT take down startup: unreachable JWKS is handled
	// lazily/fail-closed per token at verify time, and the verifier lazy-loads any
	// single issuer on first use. So we log and continue.
	if err := cp2.loadRemoteApplications(ctx); err != nil {
		log.WithError(err).Warn("controlplane: initial remote_application load failed; delegated tokens fail closed / lazy-load until next load")
	}

	return cp2, nil
}

// Core returns the underlying AuthKit core service used for in-process
// org/role/API key operations.
func (c *ControlPlane) Core() *authcore.Service {
	if c == nil {
		return nil
	}
	return c.authSvc.Core()
}

// AuthService returns the underlying AuthKit http.Service (for route mounting).
func (c *ControlPlane) AuthService() *authhttp.Service {
	if c == nil {
		return nil
	}
	return c.authSvc
}

// Pool returns the control plane's schema-aware pgx pool (the pool holding the
// openrails.* control-plane schema). Used by the tenancy lifecycle/secret-store
// service (issue #225), which owns the same OpenRails-owned control-plane state.
// SQL run on it is schema-rewritten to the configured schema (#471); call
// Pool().Raw() for the underlying pool when an API needs it verbatim. nil when
// the control plane is disabled.
func (c *ControlPlane) Pool() *db.Pool {
	if c == nil {
		return nil
	}
	return c.pool
}

// SelfHostedPosture reports whether this control plane mounts only the
// intentional AuthKit route groups (RouteCore + RoutePassword + RouteUser).
// Standalone construction never passes WithHostedPosture, so private OpenRails
// remains locked by default; embedded hosts can opt into hosted posture in code.
func (c *ControlPlane) SelfHostedPosture() bool {
	if c == nil {
		return true
	}
	return !c.hosted
}
