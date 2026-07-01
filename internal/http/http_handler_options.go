package server

import (
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/routesurface"
)

type RouteSet = embedhttp.RouteSet

const (
	RouteSetCheckout         = embedhttp.RouteSetCheckout
	RouteSetCustomer         = embedhttp.RouteSetCustomer
	RouteSetMerchantAdmin    = embedhttp.RouteSetMerchantAdmin
	RouteSetCatalog          = embedhttp.RouteSetCatalog
	RouteSetPaymentProviders = embedhttp.RouteSetPaymentProviders
	RouteSetMerchantAPI      = embedhttp.RouteSetMerchantAPI
	RouteSetWebhooks         = embedhttp.RouteSetWebhooks
)

var (
	EmbeddedDefaultRouteSets   = append([]RouteSet(nil), embedhttp.EmbeddedDefaultRouteSets...)
	StandaloneDefaultRouteSets = append([]RouteSet(nil), embedhttp.StandaloneDefaultRouteSets...)
)

// HTTPHandlerOptions controls which billing HTTP route groups are registered on the returned handler.
//
// A zero RouteSets slice uses EmbeddedDefaultRouteSets.
//
// Note: health/debug endpoints are intentionally not part of embedded handler options.
// Standalone mode owns service-level health routes.
type HTTPHandlerOptions struct {
	RouteSets      []RouteSet
	ProviderRoutes *routesurface.ProviderRoutes
}
