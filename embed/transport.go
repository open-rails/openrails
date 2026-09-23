package embed

import (
	"context"
	"fmt"
	"github.com/open-rails/openrails/internal/merchanttarget"
	"net/http"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// inprocessBaseURL is the synthetic base the unified embedded client is built
// with. `.invalid` (RFC 2606) never resolves, so if the in-process transport
// were ever bypassed the request fails instead of leaking onto the network.
const inprocessBaseURL = "http://openrails.invalid"

// newServiceHandler mounts the same merchant and verified customer routes used
// by HTTP hosts. The transport's internal default credential carries merchant
// owner authority; explicit credentials go through the configured verifier.
// Ambient host context is stripped before either path.
func newServiceHandler(rt *app.Runtime, authn billingauth.DelegatedAuthenticator) http.Handler {
	mux := &router.Table{}
	opts := httproutes.Options{Gate: httproutes.NewGate(httproutes.GateOptions{DelegatedAuthenticator: authn})}
	if rt.Auth != nil {
		opts.Gate = embedhttp.IntegrationGate(rt)
		if authn == nil {
			authn = embedhttp.RuntimeCustomerAuthentication(rt)
		}
	}
	httproutes.RegisterServiceRoutes(router.NewMux(mux, "/v1/merchant", rt), rt, opts)
	httproutes.RegisterMerchantActionRoutes(router.NewMux(mux, "/v1/merchant", rt), rt, opts)
	httproutes.RegisterCatalogRoutes(router.NewMux(mux, "/v1/merchant/catalog", rt), rt, opts)
	httproutes.RegisterCatalogCollectionRoutes(router.NewMux(mux, "/v1/merchant/catalogs", rt), rt, opts)
	httproutes.RegisterOwnedCatalogRoutes(router.NewMux(mux, "/v1/catalog", rt), rt, opts)
	httproutes.RegisterMerchantConfigRoutes(router.NewMux(mux, "/v1/merchant", rt), rt, opts)
	// #737: DeclaredBilling import, same gate (host principal holds merchant:*).
	httproutes.RegisterImportRoutes(router.NewMux(mux, "/v1/import", rt), rt, opts)
	if authn != nil {
		httproutes.RegisterSelfServiceRoutes(router.NewMux(mux, "/v1/me", rt), rt, middleware.DelegatedPrincipalRequired(authn), embedhttp.ProviderRoutesForRuntime(rt, nil))
	}
	// The same request body cap the HTTP mounts apply, so an oversized request
	// is refused with the same 413 envelope in every deployment.
	router.AddMerchantSelectorRoutes(mux, "", func(ctx context.Context, r *http.Request) (billingauth.Target, error) {
		return merchanttarget.Resolve(ctx, r, rt.Merchants, rt.ConfiguredMerchant(), "")
	})
	return withVerificationMemo(middleware.BodyLimitHTTP(middleware.DefaultMaxBodyBytes)(mux.Handler()))
}

// hostPermissions is the owner grant used by embedded host assertions.
func hostPermissions() []string {
	return []string{string(controlplane.MerchantType.OwnerGrant())}
}

func merchantMismatchMsg(bound, pinned merchant.ID) string {
	return fmt.Sprintf("openrails: client is bound to merchant %s but the runtime is bound to merchant %s", pinned, bound)
}
