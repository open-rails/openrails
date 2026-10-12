package routes

import (
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/customernetwork"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"net/http"
)

// NetworkAccountHandler reuses the network's exact native-account and terms
// admission for an embedding product's own HTTP routes.
func NetworkAccountHandler(rt *app.Runtime, a auth.Authenticator, next func(http.ResponseWriter, *http.Request, billing.NetworkAccountID)) http.Handler {
	env := &Env{Runtime: rt, Customers: a}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rt.Network == nil {
			http.Error(w, "customer network is unavailable", http.StatusServiceUnavailable)
			return
		}
		route := Route{Method: r.Method, Path: "/v1/network/_host", Auth: AuthNetwork, Group: Network, NoConn: true}
		end := func(req *httprequest.Request) {
			account, ok := customernetwork.AccountFrom(req.Request.Context())
			if !ok {
				req.ErrorCode(billing.CodePermissionRequired, "")
				return
			}
			next(w, req.Request, account)
		}
		router.Chain(end, env.networkGates(route))(httprequest.NewHTTP(w, r, rt))
	})
}
