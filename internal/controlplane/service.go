package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	authhttp "github.com/open-rails/authkit/authhttp"
	authcore "github.com/open-rails/authkit/embedded"
	jwtkit "github.com/open-rails/authkit/jwtkit"
	"github.com/open-rails/authkit/verify"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
)

// ControlPlane is OpenRails' in-process AuthKit control plane (issue #224). It
// wraps an AuthKit http.Service (which exposes selectable route groups) and its
// underlying core.Service (used for in-process group/role/API-key bootstrap calls).
//
// HARD CUT (#469): the control plane is mandatory in standalone mode — the
// standalone binary always constructs it at boot and a construction failure is
// fatal. pkg/embedded hosts opt in via pkg/embedded/controlplane.Attach.
type ControlPlane struct {
	cfg     *config.Config
	authSvc *authhttp.Service
	// authClient is the in-process AuthKit engine the host built (client-first,
	// #142); authSvc adapts it for HTTP, Core()/the delegated verifier use it
	// directly. The server no longer vends it (.Client() was dropped).
	authClient *authcore.Client
	hosted     bool
	// pool is the schema-aware wrapper used for OpenRails' own control-plane
	// queries (openrails.* tables). AuthKit's own profiles.* queries go through
	// authSvc, which holds the raw pool (#471).
	pool *db.Pool

	// delegatedVerifier validates browser-direct delegated access tokens for the
	// self-service surface (issue #222 browser tier). It is built from the same
	// issuer/audience/keys the control plane signs with, plus a self-service
	// permissions validator. See delegated.go.
	delegatedVerifier *verify.Verifier
	// userVerifier validates first-party AuthKit user access tokens for narrow
	// OpenRails wrappers around AuthKit-generated routes. AuthKit still performs
	// the authoritative route auth inside its handler; this verifier only lets
	// OpenRails run pre-auth setup such as lazy customer-group creation.
	userVerifier *verify.Verifier

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
// Standalone passes locked=true, so registration is closed and no config/env
// knob can open it.
// Embedded hosts can opt into locked=false with WithHostedPosture.
func registrationMode(locked bool) authcore.RegistrationMode {
	if locked {
		return authcore.RegistrationModeClosed
	}
	return authcore.RegistrationModeOpen
}

// registrationVerification maps the lock flag to a verification policy: closed
// registration has nothing to verify (none); hosted (open) keeps the secure
// required default and must configure a sender.
func registrationVerification(locked bool) authcore.RegistrationVerificationPolicy {
	if locked {
		return authcore.RegistrationVerificationNone
	}
	return authcore.RegistrationVerificationRequired
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

	// (authkit issue 60) Group routes are always a supported primitive — no group-mode
	// config or core flag.

	// Build the signing KeySource ONCE and inject it into the core config, so the
	// active signer the control plane MINTS with and the public keys the delegated
	// verifier TRUSTS are guaranteed to be the same key. (When Keys is left nil,
	// core.NewFromConfig auto-discovers internally and we'd have no handle on the
	// active signer; in dev it could even generate a different key on a second
	// call.) Per authkit #231 the library reads NO env vars: discovery is
	// <path>/keys.json (Vault-mounted) -> dev-generated (development only).
	//
	// The signing key is OPTIONAL (#527/#87): when no key is discoverable (the
	// prod no-key case — dev still auto-generates), the control plane runs as a
	// pure VERIFIER instead of failing the boot. It verifies inbound host-app
	// delegated tokens and serves RBAC, but cannot MINT tokens (mint paths return
	// authkit ErrMissingSigner). This is the expected posture for a standalone
	// deployment with no login-capable users; mounting a key (env/vault) re-enables
	// minting automatically. Key presence is the enablement signal — no separate knob.
	keySource, keyErr := jwtkit.ResolveKeySource(jwtkit.DefaultAuthKeysPath, cfg.IsDev())
	verifyOnly := false
	if keyErr != nil {
		log.WithError(keyErr).Warn("controlplane: no signing key discovered; running VERIFY-ONLY (token minting disabled)")
		keySource = nil
		verifyOnly = true
	}

	options := newOptions(opts)
	lockedRegistration := !options.hosted

	coreCfg := authcore.Config{
		Keys: authcore.KeysConfig{
			Source:     keySource,
			VerifyOnly: verifyOnly,
		},
		Token: authcore.TokenConfig{
			Issuer:            issuer,
			IssuedAudiences:   []string{"openrails"},
			ExpectedAudiences: []string{"openrails"},
		},
		Frontend:    authcore.FrontendConfig{BaseURL: issuer},
		APIKeys:     authcore.APIKeysConfig{Prefix: APIKeyPrefix},
		Environment: strings.TrimSpace(cfg.Env),
		// HARD CUT (#567): OpenRails declares two FLAT top-level permission-group
		// personas under `root` — `merchant` (owner/support/viewer, `merchant:*`)
		// and `customer` (owner/member, `customer:*`). There is no merchant coupling
		// outside permission groups. Each type's `owner` role is auto-seeded
		// (= `<type>:*`), so OwnerOwnsAppResources is obsolete (every owner holds
		// its own namespace directly; the flat case needs no cross-namespace grant).
		RBAC: Groups(),
		// Private standalone posture: no public user self-registration. Embedded
		// bootstrap/core calls (CreatePermissionGroup/AssignGroupRole/MintAPIKey)
		// are unaffected. Hosted products opt in with WithHostedPosture; no
		// config/env knob opens this in standalone.
		// Verification set EXPLICITLY: authkit v0.76.0 defaults unset to
		// "required" (its doc says "none" — code wins), which refuses boot with
		// no sender. Locked registration has nothing to verify → none; hosted
		// keeps required and must configure an email/SMS sender.
		Registration: authcore.RegistrationConfig{
			NativeUserMode: registrationMode(lockedRegistration),
			Verification:   registrationVerification(lockedRegistration),
		},
	}

	// Client-first construction (#142): build the AuthKit engine, then adapt it
	// with the HTTP server. Core() / the delegated verifier hold this client.
	authClient, err := authcore.New(coreCfg, pool)
	if err != nil {
		return nil, fmt.Errorf("controlplane: build authkit client: %w", err)
	}
	authSvc, err := authhttp.NewServer(authClient)
	if err != nil {
		return nil, fmt.Errorf("controlplane: build authkit service: %w", err)
	}

	// The verifier trusts exactly what coreCfg above declared (issuer,
	// audiences, API-key prefix) — read the local values instead of an
	// accessor so mint and verify cannot drift.
	userVerifier := verify.NewVerifier(
		verify.WithSkew(5*time.Second),
		verify.WithAPIKeyPrefix(APIKeyPrefix),
		verify.WithSSRFGuard(),
	)
	if err := userVerifier.AddIssuer(issuer, []string{"openrails"}, verify.IssuerOptions{
		RawKeys: authClient.PublicKeysByKID(),
		IsLocal: true,
	}); err != nil {
		return nil, fmt.Errorf("controlplane: build user verifier: %w", err)
	}
	userVerifier.WithService(authClient)

	// Build the browser-direct delegated-access-token verifier (#222 browser
	// tier). It accepts registered delegated tokens with the canonical
	// `openrails` audience. Customer delegated self-service JWTs may be
	// permissionless; any supplied permissions are bounded by the signing remote
	// application's stored authority in AuthKit.
	delegatedVerifier, err := newDelegatedVerifier(authClient, APIKeyPrefix)
	if err != nil {
		return nil, fmt.Errorf("controlplane: build delegated verifier: %w", err)
	}

	// Wire AuthKit's core as the verifier's enrichment + remote_application source
	// so it can lazy-load any single issuer on first use (#481). *core.Service
	// satisfies verify.Enricher directly under the permission-group model (#567):
	// remote-app authority is resolved as the additive walk-up of the app's group
	// roles, kept as raw grant tokens; OpenRails gates them with its own
	// namespace-glob matcher on every credential type (#565).
	delegatedVerifier.WithService(authClient)

	cp2 := &ControlPlane{
		cfg:                cfg,
		authSvc:            authSvc,
		authClient:         authClient,
		hosted:             options.hosted,
		pool:               db.WrapPool(pool, cfg.DB.SchemaName()),
		delegatedVerifier:  delegatedVerifier,
		userVerifier:       userVerifier,
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
// group/role/API key operations.
func (c *ControlPlane) Core() authkit.Client {
	if c == nil {
		return nil
	}
	return c.authClient
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
// intentional AuthKit route groups (RoutePublic + RouteSession + RouteUser).
// Standalone construction never passes WithHostedPosture, so private OpenRails
// remains locked by default; embedded hosts can opt into hosted posture in code.
func (c *ControlPlane) SelfHostedPosture() bool {
	if c == nil {
		return true
	}
	return !c.hosted
}
