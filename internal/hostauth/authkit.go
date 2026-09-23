// Package hostauth contains standalone identity composition and legacy host
// fixture adapters. Embedded hosts use the neutral billingauth integration.
package hostauth

import (
	"context"
	"fmt"
	"net/http"

	"github.com/open-rails/authkit/verify"

	"github.com/open-rails/openrails/internal/auth"
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
// lifetime; a host that authorizes from a live authority instead (host-one
// #774) omits it rather than passing a snapshot nothing should read.
func WithoutTokenRoles() Option {
	return func(o *options) { o.omitTokenRoles = true }
}

// DelegatedOption customizes NewDelegatedAuthenticator /
// NewVerifierDelegatedAuthenticator.
type DelegatedOption func(*delegatedOptions)

type delegatedOptions struct {
	resolver     PermissionResolver
	admit        Admission
	merchantSlug string
	issuer       string
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
// snapshot is stale for the token's lifetime (host-one #774).
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
// a standalone host Gate input. Embedded constructors use New(Config) with Options.Auth.
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
		Verifier:       v,
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
//     never anything read from the caller's token. host-four's hand-rolled
//     bridge pinned the caller's org UUID and scoped every request's RLS to a
//     nonexistent merchant (upstream#1765); this parameter exists so that bug is
//     unwritable. Injecting a verifier does not weaken it: the verifier only
//     produces claims, and the merchant pin is never read from them.
//   - SubjectID: the token's `sub` claim (the acting end user).
//   - Issuer: the verified token issuer, or WithIssuer's override, for audit.
//   - Permissions: none by default. WithPermissionResolver supplies explicit
//     live authority; token role names never confer billing permissions.
//
// Every host used to hand-write this mapping; host-four's was a live bug
// (upstream#1765) and host-three simply never wrote one, 404ing its whole
// self-service surface (upstream#269). Pass the result as
// embed.Options.DelegatedAuthenticator.
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
	return newDelegated(v, merchantID, opts)
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
	return auth.NewDelegatedAuthenticator(auth.DelegatedConfig{
		Verifier:     v,
		MerchantID:   merchantID,
		MerchantSlug: o.merchantSlug,
		Issuer:       o.issuer,
		Admit:        auth.Admission(o.admit),
		Permissions:  auth.PermissionResolver(o.resolver),
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
