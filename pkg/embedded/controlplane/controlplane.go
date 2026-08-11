// Package controlplane wires the OpenRails AuthKit control plane onto an app
// graph (#284). The embedded CORE (pkg/embedded.New -> app.BootstrapWithOptions ->
// internal/app) deliberately does NOT import internal/controlplane (and through
// it AuthKit): embedded hosts are host-authenticated and opt in by importing
// THIS package, which is what pulls the control plane (and AuthKit) onto a
// host's graph. The STANDALONE binary, by contrast, always attaches the control
// plane (#469): there is no verifier-only standalone mode, and an Attach
// failure is fatal at boot.
//
// The control plane is held on app.App as `any` (App.ControlPlane); this package
// builds the concrete *controlplane.ControlPlane, attaches it via
// App.SetControlPlane, and recovers it with Get for the standalone gin server.
package controlplane

import (
	"context"
	"fmt"
	"reflect"

	"github.com/jackc/pgx/v5/pgxpool"
	authcore "github.com/open-rails/authkit/embedded"
	"github.com/open-rails/authkit/ratelimit"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
)

// AttachOptions configures the embedded AuthKit control-plane seam.
//
// # Field-by-field forwarding audit (#743)
//
// AttachOptions is the ONLY seam an embedding host has onto the AuthKit dials
// OpenRails' control plane wires (authcore.Config + authhttp.NewServer's
// functional options). Below is every dial on both surfaces and whether
// AttachOptions forwards it, so the next missing forward is a deliberate
// decision recorded here, not a silent gap rediscovered by a 404 in
// production (#743's Frontend-links finding was exactly that).
//
// authcore.Config (the AuthKit engine):
//   - Token (Issuer/Audiences/durations/SessionMaxPerUser): NOT forwarded.
//     Issuer comes from cfg.Auth.Issuer (already host-configurable at the
//     config layer); the audience is the OpenRails product constant
//     billingauth.TokenAudience, not a per-host dial (#750). Token
//     durations/session caps have no AttachOptions knob yet — an open gap,
//     not yet requested by a host.
//   - Frontend: FORWARDED (this issue) via AttachOptions.Frontend.
//   - Registration: PARTIALLY forwarded. OpenRails computes native registration
//     from HostedPosture (open+required vs closed+none), while the independent
//     passwordless login and auto-registration policies are explicit opt-ins.
//   - Keys: NOT forwarded. Signing-key resolution is OpenRails' own
//     responsibility (resolveControlPlaneKeySource: cfg.Auth.* inline
//     material, else vault keys.json, else dev-ephemeral) — a host has no
//     business injecting a KeySource or flipping VerifyOnly.
//   - Identity (OAuth/OIDC providers): NOT forwarded. No AttachOptions
//     provider list exists; hosted external-login is a future issue, not a
//     silently-defaulted feature.
//   - APIKeys.Prefix: NOT forwarded — the "openrails" API-key brand prefix
//     (APIKeyPrefix) is an OpenRails product decision, not host-configurable.
//   - APIKeys.MaxTTL: NOT forwarded — no cap today; open gap, not a decision.
//   - TwoFactor / Passkeys: NOT forwarded — neither feature area is wired
//     into the control plane's mounted routes for embedded hosts yet.
//   - RBAC: NOT forwarded — OpenRails' own Groups() permission-group catalog
//     IS the product's authorization schema (#567); a host cannot override
//     its own persona/permission model.
//   - Environment: forwarded, but via cfg.Env at the config layer (already a
//     top-level host-set value), not a separate AttachOptions field.
//   - Schema / SolanaNetwork: NOT AttachOptions concerns — Schema follows
//     cfg.DB.SchemaName() at the DB-wiring layer; SolanaNetwork derives from
//     Environment (authcore's own default), never overridden today.
//
// authcore.Option (engine-construction functional options):
//   - WithEmailSender / WithSMSSender: FORWARDED via AttachOptions.EmailSender
//     / SMSSender (#738).
//   - WithEphemeralStore / embedded.WithRedis: FORWARDED (#753) — AttachWithOptions
//     wires the app graph's OWN Redis client (app.App.RedisClient, the same
//     client pkg/embedded.Options.Redis produced) into
//     controlplane.WithRedis whenever the app has one, rather than adding a
//     redundant AttachOptions field for state the app graph already holds.
//     authhttp's own construction-time validate() hard-fails when
//     Environment is not dev-like and no Redis was supplied, so a hosted
//     attach in a non-development environment now requires the app to have
//     Redis configured (pkg/embedded.Options.Redis / app.BootstrapOptions.Redis);
//     the resulting error names the requirement.
//   - WithEntitlements / WithClickHouse: NOT forwarded — unrelated feature
//     areas OpenRails' control plane does not use.
//
// authhttp.Option (HTTP-layer construction options, authhttp.NewServer):
//   - WithTrustedProxies: FORWARDED (this issue) via AttachOptions.TrustedProxies.
//   - WithClientIPFunc: NOT forwarded — superseded by WithTrustedProxies (the
//     "normal knob", per its own doc); exposing a raw ClientIPFunc would let a
//     host inject arbitrary, unaudited client-IP logic. Advanced/test-only by
//     authhttp's own doc.
//   - WithRateLimiter / WithoutRateLimiter: NOT forwarded — full-policy-replacement
//     footguns ("ADVANCED/TEST ONLY" per authhttp's own doc); forwarding them
//     would let a host disable rate limiting entirely instead of tuning one
//     bucket. Use AuthRateLimitOverrides for per-bucket tuning instead.
//   - WithRateLimitOverrides: FORWARDED (#743 task 3, unblocked by authkit
//     v0.79.0's merge-style override API) via AttachOptions.AuthRateLimitOverrides.
//   - WithRedis: NOT separately forwarded — the HTTP layer automatically
//     reuses whatever Redis client the engine was wired with above (authkit
//     v0.79.0, #210); a host never needs to pass Redis twice.
//   - WithLanguageConfig: NOT forwarded — i18n is not wired into
//     AttachOptions; no host has asked for it yet.
type AttachOptions struct {
	// HostedPosture opens AuthKit registration and mounts the full AuthKit API.
	// Leave false for private standalone-compatible embedded hosts.
	HostedPosture bool

	// PasswordlessLogin exposes AuthKit's contact-based passwordless start and
	// confirm routes. PasswordlessAutoRegistration additionally lets a verified
	// unknown contact create a no-password user during confirmation. Both are off
	// by default. Auto-registration requires login and HostedPosture; login
	// requires at least one non-nil email or SMS sender.
	PasswordlessLogin            bool
	PasswordlessAutoRegistration bool

	// EmailSender / SMSSender are host-owned verification senders threaded into
	// the AuthKit engine (#738). The types (and the VerificationMessage payload a
	// sender receives) are the github.com/open-rails/authkit/embedded aliases, so
	// an external host implements them without reaching into authkit internals.
	//
	// Hosted posture keeps registration verification Required, and authkit fails
	// construction loudly when that policy has no sender — so HostedPosture
	// requires at least one of these. Self-hosted posture ignores them for
	// registration (closed, nothing to verify); a supplied sender still powers
	// the mounted self-service verify/reset routes.
	EmailSender authcore.EmailSender
	SMSSender   authcore.SMSSender

	// Frontend overrides AuthKit's Frontend config — the host-owned frontend
	// routes used to build absolute emailed links (verification, password
	// reset, ...). #743: left zero, BaseURL pins at the control-plane's own
	// issuer, which for a hosted product is an API host that serves no pages
	// — emailed verify/reset links 404 (the reported bug). Hosted products
	// MUST set at least Frontend.BaseURL to their product frontend's origin;
	// VerifyPath/PasswordResetPath/etc default per authcore's own rules
	// (VerifyPath->"/verify", PasswordResetPath->"/reset", ...) when left
	// blank. The type is authkit/embedded's own exported FrontendConfig — no
	// OpenRails-owned wrapper needed, matching EmailSender/SMSSender above.
	Frontend authcore.FrontendConfig

	// TrustedProxies lists the CIDRs of reverse proxies/load balancers in
	// front of this deployment. When set, AuthKit's HTTP server derives the
	// client IP for its per-client registration/login rate limiter from
	// forwarded headers ONLY when the immediate peer is one of these CIDRs
	// (#743); otherwise (the default, empty) it uses the direct TCP peer,
	// which behind any LB collapses every client into one shared bucket.
	// Coordinate with #746's engine-wide trusted-proxy resolver: this is
	// deliberately a standalone AttachOptions field, not a shared config key,
	// until that unification lands.
	TrustedProxies []string

	// AuthRateLimitOverrides overlays bucket-specific limits onto AuthKit's
	// built-in rate-limit defaults (#743 task 3, unblocked by authkit
	// v0.79.0's authhttp.WithRateLimitOverrides). Keys are AuthKit's exported
	// bucket names (authhttp.RLPasswordLogin, authhttp.RLAuthRegister, ...);
	// a host tunes only the buckets it names, every other bucket keeps
	// AuthKit's default. The type is authkit/ratelimit's own exported Limit —
	// no OpenRails wrapper needed, matching Frontend/EmailSender above.
	AuthRateLimitOverrides map[string]ratelimit.Limit

	// MerchantCreation opts the merchant persona into authkit's generated
	// instance-creation path (ak#263, or#914). With it set,
	// ProvisionMerchant routes USER-claimed slugs (OwnerUserID != "") through
	// CreateInstanceForSubject — slug pattern, reserved slugs
	// (merchant.ReservedHostedSlugs + cfg.ReservedSlugs) and the
	// cfg.Admission cost gate all apply automatically — and authkit mounts
	// POST /merchant with its own per-IP/per-user velocity limits. Hosted
	// "registration is provisioning" products should set this; leave nil for
	// operator-provisioned (manifest/bootstrap) deployments.
	MerchantCreation *MerchantCreationConfig
}

// MerchantCreationConfig is the nameable alias for
// controlplane.MerchantCreationConfig (the BootstrapOptions alias pattern —
// internal/controlplane cannot be imported outside this module).
type MerchantCreationConfig = controlplane.MerchantCreationConfig

// Get recovers the concrete *controlplane.ControlPlane attached to the app, or
// nil when no control plane has been attached (an embedded host that never
// called Attach) or the field holds some other type.
func Get(a *app.App) *controlplane.ControlPlane {
	if a == nil {
		return nil
	}
	cp, _ := a.ControlPlane.(*controlplane.ControlPlane)
	return cp
}

// Attach builds the OpenRails-owned AuthKit control plane (#224) and attaches it
// to the app. Construction always happens (#469: there is no disabled state);
// any failure — missing issuer, bad keys, unreachable DB — is returned so the
// standalone boot path can exit non-zero. injectedPool, when non-nil, is reused
// as the control-plane pool; otherwise Attach creates an OpenRails-owned pool
// whose lifecycle App.Close manages.
func Attach(ctx context.Context, a *app.App, cfg *config.Config, injectedPool *pgxpool.Pool) error {
	return AttachWithOptions(ctx, a, cfg, injectedPool, AttachOptions{})
}

// AttachWithOptions builds the control plane with host-selected posture.
func AttachWithOptions(ctx context.Context, a *app.App, cfg *config.Config, injectedPool *pgxpool.Pool, opts AttachOptions) error {
	if a == nil || cfg == nil {
		return fmt.Errorf("control plane: app and config are required")
	}
	if err := validateAttachOptions(opts); err != nil {
		return err
	}

	// The control plane needs a pgx pool over the database holding AuthKit's
	// profiles.* schema. Reuse an injected pool when provided, else create one.
	pool := injectedPool
	ownedPool := false
	if pool == nil {
		p, err := pgxpool.New(ctx, cfg.DB.GetConnectionString())
		if err != nil {
			return fmt.Errorf("control plane: build pgx pool: %w", err)
		}
		pool = p
		ownedPool = true
	}

	var cpOpts []controlplane.Option
	if opts.HostedPosture {
		cpOpts = append(cpOpts, controlplane.WithHostedPosture())
	}
	if opts.PasswordlessLogin {
		cpOpts = append(cpOpts, controlplane.WithPasswordless(opts.PasswordlessAutoRegistration))
	}
	if !isNil(opts.EmailSender) {
		cpOpts = append(cpOpts, controlplane.WithEmailSender(opts.EmailSender))
	}
	if !isNil(opts.SMSSender) {
		cpOpts = append(cpOpts, controlplane.WithSMSSender(opts.SMSSender))
	}
	if opts.Frontend != (authcore.FrontendConfig{}) {
		cpOpts = append(cpOpts, controlplane.WithFrontend(opts.Frontend))
	}
	if len(opts.TrustedProxies) > 0 {
		cpOpts = append(cpOpts, controlplane.WithTrustedProxies(opts.TrustedProxies))
	}
	if len(opts.AuthRateLimitOverrides) > 0 {
		cpOpts = append(cpOpts, controlplane.WithRateLimitOverrides(opts.AuthRateLimitOverrides))
	}
	// #753: reuse the app graph's OWN Redis client (the same client
	// pkg/embedded.Options.Redis / app.BootstrapOptions.Redis produced) as
	// AuthKit's ephemeral store, rather than adding a redundant AttachOptions
	// field for state the app already holds. Nil (no Redis configured) is
	// passed through unchanged — authhttp.NewServer below fails construction
	// with a clear error naming the Redis requirement whenever Environment
	// is non-development and no Redis was wired.
	if a.RedisClient != nil {
		cpOpts = append(cpOpts, controlplane.WithRedis(a.RedisClient))
	}
	if opts.MerchantCreation != nil {
		cpOpts = append(cpOpts, controlplane.WithMerchantCreation(*opts.MerchantCreation))
	}
	cp, err := controlplane.New(ctx, cfg, pool, cpOpts...)
	if err != nil {
		if ownedPool {
			pool.Close()
		}
		return fmt.Errorf("build control plane: %w", err)
	}

	if ownedPool {
		a.SetControlPlane(cp, pool)
	} else {
		a.SetControlPlane(cp, nil)
	}

	// or#914: install the rename-forwarding seam so directory slug resolution
	// (webhook routes, GetBySlug) follows ak#264 group renames. Covers both
	// wiring orders: a merchants service armed LATER picks the seam up from
	// the runtime (ArmMerchantsService); one armed EARLIER is wired here.
	if a.Runtime != nil {
		resolver := cp.MerchantGroupSlugResolver()
		a.Runtime.MerchantGroupResolver = resolver
		if a.Runtime.Merchants != nil {
			a.Runtime.Merchants.WithGroupSlugResolver(resolver)
		}
	}
	return nil
}

func validateAttachOptions(opts AttachOptions) error {
	if opts.PasswordlessAutoRegistration && !opts.PasswordlessLogin {
		return fmt.Errorf("control plane: passwordless auto-registration requires passwordless login")
	}
	if opts.PasswordlessAutoRegistration && !opts.HostedPosture {
		return fmt.Errorf("control plane: passwordless auto-registration requires hosted posture")
	}
	if opts.PasswordlessLogin && isNil(opts.EmailSender) && isNil(opts.SMSSender) {
		return fmt.Errorf("control plane: passwordless login requires an email or SMS sender")
	}
	return nil
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// BootstrapOptions is the nameable, externally-constructible alias for
// controlplane.BootstrapOptions (#747, the embed/provision.go alias pattern):
// internal/controlplane cannot be imported outside this module, so before this
// alias existed RunBootstrap took a parameter no external host could construct
// — the ONLY caller was this repo's own test harness.
type BootstrapOptions = controlplane.BootstrapOptions

// BootstrapResult is the nameable alias for controlplane.BootstrapResult; see
// BootstrapOptions.
type BootstrapResult = controlplane.BootstrapResult

// RunBootstrap idempotently bootstraps the OpenRails-owned AuthKit control plane
// (#224): ensures the bootstrap authority, OpenRails operator role, the
// openrails.* permission catalog, and — only when opts.MintInitialAPIKey is
// true — an initial operator API key. Calling it without an attached control
// plane is a wiring error (#469: the standalone always attaches one first).
//
// #747: opts is forwarded VERBATIM — RunBootstrap used to force
// MintInitialAPIKey to true regardless of what the caller passed, so "defaults
// to true" actually meant "always true". A caller that wants a key must now
// ask for one explicitly, every time, and even then Bootstrap mints only when
// this merchant group has never had a key before (see BootstrapOptions'
// MintInitialAPIKey doc) — an operator who revokes every key after a
// suspected compromise is never silently re-issued a fresh one on the next
// routine boot.
//
// Call it AFTER migrations have run (so openrails.merchants and profiles.* exist) and
// at startup. Safe to re-run. This was App.RunControlPlaneBootstrap before #284.
func RunBootstrap(ctx context.Context, a *app.App, opts BootstrapOptions) (*BootstrapResult, error) {
	cp := Get(a)
	if cp == nil {
		return nil, fmt.Errorf("control plane bootstrap: no control plane attached (call Attach first)")
	}
	return cp.Bootstrap(ctx, opts)
}
