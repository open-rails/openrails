package routes

import (
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/scim"
)

// scimBody is a SCIM message (RFC 7643, RFC 7644): its schema is the
// standard's, which the discovery routes describe, so api/openapi.json leaves
// these routes out.
var scimBody = Stream{ContentType: scim.MediaType}

// provisioningRoutes are the SCIM 2.0 service provider, in the programmatic
// group: the merchant's directory pushing its users, which become its
// customers' contacts. Each takes the merchant's provisioning token, which
// opens nothing else, or an application holding its permission: reads
// merchant:provisioning:read, writes merchant:provisioning:manage.
var provisioningRoutes = []Route{
	{Method: GET, Path: "/v1/app/scim/v2/ServiceProviderConfig", Group: App, Auth: AuthProvisioning, Permission: needProvisioningRead, When: FeatureCustomerProvisioning, NoConn: true,
		Responses: []Reply{{200, scimBody}}, Bind: provision((*scim.Server).ServiceProviderConfig)},
	{Method: GET, Path: "/v1/app/scim/v2/ResourceTypes", Group: App, Auth: AuthProvisioning, Permission: needProvisioningRead, When: FeatureCustomerProvisioning, NoConn: true,
		Responses: []Reply{{200, scimBody}}, Bind: provision((*scim.Server).ResourceTypes)},
	{Method: GET, Path: "/v1/app/scim/v2/ResourceTypes/{id}", Group: App, Auth: AuthProvisioning, Permission: needProvisioningRead, When: FeatureCustomerProvisioning, NoConn: true,
		Responses: []Reply{{200, scimBody}}, Bind: provision((*scim.Server).ResourceType)},
	{Method: GET, Path: "/v1/app/scim/v2/Schemas", Group: App, Auth: AuthProvisioning, Permission: needProvisioningRead, When: FeatureCustomerProvisioning, NoConn: true,
		Responses: []Reply{{200, scimBody}}, Bind: provision((*scim.Server).Schemas)},
	{Method: GET, Path: "/v1/app/scim/v2/Schemas/{id}", Group: App, Auth: AuthProvisioning, Permission: needProvisioningRead, When: FeatureCustomerProvisioning, NoConn: true,
		Responses: []Reply{{200, scimBody}}, Bind: provision((*scim.Server).Schema)},
	{Method: POST, Path: "/v1/app/scim/v2/Users", Group: App, Auth: AuthProvisioning, Permission: needProvisioning, When: FeatureCustomerProvisioning, NoConn: true,
		Request: scimBody, Responses: []Reply{{201, scimBody}}, Bind: provision((*scim.Server).CreateUser)},
	{Method: GET, Path: "/v1/app/scim/v2/Users", Group: App, Auth: AuthProvisioning, Permission: needProvisioningRead, When: FeatureCustomerProvisioning, NoConn: true,
		Query: scim.ListQuery{}, Responses: []Reply{{200, scimBody}}, Bind: provision((*scim.Server).ListUsers)},
	{Method: GET, Path: "/v1/app/scim/v2/Users/{id}", Group: App, Auth: AuthProvisioning, Permission: needProvisioningRead, When: FeatureCustomerProvisioning, NoConn: true,
		Responses: []Reply{{200, scimBody}}, Bind: provision((*scim.Server).GetUser)},
	{Method: PUT, Path: "/v1/app/scim/v2/Users/{id}", Group: App, Auth: AuthProvisioning, Permission: needProvisioning, When: FeatureCustomerProvisioning, NoConn: true,
		Request: scimBody, Responses: []Reply{{200, scimBody}}, Bind: provision((*scim.Server).ReplaceUser)},
	{Method: PATCH, Path: "/v1/app/scim/v2/Users/{id}", Group: App, Auth: AuthProvisioning, Permission: needProvisioning, When: FeatureCustomerProvisioning, NoConn: true,
		Request: scimBody, Responses: []Reply{{200, scimBody}}, Bind: provision((*scim.Server).PatchUser)},
	{Method: DELETE, Path: "/v1/app/scim/v2/Users/{id}", Group: App, Auth: AuthProvisioning, Permission: needProvisioning, When: FeatureCustomerProvisioning, NoConn: true,
		Responses: []Reply{{204, nil}}, Bind: provision((*scim.Server).DeleteUser)},
	{Method: POST, Path: "/v1/app/scim/v2/Bulk", Group: App, Auth: AuthProvisioning, Permission: needProvisioning, When: FeatureCustomerProvisioning, NoConn: true,
		Request: scimBody, Responses: []Reply{{200, scimBody}}, Bind: provision((*scim.Server).Bulk)},

	// The merchant's provisioning tokens.
	{Method: GET, Path: "/v1/merchant/provisioning-tokens", Group: MerchantConfig, When: FeatureCustomerProvisioning, Level: LevelRead, Auth: AuthMerchant, Name: "ListProvisioningTokens",
		Doc: "ListProvisioningTokens lists the merchant's provisioning tokens, oldest first; their secrets are " +
			"never read back.",
		Query: billing.ProvisioningTokenListParams{}, Responses: []Reply{{200, billing.ListPage[billing.ProvisioningToken]{}}}, Handler: h(handlers.ListProvisioningTokens)},
	{Method: POST, Path: "/v1/merchant/provisioning-tokens", Group: MerchantConfig, When: FeatureCustomerProvisioning, Level: LevelUpdate, Auth: AuthMerchant, Name: "CreateProvisioningToken",
		Doc: "CreateProvisioningToken mints a token the merchant's directory presents at /scim/v2 as its bearer. " +
			"The answer holds the token, which is never shown again.",
		Request: billing.CreateProvisioningTokenParams{}, Responses: []Reply{{201, billing.CreatedProvisioningToken{}}}, Errors: codes("invalid_param"), Handler: h(handlers.CreateProvisioningToken)},
	{Method: DELETE, Path: "/v1/merchant/provisioning-tokens/{id}", Group: MerchantConfig, When: FeatureCustomerProvisioning, Level: LevelUpdate, Auth: AuthMerchant, Name: "DeleteProvisioningToken",
		Args:      []any{billing.ProvisioningTokenID{}},
		Doc:       "DeleteProvisioningToken revokes a provisioning token: the next request presenting it is refused.",
		Responses: []Reply{{204, nil}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.DeleteProvisioningToken)},
}

// provision binds a route to the assembly's SCIM server.
func provision(route func(*scim.Server) http.Handler) func(*Env) router.Handler {
	return func(e *Env) router.Handler {
		s := e.scimServer()
		if s == nil {
			return nil
		}
		return httprequest.FromHTTP(route(s))
	}
}
