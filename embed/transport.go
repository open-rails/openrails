package embed

import (
	"fmt"
	"net/http"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/pkg/merchant"
)

// inprocessBaseURL is the synthetic base the unified embedded client is built
// with. `.invalid` (RFC 2606) never resolves, so if the in-process transport
// were ever bypassed the request fails instead of leaking onto the network.
const inprocessBaseURL = "http://openrails.invalid"

// newServiceHandler builds the in-process merchant API surface (#685): the SAME
// neutral RegisterServiceRoutes mux the standalone server mounts at
// /v1/merchant (#670), including the real permission gate and the
// MerchantDBConnMW RLS pin. The gate is built with NO resolvers, so the ONLY
// credential it accepts is the context-attached host principal — which only the
// in-process transport can set; a request reaching this handler without it
// (i.e. anything network-shaped) is rejected 401 by the real middleware.
func newServiceHandler(rt *app.Runtime) http.Handler {
	mux := http.NewServeMux()
	opts := httproutes.Options{Gate: httproutes.NewGate(httproutes.GateOptions{})}
	httproutes.RegisterServiceRoutes(router.NewMux(mux, "/v1/merchant", rt), rt, opts)
	httproutes.RegisterMerchantActionRoutes(router.NewMux(mux, "/v1/merchant", rt), rt, opts)
	httproutes.RegisterCatalogRoutes(router.NewMux(mux, "/v1/merchant/catalog", rt), rt, opts)
	// #737: DeclaredBilling import, same gate (host principal holds merchant:*).
	httproutes.RegisterImportRoutes(router.NewMux(mux, "/v1/import", rt), rt, opts)
	// The same request body cap the HTTP mounts apply, so an oversized request
	// is refused with the same 413 envelope in every deployment.
	return middleware.BodyLimitHTTP(middleware.DefaultMaxBodyBytes)(mux)
}

// hostPermissions is the owner grant used by embedded host assertions.
func hostPermissions() []string {
	return []string{string(controlplane.MerchantType.OwnerGrant())}
}

func merchantMismatchMsg(bound, pinned merchant.ID) string {
	return fmt.Sprintf("openrails: client is bound to merchant %s but the runtime is bound to merchant %s", pinned, bound)
}
