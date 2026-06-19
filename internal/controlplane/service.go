package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
// underlying core.Service (used for in-process tenant/role/service-token bootstrap calls).
//
// HARD CUT (#469): the control plane is mandatory in standalone mode — the
// standalone binary always constructs it at boot and a construction failure is
// fatal. pkg/embedded hosts opt in via pkg/embedded/controlplane.Attach.
type ControlPlane struct {
	cfg     *config.Config
	authSvc *authhttp.Service
	// pool is the schema-aware wrapper used for OpenRails' own control-plane
	// queries (openrails.* tables). AuthKit's own profiles.* queries go through
	// authSvc, which holds the raw pool (#471).
	pool *db.Pool

	// delegatedVerifier validates browser-direct delegated access tokens for the
	// self-service surface (issue #222 browser tier). It is built from the same
	// issuer/audience/keys the control plane signs with, plus a self-service
	// permission-catalog validator. See delegated.go.
	delegatedVerifier *authhttp.Verifier

	// issuer is the control plane's token issuer (`iss`), stamped on minted tokens
	// so they verify against delegatedVerifier.
	issuer string
	// delegatedAudiences are the audiences (`aud`) stamped on minted delegated
	// access tokens. They are the control plane's accepted (expected) audiences, so
	// every minted token is accepted by delegatedVerifier (and the /v1/self gate).
	delegatedAudiences []string
}

// delegatedIssuerJWKSCacheTTL is how long the verifier treats a fetched tenant
// JWKS as fresh before a scheduled refresh (issue #259, ~hourly cadence). The
// verifier also refetches on-demand when a presented `kid` is unknown.
const delegatedIssuerJWKSCacheTTL = time.Hour

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
func New(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool) (*ControlPlane, error) {
	if cfg == nil || cfg.Auth == nil || cfg.Auth.ControlPlane == nil {
		return nil, errors.New("controlplane: auth.control_plane configuration is required (the control plane is mandatory in standalone mode, #469)")
	}
	if pool == nil {
		return nil, errors.New("controlplane: pgx pool is required")
	}
	cp := cfg.Auth.ControlPlane

	issuer := strings.TrimSpace(cp.Issuer)
	if issuer == "" {
		return nil, errors.New("controlplane: auth.control_plane.issuer is required")
	}

	// (authkit issue 60) Tenants are always a supported primitive — no tenant-mode
	// config or core flag.

	// Build the signing KeySource ONCE and inject it into the core config, so the
	// active signer the control plane MINTS with and the public keys the delegated
	// verifier TRUSTS are guaranteed to be the same key. (When Keys is left nil,
	// core.NewFromConfig auto-discovers internally and we'd have no handle on the
	// active signer; in dev it could even generate a different key on a second
	// call.) Discovery priority is unchanged: env -> /vault/auth -> dev-generated.
	keySource, err := jwtkit.NewAutoKeySource()
	if err != nil {
		return nil, fmt.Errorf("controlplane: discover signing keys: %w", err)
	}

	coreCfg := authcore.Config{
		Keys:               keySource,
		Issuer:             issuer,
		BaseURL:            issuer,
		IssuedAudiences:    []string{"openrails"},
		ExpectedAudiences:  []string{"openrails"},
		ServiceTokenPrefix: strings.TrimSpace(cp.TokenPrefix),
		Environment:        strings.TrimSpace(cfg.Env),
		PermissionCatalog:  Catalog(),
		DefaultRoles: []authcore.DefaultRole{
			// The operator role is also declared as a per-tenant DefaultRole so that
			// AssignRole can materialize it lazily even on tenants created outside the
			// bootstrap path. Bootstrap still defines+sets it explicitly.
			{Name: OperatorRole, Permissions: OperatorRolePermissions()},
		},
		// Self-hosted locked-down posture (#47 switches): no public user
		// self-registration, no public tenant onboarding/management. Embedded
		// bootstrap/core calls (CreateOrg/AssignRole/MintServiceToken) are unaffected.
		NativeUserRegistrationMode: registrationMode(!cp.PublicHosted),
		OrgRegistrationMode:        registrationMode(!cp.PublicHosted),
	}

	authSvc, err := authhttp.NewService(coreCfg)
	if err != nil {
		return nil, fmt.Errorf("controlplane: build authkit service: %w", err)
	}
	authSvc = authSvc.WithPostgres(pool)

	// Build the browser-direct delegated-access-token verifier (#222 browser
	// tier). It accepts ONLY this control plane's own delegated tokens with the
	// canonical `openrails` audience and `openrails:self:*` permissions.
	delegatedVerifier, err := newDelegatedVerifier(authSvc.Core(), strings.TrimSpace(cp.TokenPrefix))
	if err != nil {
		return nil, fmt.Errorf("controlplane: build delegated verifier: %w", err)
	}

	// Wire AuthKit's core as the verifier's enrichment + remote_application source
	// so it can lazy-load any single issuer on first use (#481).
	delegatedVerifier.WithService(authSvc.Core())

	cp2 := &ControlPlane{
		cfg:                cfg,
		authSvc:            authSvc,
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
// org/role/service token operations.
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
// intentional AuthKit route groups (self-hosted) rather than the full hosted-SaaS
// DefaultAPI surface. True unless BOTH registration axes are public.
func (c *ControlPlane) SelfHostedPosture() bool {
	if c == nil || c.cfg == nil || c.cfg.Auth == nil || c.cfg.Auth.ControlPlane == nil {
		return true
	}
	return c.cfg.Auth.ControlPlane.SelfHostedPosture()
}
