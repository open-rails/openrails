// Package authkit is the OPT-IN AuthKit verifier adapter for embedded hosts
// (#284). The embedded CORE (pkg/embedded.New -> bootstrap.NewApp ->
// internal/app) deliberately does NOT import AuthKit: it authenticates only via
// the host-supplied billingauth.Authenticator. Hosts (and the OpenRails
// standalone binary) that want the AuthKit JWT-verifier auth boundary opt in
// here and pass the result as embedded.Options.Authenticator.
//
// Importing this package is what pulls github.com/open-rails/authkit onto a
// host's dependency graph; pkg/embedded itself stays AuthKit-free.
package authkit

import (
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/auth"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// NewVerifierAuthenticator builds an AuthKit-backed, framework-neutral
// billingauth.Authenticator that verifies bearer tokens against the given JWKS
// issuers, optionally constraining the token audience. Pass the returned value
// as embedded.Options.Authenticator.
//
// This is the verifier-only ("pure verifier") auth mode that used to be wired
// implicitly from Config.Auth.Issuers inside the embedded core. It is now opt-in.
func NewVerifierAuthenticator(issuers []string, expectedAud string) (billingauth.Authenticator, error) {
	return auth.NewAuthenticator(&config.AuthConfig{
		Issuers:          issuers,
		ExpectedAudience: expectedAud,
	})
}

// NewVerifierAuthenticatorFromConfig builds the AuthKit-backed authenticator
// directly from a *config.AuthConfig (issuers + expected audience). Convenience
// for hosts/standalone that already hold the parsed config.
func NewVerifierAuthenticatorFromConfig(cfg *config.AuthConfig) (billingauth.Authenticator, error) {
	return auth.NewAuthenticator(cfg)
}
