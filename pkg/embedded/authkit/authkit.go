// Package authkit is the OPT-IN AuthKit verifier adapter for embedded hosts
// (#284). The embedded CORE (pkg/embedded.New -> app.BootstrapWithOptions ->
// internal/app) deliberately does NOT import AuthKit. Hosts (and the OpenRails
// standalone binary) that want the AuthKit JWT-verifier auth boundary opt in
// here and pass the result at HTTP mount time.
//
// Importing this package is what pulls github.com/open-rails/authkit onto a
// host's dependency graph; pkg/embedded itself stays AuthKit-free.
package authkit

import (
	"fmt"

	"github.com/open-rails/openrails/internal/auth"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/open-rails/openrails/permissions"
)

// NewVerifierAuthenticator builds an AuthKit-backed, framework-neutral
// billingauth.Authenticator that verifies bearer tokens against the given JWKS
// issuers, optionally constraining the token audience. Pass the returned value
// as gin.MountOptions.Authenticator or a host Gate input.
//
// This is an explicit embedded-host bridge for REMOTE issuers: keys are
// HTTP-fetched from each issuer's /.well-known/jwks.json. A host embedding the
// control plane in-process should NOT verify its own tokens this way — use
// ControlPlane.UserAuthenticator (#739), which shares the control plane's
// in-memory verifier state; the JWKS HTTP route exists purely for external
// verifiers. Standalone OpenRails does not read issuers from config; it
// authenticates with its own control-plane/AuthKit tokens and merchant remote
// applications.
func NewVerifierAuthenticator(issuers []string, expectedAud string) (billingauth.Authenticator, error) {
	v, err := auth.NewIssuerVerifier(issuers, expectedAud)
	if err != nil {
		return nil, err
	}
	return auth.NewAuthenticator(v), nil
}

// DelegatedOption customizes NewVerifierDelegatedAuthenticator.
type DelegatedOption func(*delegatedOptions)

type delegatedOptions struct {
	rolePermissions func(roles []string) []string
}

// WithRolePermissions overrides the canonical permissions.ForRoles preset
// with the host's own role→permission mapping. The mapping's output feeds
// DelegatedPrincipal.Permissions verbatim (the embedding host is trusted,
// #564), and composes with billingauth.NewDelegatedGate — wildcard grants
// like permissions.MerchantAll are expanded by billingauth.HasPermission. A
// mapper returning nil grants nothing beyond the grant-free /v1/me surface.
func WithRolePermissions(mapper func(roles []string) []string) DelegatedOption {
	return func(o *delegatedOptions) { o.rolePermissions = mapper }
}

// NewVerifierDelegatedAuthenticator is the delegated twin of
// NewVerifierAuthenticator (#913): an AuthKit-backed
// billingauth.DelegatedAuthenticator for the embedded self-service surface
// (RouteSetCustomer — /billing/v1/me/* and /billing/v1/customers/*). It
// verifies the bearer token against the given JWKS issuers (optionally
// constraining the audience) and returns a DelegatedPrincipal carrying:
//
//   - MerchantID: the ENGINE's bound merchant, pinned here at construction —
//     never anything read from the caller's token. tensorhub's hand-rolled
//     bridge pinned the caller's org UUID and scoped every request's RLS to a
//     nonexistent merchant (th#1765); this parameter exists so that bug is
//     unwritable.
//   - SubjectID: the token's `sub` claim (the acting end user).
//   - Issuer: the verified token issuer, for audit.
//   - Permissions: permissions.ForRoles over the token's roles — the
//     documented canonical preset (owner/admin → merchant:*+customer:*,
//     member → the customer self-service set, read-only → its :read subset).
//     Hosts with their own role vocabulary override via WithRolePermissions.
//
// Every host used to hand-write this mapping; tensorhub's was a live bug
// (th#1765) and cozy-art simply never wrote one, 404ing its whole
// self-service surface (ca#269). Pass the result as
// embedded.MountOptions.DelegatedAuthenticator.
func NewVerifierDelegatedAuthenticator(issuers []string, expectedAud string, merchantID string, opts ...DelegatedOption) (billingauth.DelegatedAuthenticator, error) {
	if _, err := merchant.ParseID(merchantID); err != nil {
		return nil, fmt.Errorf("delegated authenticator: merchant id %q: %w (pass the engine's bound merchant id)", merchantID, err)
	}
	v, err := auth.NewIssuerVerifier(issuers, expectedAud)
	if err != nil {
		return nil, err
	}
	o := delegatedOptions{rolePermissions: func(roles []string) []string { return permissions.ForRoles(roles...) }}
	for _, opt := range opts {
		opt(&o)
	}
	return auth.NewDelegatedAuthenticator(v, merchantID, o.rolePermissions), nil
}
