// Package operator wires the standalone AuthKit control plane onto an app.
// Standalone always attaches it; embedding hosts opt in with Config.ControlPlane.
// Billing's directory adapters are explicitly wired here after construction.
package operator

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/controlplane"
	billingauthkit "github.com/open-rails/openrails/internal/hostauth"
)

// AttachOptions configures the embedded AuthKit control plane: the only seam
// a host has onto the AuthKit configuration OpenRails wires (authkit.Config and
// Deps). What it does not forward is deliberate:
//
//   - Token: the issuer is Auth.Issuer and the audience the product constant
//     billingauth.TokenAudience (#750); durations take AuthKit's defaults.
//   - Keys: resolved by OpenRails from Auth (inline PEM, else keys_path, else
//     a development key when allowed); a host injects no key source.
//   - Roles: OpenRails' permission model (#567) is the product's schema.
//   - APIKeys: the "openrails" key prefix is a product decision.
//   - Identity providers, 2FA policy, passkeys, delegation, languages,
//     entitlements and hooks: feature areas the control plane does not use.
//   - Limiter and ClientIP: full replacements of the rate limiter and the
//     client-IP posture; tune AuthRateLimitOverrides and the proxy fields.
//
// Redis is the app graph's own client (#753), shared for rate limits.
type AttachOptions struct {
	// Auth is the explicit standalone identity configuration, separate from billing.
	Auth *config.AuthConfig

	// Naming overrides Auth.Naming, the site naming policy for usernames and
	// merchant names. Nil uses config.
	Naming *config.NamingConfig
	// NameAdmission is a side-effect-free username policy; merchant creation
	// charges use MerchantCreation.Admission.
	NameAdmission func(context.Context, iam.NameAdmissionRequest) error
	// Registration is AuthKit's native self-registration mode. Empty is
	// closed, the right mode for private standalone-compatible embedded hosts.
	Registration iam.RegistrationMode
	// ResourceServer accepts trusted issuers' RFC 9068 access tokens (#1140).
	ResourceServer *config.ResourceServerConfig

	// PasswordlessLogin exposes AuthKit's contact-based passwordless start and
	// confirm routes. PasswordlessAutoRegistration additionally lets a verified
	// unknown contact create a no-password user during confirmation. Both are off
	// by default. Auto-registration requires login and open registration;
	// login requires an email or SMS sender.
	PasswordlessLogin            bool
	PasswordlessAutoRegistration bool
	// LocalSignIn serves sign-in to the control plane's own accounts; off,
	// people arrive with trusted issuers' access tokens.
	LocalSignIn bool

	// EmailSender and SMSSender deliver AuthKit's messages and report their
	// health (#738; adapters/twilio provides both). Open and invite-only
	// registration verify contacts, so they need at least one. Closed
	// registration registers nobody; a sender still powers the mounted verify
	// and reset routes.
	EmailSender authkit.EmailSender
	SMSSender   authkit.SMSSender

	// Frontend is where emailed links point (#743). Left zero, they point at
	// the control plane's issuer, which for a hosted product is an API host
	// serving no pages: hosted products MUST set Frontend.BaseURL.
	Frontend authkit.FrontendConfig

	// TrustedProxies overrides cfg.TrustedProxies for AuthKit's HTTP client-IP
	// resolver. Production requires proxies or an explicit direct-peer posture.
	TrustedProxies []string
	// CloudflareProxies overrides cfg.CloudflareProxies: Cloudflare's egress
	// CIDRs, declared ONLY where Cloudflare fronts the origin (ak#298). These
	// peers alone may assert CF-Connecting-IP.
	CloudflareProxies []string
	// DirectPeerIP declares there is no reverse proxy in front of AuthKit.
	// It cannot be combined with configured or host-supplied trusted proxies.
	DirectPeerIP bool

	// AuthRateLimitOverrides overlays bucket limits onto AuthKit's defaults
	// (authkit.DefaultRateLimits, #743); every other bucket keeps its default.
	AuthRateLimitOverrides map[string]authkit.RateLimit

	// MerchantCreation declares the hosted policy for merchant names claimed by
	// users (or#914): reserved names (billing.ReservedMerchantSlugs +
	// cfg.ReservedSlugs), the creation pattern and the cfg.Admission cost gate
	// apply to ProvisionMerchant with an OwnerUserID and to merchant renames.
	// Leave nil for operator-provisioned (manifest/bootstrap) deployments.
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
func Attach(ctx context.Context, a *app.App, cfg *config.Config, auth *config.AuthConfig, injectedPool *pgxpool.Pool) error {
	return AttachWithOptions(ctx, a, cfg, injectedPool, AttachOptions{Auth: auth})
}

// AttachWithOptions builds the control plane with host-selected posture.
func AttachWithOptions(ctx context.Context, a *app.App, cfg *config.Config, injectedPool *pgxpool.Pool, opts AttachOptions) error {
	if a == nil || cfg == nil {
		return fmt.Errorf("control plane: app and config are required")
	}
	if err := validateAttachOptions(opts); err != nil {
		return err
	}
	if a.Runtime == nil {
		return fmt.Errorf("control plane: runtime is required")
	}
	if err := a.Runtime.CheckRiverConfigurable(); err != nil {
		return fmt.Errorf("control plane: attach before River initialization; a host fleet must compose AuthKit workers before binding its client")
	}

	// The control plane needs a pgx pool over the database holding AuthKit's
	// profiles.* schema. Reuse an injected pool when provided, else create one.
	pool := injectedPool
	ownedPool := false
	if pool == nil {
		p, err := pgxpool.New(ctx, config.DBConnectionString(cfg.DB))
		if err != nil {
			return fmt.Errorf("control plane: build pgx pool: %w", err)
		}
		pool = p
		ownedPool = true
	} else if a.Runtime != nil && a.Runtime.DB != nil && pool == a.Runtime.DB.Pool() {
		// Authority lookups can follow a pinned billing read. Keep their pool
		// independent so a one-connection host cannot deadlock itself.
		separate, err := pgxpool.NewWithConfig(ctx, pool.Config())
		if err != nil {
			return fmt.Errorf("control plane: build independent authority pool: %w", err)
		}
		pool = separate
		ownedPool = true
	}

	var cpOpts []controlplane.Option
	if opts.Naming != nil {
		cpOpts = append(cpOpts, controlplane.WithNaming(*opts.Naming))
	}
	if opts.NameAdmission != nil {
		cpOpts = append(cpOpts, controlplane.WithNameAdmission(opts.NameAdmission))
	}
	if opts.Registration != "" {
		cpOpts = append(cpOpts, controlplane.WithRegistration(opts.Registration))
	}
	if opts.ResourceServer != nil {
		cpOpts = append(cpOpts, controlplane.WithResourceServer(*opts.ResourceServer))
	}
	if opts.PasswordlessLogin {
		cpOpts = append(cpOpts, controlplane.WithPasswordless(opts.PasswordlessAutoRegistration))
	}
	if opts.LocalSignIn {
		cpOpts = append(cpOpts, controlplane.WithLocalSignIn())
	}
	if opts.EmailSender != nil {
		cpOpts = append(cpOpts, controlplane.WithEmailSender(opts.EmailSender))
	}
	if opts.SMSSender != nil {
		cpOpts = append(cpOpts, controlplane.WithSMSSender(opts.SMSSender))
	}
	if opts.Frontend != (authkit.FrontendConfig{}) {
		cpOpts = append(cpOpts, controlplane.WithFrontend(opts.Frontend))
	}
	if opts.DirectPeerIP {
		cpOpts = append(cpOpts, controlplane.WithDirectPeerIP())
	}
	if len(opts.TrustedProxies) > 0 {
		cpOpts = append(cpOpts, controlplane.WithTrustedProxies(opts.TrustedProxies))
	}
	if len(opts.CloudflareProxies) > 0 {
		cpOpts = append(cpOpts, controlplane.WithCloudflareProxies(opts.CloudflareProxies))
	}
	if len(opts.AuthRateLimitOverrides) > 0 {
		cpOpts = append(cpOpts, controlplane.WithRateLimitOverrides(opts.AuthRateLimitOverrides))
	}
	// #753: reuse the app graph's OWN Redis client (the same client
	// Deps.Redis / app.BootstrapOptions.Redis produced) for
	// AuthKit's shared rate limits. Without it the control plane requires
	// auth.allow_memory (per-process limits, one replica).
	if a.RedisClient != nil {
		cpOpts = append(cpOpts, controlplane.WithRedis(a.RedisClient))
	}
	if opts.MerchantCreation != nil {
		cpOpts = append(cpOpts, controlplane.WithMerchantCreation(*opts.MerchantCreation))
	}
	cp, err := controlplane.New(ctx, cfg, opts.Auth, pool, cpOpts...)
	if err != nil {
		if ownedPool {
			pool.Close()
		}
		return fmt.Errorf("build control plane: %w", err)
	}

	if err := a.Runtime.AddRiverContribution(cp.RiverJobs()); err != nil {
		cp.Close()
		if ownedPool {
			pool.Close()
		}
		return fmt.Errorf("control plane: register maintenance: %w", err)
	}

	if ownedPool {
		a.SetControlPlane(cp, pool)
	} else {
		a.SetControlPlane(cp, nil)
	}

	if a.Runtime != nil {
		a.Runtime.ReserveAPIHosts(opts.Auth.Issuer, opts.Auth.RequestOrigin, opts.Frontend.BaseURL)
		// The standalone control plane explicitly opts billing into AuthKit's
		// public directory API. Preserve independently injected host adapters.
		directory := billingauthkit.NewDirectory(cp.Core())
		if a.Runtime.EmailService != nil {
			a.Runtime.EmailService.SetDefaultUserDirectory(directory)
		}
		if a.Runtime.WebhookDispatcher != nil && a.Runtime.WebhookDispatcher.ProfileRepo == nil {
			a.Runtime.WebhookDispatcher.ProfileRepo = directory
		}
	}
	return nil
}

func validateAttachOptions(opts AttachOptions) error {
	if err := controlplane.ValidateRegistrationMode(opts.Registration); err != nil {
		return err
	}
	registers := opts.Registration != "" && opts.Registration != iam.RegistrationModeClosed
	if (registers || opts.PasswordlessLogin) && !opts.LocalSignIn {
		return fmt.Errorf("control plane: registration and passwordless login need local sign-in")
	}
	if opts.PasswordlessAutoRegistration && !opts.PasswordlessLogin {
		return fmt.Errorf("control plane: passwordless auto-registration requires passwordless login")
	}
	if opts.PasswordlessAutoRegistration && opts.Registration != iam.RegistrationModeOpen {
		return fmt.Errorf("control plane: passwordless auto-registration requires registration open")
	}
	noSender := opts.EmailSender == nil && opts.SMSSender == nil
	if opts.PasswordlessLogin && noSender {
		return fmt.Errorf("control plane: passwordless login requires an email or SMS sender")
	}
	if registers && noSender && (opts.Auth == nil || !opts.Auth.AllowMissingSenders) {
		return fmt.Errorf("control plane: registration %s requires an email or SMS sender", opts.Registration)
	}
	return nil
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
// Call it AFTER migrations have run (so billing.merchants and profiles.* exist) and
// at startup. Safe to re-run. This was App.RunControlPlaneBootstrap before #284.
func RunBootstrap(ctx context.Context, a *app.App, opts BootstrapOptions) (*BootstrapResult, error) {
	cp := Get(a)
	if cp == nil {
		return nil, fmt.Errorf("control plane bootstrap: no control plane attached (call Attach first)")
	}
	return cp.Bootstrap(ctx, opts)
}
