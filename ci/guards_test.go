//go:build e2e && integration

package ci_test

import (
	"context"
	"net/http"

	"github.com/open-rails/helpers/auth"
	"github.com/open-rails/openrails/internal/billingauth/authtest"

	"github.com/open-rails/openrails"
)

// staffGroups turns every staff route group on; the test hosts' staff hold
// every permission in staffRoot, their Authenticators' root scope.
var (
	staffGroups = openrails.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true}
	staffRoot   = authtest.Root
)

// hostKey says every request is the host backend's API key, an application
// holding every permission in staffRoot.
type hostKey struct{}

func (hostKey) Authenticate(*http.Request) (auth.Verified, error) { return hostKey{}, nil }
func (hostKey) RootScope(context.Context) (auth.Scope, error)     { return staffRoot, nil }

func (hostKey) Identity() openrails.Identity {
	return openrails.Identity{Issuer: authtest.Issuer, Subject: "test-host", SubjectKind: openrails.SubjectApplication,
		Invoker: openrails.Invoker{Issuer: authtest.Issuer, ID: "test-host"}, Credential: openrails.Credential{Kind: openrails.CredentialAPIKey, ID: "k_test"}}
}

func (hostKey) Can(_ context.Context, scope auth.Scope, permission string) (bool, error) {
	return scope == staffRoot && permission != "", nil
}
