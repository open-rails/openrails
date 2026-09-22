//go:build integration

package embedhttp

import (
	"fmt"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// FixtureRoutes keeps legacy standalone/delegated Gate business fixtures on
// production registrations and middleware. Constructor Auth acceptance tests
// must call ConfiguredRoutes or Runtime.HTTPRoutes instead.
func FixtureRoutes(a *app.App, policy *HTTPConfig, delegated billingauth.DelegatedAuthenticator, authenticator billingauth.Authenticator, gate billingauth.Gate) (*router.Table, error) {
	if authenticator == nil && gate == nil {
		return ConfiguredRoutes(a, policy)
	}
	if a == nil || a.Runtime == nil || policy == nil {
		return nil, fmt.Errorf("fixture runtime and policy required")
	}
	if err := validateCustomerRoutes(policy.CustomerRoutes, a.Runtime.Auth); err != nil {
		return nil, err
	}
	asm := FromApp(a)
	asm.Authenticator, asm.Gate = authenticator, gate
	return buildConfiguredRoutes(a, *policy, asm)
}
