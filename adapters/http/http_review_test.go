package openrailshttp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

func TestHTTPReviewFullInventoryOnServeMuxAndChi(t *testing.T) {
	for _, kind := range []string{"servemux", "chi"} {
		t.Run(kind, func(t *testing.T) {
			cfg := &config.Config{AllowCatalogUpdates: true, MerchantConfigSource: config.MerchantConfigSourceAPI}
			delegated := billingauth.DelegatedAuthenticatorFunc(func(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
				return nil, billingauth.ErrUnauthenticated
			})
			graph := &app.App{Config: cfg, Runtime: &app.Runtime{Config: cfg}}
			policy := &embed.HTTPConfig{Checkout: true, Customer: true, MerchantAdmin: true, Catalog: true, MerchantConfig: true, MerchantAPI: true,
				Authenticator: billingauth.AuthenticatorFunc(func(context.Context, *http.Request) (billingauth.UserContext, error) {
					return billingauth.UserContext{}, billingauth.ErrUnauthenticated
				}), Gate: billingauth.NewDelegatedGate(delegated)}
			table, err := embedhttp.ConfiguredRoutes(graph, policy, delegated)
			require.NoError(t, err)
			b := &Bundle{}
			for _, route := range table.Entries {
				b.routes = append(b.routes, embed.HTTPRoute{Method: route.Method, Path: strings.TrimPrefix(route.Path, "/billing"), Handler: route.Handler})
			}
			var engine http.Handler
			if kind == "servemux" {
				mux := http.NewServeMux()
				require.NoError(t, b.Mount(mux, "/api/pay"))
				mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(418) })
				engine = mux
			} else {
				mux := chi.NewRouter()
				mux.Route("/api/pay", func(group chi.Router) { require.NoError(t, b.Mount(group)) })
				mux.NotFound(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(418) })
				engine = mux
			}
			for _, tc := range []struct {
				method, path string
				code         int
			}{
				{http.MethodGet, "/api/pay/v1/capabilities", http.StatusOK},
				{http.MethodHead, "/api/pay/v1/capabilities", http.StatusOK},
				{http.MethodPost, "/api/pay/v1/merchant/customers/entitlements:batch", http.StatusUnauthorized},
				{http.MethodOptions, "/api/pay/v1/checkout", http.StatusNoContent},
				{http.MethodGet, "/api/pay/v1/unrelated", 418},
				{http.MethodGet, "/api/payment/v1/capabilities", 418},
				{http.MethodGet, "/other", 418},
			} {
				w := httptest.NewRecorder()
				req := httptest.NewRequest(tc.method, tc.path, nil)
				req.Header.Set("Authorization", "Bearer invalid")
				engine.ServeHTTP(w, req)
				require.Equal(t, tc.code, w.Code, tc.path+" "+w.Body.String())
			}
		})
	}
}

func TestHTTPReviewRawWebhookRequestAcrossMounts(t *testing.T) {
	const body = "{ \"signed\" : \"bytes\" }\n"
	for _, kind := range []string{"servemux", "chi"} {
		t.Run(kind, func(t *testing.T) {
			const target = "/api/pay/v1/webhooks/stripe/acct_test?signature=unchanged"
			calls := 0
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				raw, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				require.Equal(t, body, string(raw))
				require.Equal(t, target, r.RequestURI)
				require.Equal(t, "/api/pay/v1/webhooks/stripe/acct_test", r.URL.Path)
				require.Equal(t, "signed-header", r.Header.Get("Stripe-Signature"))
				w.WriteHeader(http.StatusNoContent)
			})
			b := &Bundle{routes: []embed.HTTPRoute{{Method: http.MethodPost, Path: "/v1/webhooks/{provider}/{account_id}", Handler: h}}}
			var engine http.Handler
			if kind == "servemux" {
				mux := http.NewServeMux()
				require.NoError(t, b.Mount(mux, "/api/pay"))
				engine = mux
			} else {
				mux := chi.NewRouter()
				mux.Route("/api/pay", func(r chi.Router) { require.NoError(t, b.Mount(r)) })
				engine = mux
			}
			req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
			req.Header.Set("Stripe-Signature", "signed-header")
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)
			require.Equal(t, http.StatusNoContent, w.Code)
			require.Equal(t, 1, calls)
		})
	}
}
