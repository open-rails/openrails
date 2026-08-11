// Package authkit is the OPT-IN AuthKit verifier adapter for embedded hosts
// (#284). The embedded CORE (pkg/embedded.New -> app.BootstrapWithOptions ->
// internal/app) deliberately does NOT import AuthKit. Hosts (and the OpenRails
// standalone binary) that want the AuthKit JWT-verifier auth boundary opt in
// here and pass the result at HTTP mount time.
//
// Importing this package is what pulls github.com/open-rails/authkit onto a
// host's dependency graph; pkg/embedded itself stays AuthKit-free.
//
// # Two ways in
//
// Each authenticator comes in two constructors, differing ONLY in where the
// verifier comes from:
//
//   - New…Authenticator(v, …) takes the host's OWN verifier. Use this whenever
//     the host already has one — an in-process AuthKit, a control plane it
//     embeds, a verifier configured with the host's credential chain. The
//     request is verified exactly the way the host verifies every other
//     request (VerifyRequest: API-key branch, 2FA-enrollment gate,
//     delegated-issuer enrichment), so billing cannot end up with a weaker
//     credential check than the rest of the host.
//   - NewVerifier…Authenticator(issuers, aud, …) builds a JWKS verifier over a
//     REMOTE issuer allowlist, fetching keys from each issuer's
//     /.well-known/jwks.json. A host embedding the control plane in-process
//     must NOT verify its own tokens this way (mint and verify would drift
//     across an HTTP fetch of its own keys); that is what the injecting
//     constructor is for.
//
// # The admission seam (#918)
//
// Both families take an Admission veto and, on the delegated side, a
// request-scoped PermissionResolver. They exist because "principal = f(token)"
// is not enough for a privileged surface: JWT verification is stateless, so a
// banned or deleted subject keeps a valid token until it expires, and a grant
// like "is this user a billing admin?" is a live DB read, not a claim. Hosts
// that had to hand-roll a bridge for those two decisions (doujins #803) plug
// them in here instead. OpenRails never learns what a host's admission or
// grant authority IS — the host injects it, and no OpenRails package imports
// authkit's client to go looking.
package authkit

import (
	"context"
	"fmt"
	"net/http"

	"github.com/open-rails/authkit/verify"

	"github.com/open-rails/openrails/internal/auth"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Verifier is a host's own AuthKit verifier. authkit's *verify.Verifier
// satisfies it, so a host passes the verifier it already uses.
type Verifier interface {
	VerifyRequest(r *http.Request) (verify.Claims, error)
}

// Admission is the host's per-request veto, evaluated AFTER the credential
// verifies and BEFORE the principal is built. Return a non-nil error to
// refuse: OpenRails logs the cause and answers 401, never echoing the host's
// message to the client.
//
// This is where a liveness gate belongs while AuthKit's verify stays stateless
// (ak#267):
//
//	authkit.WithAdmission(func(ctx context.Context, _ *http.Request, cl verify.Claims) error {
//		allowed, err := core.IsUserAllowed(ctx, cl.UserID) // the host's live authority
//		if err != nil {
//			return fmt.Errorf("liveness lookup: %w", err) // fail closed
//		}
//		if !allowed {
//			return errors.New("user is not allowed")
//		}
//		return nil
//	})
type Admission func(ctx context.Context, r *http.Request, cl verify.Claims) error

// PermissionResolver resolves the principal's OpenRails permissions from the
// verified claims AND the live request — the general form of
// WithRolePermissions, for grants that are neither role-derived nor
// context-free (a DB-backed merchant-admin check scoped to the admin path).
// An error refuses the request; a host that prefers a soft fallback returns
// the reduced set with a nil error.
type PermissionResolver func(ctx context.Context, r *http.Request, cl verify.Claims) ([]string, error)

// Option customizes NewAuthenticator / NewVerifierAuthenticator.
type Option func(*options)

type options struct {
	admit          Admission
	omitTokenRoles bool
}

// WithUserAdmission installs the per-request admission veto on the
// (non-delegated) user authenticator.
func WithUserAdmission(a Admission) Option {
	return func(o *options) { o.admit = a }
}

// WithoutTokenRoles drops the token's role snapshot from the resulting
// billingauth.UserContext. A JWT role list is stale for the whole token
// lifetime; a host that authorizes from a live authority instead (doujins
// #774) omits it rather than passing a snapshot nothing should read.
func WithoutTokenRoles() Option {
	return func(o *options) { o.omitTokenRoles = true }
}

// DelegatedOption customizes NewDelegatedAuthenticator /
// NewVerifierDelegatedAuthenticator.
type DelegatedOption func(*delegatedOptions)

type delegatedOptions struct {
	rolePermissions func(roles []string) []string
	resolver        PermissionResolver
	admit           Admission
	merchantSlug    string
	issuer          string
}

// WithRolePermissions overrides the canonical permissions.ForRoles preset
// with the host's own role→permission mapping. The mapping's output feeds
// DelegatedPrincipal.Permissions verbatim (the embedding host is trusted,
// #564), and composes with billingauth.NewDelegatedGate — wildcard grants
// like permissions.MerchantAll are expanded by billingauth.HasPermission. A
// mapper returning nil grants nothing beyond the grant-free /v1/me surface.
//
// Use WithPermissionResolver instead when the grant depends on anything other
// than the token's roles; setting both is a construction error.
func WithRolePermissions(mapper func(roles []string) []string) DelegatedOption {
	return func(o *delegatedOptions) { o.rolePermissions = mapper }
}

// WithPermissionResolver replaces the role→permission mapping with a
// request-scoped resolver — the seam for a live, DB-backed grant:
//
//	authkit.WithPermissionResolver(func(ctx context.Context, r *http.Request, cl verify.Claims) ([]string, error) {
//		perms := []string{permissions.CustomerAll}
//		if isMerchantAdminRequest(r) { // keep the hot self path free of lookups
//			ok, err := hostAuthority.MayInspectBilling(ctx, cl.UserID)
//			if err != nil {
//				return nil, err
//			}
//			if ok {
//				perms = append(perms, permissions.MerchantAll)
//			}
//		}
//		return perms, nil
//	})
//
// It runs AFTER the admission veto, so an inadmissible subject never reaches
// the lookup. Resolve from a LIVE authority, not cl.Roles: the token's role
// snapshot is stale for the token's lifetime (doujins #774).
func WithPermissionResolver(r PermissionResolver) DelegatedOption {
	return func(o *delegatedOptions) { o.resolver = r }
}

// WithAdmission installs the per-request admission veto on the delegated
// authenticator. It covers EVERY delegated principal, including the /v1/me
// self surface — that surface is billing too.
func WithAdmission(a Admission) DelegatedOption {
	return func(o *delegatedOptions) { o.admit = a }
}

// WithMerchantSlug stamps the bound merchant's slug on every principal. It is
// load-bearing on the customer-treasury surface: or#916 lets a principal
// holding the full merchant:* grant address the merchant's OWN treasury
// account by naming it, and without the slug the merchant uuid is the only
// address that resolves.
func WithMerchantSlug(slug string) DelegatedOption {
	return func(o *delegatedOptions) { o.merchantSlug = slug }
}

// WithIssuer overrides the audit issuer stamped on the principal (default: the
// verified token's own `iss`). A host whose customers rows are keyed to one
// canonical issuer — openrails.SelfIssuer, what EnsureCustomerID and a legacy
// migration write — stamps that instead, so its principals and its rows agree.
func WithIssuer(issuer string) DelegatedOption {
	return func(o *delegatedOptions) { o.issuer = issuer }
}

// NewAuthenticator builds a framework-neutral billingauth.Authenticator over
// the HOST's own verifier: the credential is checked exactly the way the host
// checks every other request. Pass the result as
// gin.MountOptions.Authenticator or a host Gate input.
func NewAuthenticator(v Verifier, opts ...Option) (billingauth.Authenticator, error) {
	if v == nil {
		return nil, fmt.Errorf("authenticator: verifier is required (pass the host's own verifier, or use NewVerifierAuthenticator for remote JWKS issuers)")
	}
	o := applyOptions(opts)
	return auth.NewAuthenticator(auth.AuthenticatorConfig{
		Verifier:       v,
		Admit:          auth.Admission(o.admit),
		OmitTokenRoles: o.omitTokenRoles,
	}), nil
}

// NewVerifierAuthenticator builds an AuthKit-backed, framework-neutral
// billingauth.Authenticator that verifies bearer tokens against the given JWKS
// issuers, optionally constraining the token audience.
//
// This is an explicit embedded-host bridge for REMOTE issuers: keys are
// HTTP-fetched from each issuer's /.well-known/jwks.json. A host embedding the
// control plane in-process should NOT verify its own tokens this way — use
// ControlPlane.UserAuthenticator (#739) or NewAuthenticator with its own
// verifier; the JWKS HTTP route exists purely for external verifiers.
// Standalone OpenRails does not read issuers from config; it authenticates
// with its own control-plane/AuthKit tokens and merchant remote applications.
func NewVerifierAuthenticator(issuers []string, expectedAud string, opts ...Option) (billingauth.Authenticator, error) {
	v, err := auth.NewIssuerVerifier(issuers, expectedAud)
	if err != nil {
		return nil, err
	}
	o := applyOptions(opts)
	return auth.NewAuthenticator(auth.AuthenticatorConfig{
		Verifier:       auth.RequestVerifierFor(v),
		Admit:          auth.Admission(o.admit),
		OmitTokenRoles: o.omitTokenRoles,
	}), nil
}

// NewDelegatedAuthenticator is the delegated twin of NewAuthenticator: a
// billingauth.DelegatedAuthenticator for the embedded self-service surface
// (RouteSetCustomer — /billing/v1/me/* and /billing/v1/customers/*) built over
// the HOST's own verifier. It returns a DelegatedPrincipal carrying:
//
//   - MerchantID: the ENGINE's bound merchant, pinned here at construction —
//     never anything read from the caller's token. tensorhub's hand-rolled
//     bridge pinned the caller's org UUID and scoped every request's RLS to a
//     nonexistent merchant (th#1765); this parameter exists so that bug is
//     unwritable. Injecting a verifier does not weaken it: the verifier only
//     produces claims, and the merchant pin is never read from them.
//   - SubjectID: the token's `sub` claim (the acting end user).
//   - Issuer: the verified token issuer, or WithIssuer's override, for audit.
//   - Permissions: permissions.ForRoles over the token's roles by default;
//     WithRolePermissions for another role vocabulary, WithPermissionResolver
//     for a grant that needs the request or a live authority.
//
// Every host used to hand-write this mapping; tensorhub's was a live bug
// (th#1765) and cozy-art simply never wrote one, 404ing its whole
// self-service surface (ca#269). Pass the result as
// embedded.MountOptions.DelegatedAuthenticator.
func NewDelegatedAuthenticator(v Verifier, merchantID string, opts ...DelegatedOption) (billingauth.DelegatedAuthenticator, error) {
	if v == nil {
		return nil, fmt.Errorf("delegated authenticator: verifier is required (pass the host's own verifier, or use NewVerifierDelegatedAuthenticator for remote JWKS issuers)")
	}
	return newDelegated(v, merchantID, opts)
}

// NewVerifierDelegatedAuthenticator is NewDelegatedAuthenticator over a JWKS
// verifier this package builds from a REMOTE issuer allowlist (#913),
// optionally constraining the audience. A host that verifies its own tokens
// in-process wants NewDelegatedAuthenticator instead — see the package doc.
func NewVerifierDelegatedAuthenticator(issuers []string, expectedAud string, merchantID string, opts ...DelegatedOption) (billingauth.DelegatedAuthenticator, error) {
	v, err := auth.NewIssuerVerifier(issuers, expectedAud)
	if err != nil {
		return nil, err
	}
	return newDelegated(auth.RequestVerifierFor(v), merchantID, opts)
}

func newDelegated(v auth.RequestVerifier, merchantID string, opts []DelegatedOption) (billingauth.DelegatedAuthenticator, error) {
	if _, err := merchant.ParseID(merchantID); err != nil {
		return nil, fmt.Errorf("delegated authenticator: merchant id %q: %w (pass the engine's bound merchant id)", merchantID, err)
	}
	var o delegatedOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	// Two answers to the same question is a wiring bug, and silently
	// preferring one hides a grant the host believes it configured.
	if o.rolePermissions != nil && o.resolver != nil {
		return nil, fmt.Errorf("delegated authenticator: WithRolePermissions and WithPermissionResolver are mutually exclusive — a role mapping is the resolver's simple case, so pass one")
	}
	resolver := o.resolver
	if resolver == nil {
		mapper := o.rolePermissions
		if mapper == nil {
			mapper = func(roles []string) []string { return permissions.ForRoles(roles...) }
		}
		resolver = func(_ context.Context, _ *http.Request, cl verify.Claims) ([]string, error) {
			return mapper(cl.Roles), nil
		}
	}
	return auth.NewDelegatedAuthenticator(auth.DelegatedConfig{
		Verifier:     v,
		MerchantID:   merchantID,
		MerchantSlug: o.merchantSlug,
		Issuer:       o.issuer,
		Admit:        auth.Admission(o.admit),
		Permissions:  auth.PermissionResolver(resolver),
	}), nil
}

func applyOptions(opts []Option) options {
	var o options
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}
