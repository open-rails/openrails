// Package operator holds the standalone server's control-plane operations
// and the declaration package server builds its control plane from. The
// embedded engine has no control plane: package server composes one beside it.
package operator

import (
	"fmt"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"
	"github.com/redis/go-redis/v9"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/modules/ratelimit"
	"github.com/open-rails/openrails/server/internal/controlplane"
	"github.com/open-rails/openrails/server/internal/hostconfig"
)

// Options is the control plane a standalone server declares: its only seam
// onto the AuthKit configuration OpenRails builds. What it does not forward is
// deliberate:
//
//   - Token: the issuer is Auth.Issuer and the audience the product constant
//     billingauth.TokenAudience (#750); durations take AuthKit's defaults.
//   - Keys: resolved from Auth (inline PEM, else keys_path, else a
//     development key when allowed); no key source is injected.
//   - Roles: OpenRails' permission model (#567) is the product's schema.
//   - APIKeys: the "openrails" key prefix is a product decision.
//   - Identity providers, 2FA policy, passkeys, delegation, languages,
//     entitlements and hooks: feature areas the control plane does not use.
//   - Limiter and ClientIP: tune AuthRateLimitOverrides and the proxy fields.
type Options struct {
	// Auth is the server's identity configuration, separate from billing.
	Auth hostconfig.AuthConfig
	// Registration is AuthKit's native self-registration mode; empty is
	// closed.
	Registration iam.RegistrationMode
	// ResourceServer accepts trusted issuers' RFC 9068 access tokens (#1140).
	// ProofClaims records the DPoP proofs it accepted, shared by replicas.
	ResourceServer *hostconfig.ResourceServerConfig
	ProofClaims    *ratelimit.Windows

	// PasswordlessLogin exposes AuthKit's contact-based passwordless start and
	// confirm routes; PasswordlessAutoRegistration also lets a verified
	// unknown contact create a no-password user. Auto-registration requires
	// login and open registration; login requires an email or SMS sender.
	PasswordlessLogin            bool
	PasswordlessAutoRegistration bool
	// LocalSignIn serves sign-in to the control plane's own accounts; off,
	// people arrive with trusted issuers' access tokens.
	LocalSignIn bool

	// EmailSender and SMSSender deliver AuthKit's messages and report their
	// health (#738). Open and invite-only registration verify contacts, so
	// they need at least one.
	EmailSender authkit.EmailSender
	SMSSender   authkit.SMSSender

	// Frontend is where emailed links point (#743). Left zero, they point at
	// the issuer, which for a hosted product serves no pages.
	Frontend authkit.FrontendConfig

	// TrustedProxies and CloudflareProxies override the engine's for AuthKit's
	// client-IP resolver; only CloudflareProxies may assert CF-Connecting-IP
	// (ak#298).
	TrustedProxies    []string
	CloudflareProxies []string

	// AuthRateLimitOverrides overlays bucket limits onto AuthKit's defaults
	// (authkit.DefaultRateLimits, #743).
	AuthRateLimitOverrides map[string]authkit.RateLimit

	// Redis counts AuthKit's rate limits; nil counts them in PostgreSQL, which
	// every replica shares.
	Redis *redis.Client

	// MerchantCreation is the hosted policy for merchant names users claim
	// (or#914); nil for operator-provisioned deployments.
	MerchantCreation *controlplane.MerchantCreationConfig
}

// ControlPlaneOptions validates opts and translates them for
// controlplane.AuthKit and controlplane.New.
func ControlPlaneOptions(opts Options) ([]controlplane.Option, error) {
	if err := validate(opts); err != nil {
		return nil, err
	}
	var out []controlplane.Option
	if opts.Registration != "" {
		out = append(out, controlplane.WithRegistration(opts.Registration))
	}
	if opts.ResourceServer != nil {
		out = append(out, controlplane.WithResourceServer(*opts.ResourceServer, opts.ProofClaims))
	}
	if opts.PasswordlessLogin {
		out = append(out, controlplane.WithPasswordless(opts.PasswordlessAutoRegistration))
	}
	if opts.LocalSignIn {
		out = append(out, controlplane.WithLocalSignIn())
	}
	if opts.EmailSender != nil {
		out = append(out, controlplane.WithEmailSender(opts.EmailSender))
	}
	if opts.SMSSender != nil {
		out = append(out, controlplane.WithSMSSender(opts.SMSSender))
	}
	if opts.Frontend != (authkit.FrontendConfig{}) {
		out = append(out, controlplane.WithFrontend(opts.Frontend))
	}
	if len(opts.TrustedProxies) > 0 {
		out = append(out, controlplane.WithTrustedProxies(opts.TrustedProxies))
	}
	if len(opts.CloudflareProxies) > 0 {
		out = append(out, controlplane.WithCloudflareProxies(opts.CloudflareProxies))
	}
	if len(opts.AuthRateLimitOverrides) > 0 {
		out = append(out, controlplane.WithRateLimitOverrides(opts.AuthRateLimitOverrides))
	}
	if opts.Redis != nil {
		out = append(out, controlplane.WithRedis(opts.Redis))
	}
	if opts.MerchantCreation != nil {
		out = append(out, controlplane.WithMerchantCreation(*opts.MerchantCreation))
	}
	return out, nil
}

func validate(opts Options) error {
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
	if registers && noSender && !opts.Auth.AllowMissingSenders {
		return fmt.Errorf("control plane: registration %s requires an email or SMS sender", opts.Registration)
	}
	return nil
}

// Join makes a the engine of a standalone server with control plane cp: its
// AuthKit jobs join the engine's River fleet, the server's own hosts are kept
// from merchants' API hosts, and the embedded mount refuses. Join before the
// engine's River is bound.
func Join(a *app.App, cp *controlplane.ControlPlane, auth hostconfig.AuthConfig, frontendBaseURL string) error {
	if err := a.Runtime.AddRiverContribution(cp.RiverJobs()); err != nil {
		return fmt.Errorf("control plane: register AuthKit jobs: %w", err)
	}
	a.Runtime.ReserveAPIHosts(auth.Issuer, auth.RequestOrigin, frontendBaseURL)
	a.Standalone = true
	return nil
}

// Of returns the engine graph and control plane behind a *server.Server, for
// the standalone binary's tooling and tests; package server sets it.
var Of func(srv any) (*app.App, *controlplane.ControlPlane)
