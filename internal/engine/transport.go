package engine

import (
	"context"
	"fmt"
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/inprocess"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/internal/requestauth"
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

// newServiceHandler mounts the staff routes the Go client calls, gated by
// the transport's own credential: the host's merchant-owner authority, a
// context value no network request carries. Ambient host context is stripped
// before it. It serves no customer routes: the Go client has no customer
// methods.
func newServiceHandler(rt *app.Runtime) http.Handler {
	mux := &router.Table{}
	opts := httproutes.HostOptions()
	caps := embedhttp.CapabilitiesFor(rt, opts.Permissions, embedhttp.ProviderRoutesForRuntime(rt, nil), nil)
	opts.Capabilities = &caps
	httproutes.RegisterStaffRoutes(router.NewMux(mux, "/v1", rt), rt, opts)
	router.ResolveMerchantSelectors(mux, "", func(ctx context.Context, r *http.Request) (billingauth.Target, error) {
		return merchanttarget.Resolve(ctx, r, rt.Merchants, rt.ConfiguredMerchant(), "")
	})
	// The same body cap the HTTP mounts apply: an oversized request gets the
	// same 413 envelope in every deployment.
	return withVerificationMemo(middleware.RequestLimitsHTTP(middleware.DefaultMaxBodyBytes)(mux.Handler()))
}

func withVerificationMemo(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { next.ServeHTTP(w, requestauth.Begin(r)) })
}

func merchantMismatchMsg(bound, pinned billing.MerchantID) string {
	return fmt.Sprintf("openrails: client is bound to merchant %s but the runtime is bound to merchant %s", pinned, bound)
}
