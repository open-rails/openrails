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
	"github.com/open-rails/openrails/internal/auth"
	"github.com/open-rails/openrails/pkg/billingauth"
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
