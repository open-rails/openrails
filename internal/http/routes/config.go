package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/router"
)

// configRoutes serve what a browser needs to know before it pays. The
// deployment's part takes no credential and is always mounted, so even a
// minimal deployment is discoverable; the merchant's part (its payment setup
// and name) answers only a credential bound to that merchant.
var configRoutes = []Route{
	{Method: GET, Path: "/v1/config", Name: "GetPublicConfig", Group: Meta, Auth: AuthPublic, NoConn: true, PayURL: PayURLPublic,
		Cache: billing.ReadPublicConfig,
		Doc: "GetPublicConfig reads the deployment's capabilities and public configuration. It sends no credential " +
			"and does not invoke the handle's token provider. Over HTTP it is what a browser reads from the same " +
			"server, ready for a host's page response (billing-ui's BillingProvider config); in process, " +
			"Engine.PublicConfig names the mount the browser reads.",
		Responses: []Reply{{200, billing.PublicConfig{}}}, Bind: publicConfig},
	{Method: GET, Path: "/v1/me/config", Group: Customer, Auth: AuthCustomer, PayURL: PayURLMerchant,
		Responses: []Reply{{200, billing.CustomerConfig{}}}, Errors: codes("service_unavailable"), Handler: h(handlers.GetCustomerConfig)},
}

// publicConfig binds the public read to the capabilities its assembly
// supplies; without them it is not mounted.
func publicConfig(e *Env) router.Handler {
	if e.Capabilities == nil {
		return nil
	}
	return router.Handler(handlers.GetPublicConfig(*e.Capabilities))
}
