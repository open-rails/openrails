package engine

import (
	"context"
	"fmt"
	"net/http"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/inprocess"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// InprocessBaseURL is the synthetic base of the embedded client. `.invalid`
// (RFC 2606) never resolves, so a request that bypassed the in-process
// transport fails instead of leaking onto the network.
const InprocessBaseURL = "http://openrails.invalid"

// Transport is the in-process transport the embedded Client is built on, and
// the bearer that carries the engine's own merchant-owner authority over it.
func (e *Engine) Transport() (http.RoundTripper, string) {
	rt := e.App.Runtime
	e.handlerOnce.Do(func() { e.handler = newServiceHandler(rt) })
	return inprocess.NewTransportWithResolver(e.handler, rt.ConfiguredMerchant, func(ctx context.Context, request *http.Request) (billingauth.Target, error) {
		return merchanttarget.Resolve(ctx, request, rt.Merchants, rt.ConfiguredMerchant(), "")
	})
}

// newServiceHandler mounts the merchant and verified customer routes HTTP
// hosts publish. The transport's own credential carries merchant-owner
// authority; explicit customer credentials go through the host's
// authentication. Ambient host context is stripped before either path.
func newServiceHandler(rt *app.Runtime) http.Handler {
	mux := &router.Table{}
	opts := httproutes.Options{Gate: httproutes.NewGate(httproutes.GateOptions{}), InProcess: true}
	var authn billingauth.DelegatedAuthenticator
	if rt.Auth != nil {
		opts.Gate = embedhttp.IntegrationGate(rt)
		authn = embedhttp.RuntimeCustomerAuthentication(rt)
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
	router.AddMerchantSelectorRoutes(mux, "", func(ctx context.Context, r *http.Request) (billingauth.Target, error) {
		return merchanttarget.Resolve(ctx, r, rt.Merchants, rt.ConfiguredMerchant(), "")
	})
	// The same body cap the HTTP mounts apply: an oversized request gets the
	// same 413 envelope in every deployment.
	return withVerificationMemo(middleware.BodyLimitHTTP(middleware.DefaultMaxBodyBytes)(mux.Handler()))
}

func withVerificationMemo(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { next.ServeHTTP(w, requestauth.Begin(r)) })
}

func merchantMismatchMsg(bound, pinned merchant.ID) string {
	return fmt.Sprintf("openrails: client is bound to merchant %s but the runtime is bound to merchant %s", pinned, bound)
}
