package engine

import (
	"context"
	"errors"
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/inprocess"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/merchants"
)

// InprocessBaseURL is the synthetic base of an in-process handle. `.invalid`
// (RFC 2606) never resolves, so a request that bypassed the in-process
// transport fails instead of leaking onto the network.
const InprocessBaseURL = "http://openrails.invalid"

// NewClient builds the openrails.Client handle for merchant over an
// in-process transport (inprocess.NewTransport). The root package sets it;
// operator tooling inside the module asserts the result to *openrails.Client.
var NewClient func(transport http.RoundTripper, merchant billing.MerchantID) any

// Transport is the in-process transport the engine's handles call through:
// each call acts as the host bound to the handle's merchant.
func (e *Engine) Transport() http.RoundTripper {
	rt := e.App.Runtime
	// Mounted at the first call, so what a host adds to the runtime after
	// New (a server's customer network) is served.
	served := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.handlerOnce.Do(func() { e.handler = newServiceHandler(rt) })
		e.handler.ServeHTTP(w, r)
	})
	return inprocess.NewTransport(served)
}

// LiveMerchant is id's directory row while it is a live merchant:
// billing.ErrMerchantUnresolved for an unknown, deleted or archived one.
func (e *Engine) LiveMerchant(ctx context.Context, id billing.MerchantID) (*merchants.Merchant, error) {
	directory := e.App.Runtime.Merchants
	if id.IsZero() || directory == nil {
		return nil, billing.ErrMerchantUnresolved
	}
	m, err := directory.Get(ctx, id)
	switch {
	case errors.Is(err, merchants.ErrMerchantNotFound):
		return nil, billing.ErrMerchantUnresolved
	case err != nil:
		return nil, err
	case m == nil || m.Status != merchants.StatusActive:
		return nil, billing.ErrMerchantUnresolved
	}
	return m, nil
}

// newServiceHandler mounts the routes the Go client calls (the staff and
// programmatic ones), gated by the transport's own credential: the host's
// merchant-owner authority, a context value no network request carries.
// Ambient host context is stripped before it. It serves no customer routes:
// the Go client has no customer methods.
func newServiceHandler(rt *app.Runtime) http.Handler {
	mux := &router.Table{}
	opts := httproutes.HostOptions()
	caps := embedhttp.CapabilitiesFor(rt, opts.Groups, embedhttp.ProviderRoutesForRuntime(rt, nil), nil)
	opts.Capabilities = &caps
	httproutes.RegisterMetaRoutes(router.NewMux(mux, "", rt), opts)
	httproutes.RegisterStaffRoutes(router.NewMux(mux, "/v1", rt), rt, opts)
	httproutes.RegisterAppRoutes(router.NewMux(mux, "/v1", rt), rt, opts)
	// The same body cap the HTTP mounts apply: an oversized request gets the
	// same 413 envelope in every deployment.
	return middleware.RequestLimitsHTTP(middleware.DefaultMaxBodyBytes)(mux.Handler())
}
