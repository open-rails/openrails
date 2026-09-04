package routes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

func TestSolanaEnrollmentRequiresRouteAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name, token, subject string
		wantStatus           int
	}{
		{name: "anonymous", wantStatus: http.StatusUnauthorized},
		{name: "invalid token", token: "invalid", wantStatus: http.StatusUnauthorized},
		{name: "missing subject", token: "valid", wantStatus: http.StatusUnauthorized},
		{name: "authenticated reaches enrollment", token: "valid", subject: "f2f186c0-6af9-4a40-8b99-6f0fb5b1f889", wantStatus: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			auth := billingauth.AuthenticatorFunc(func(_ context.Context, req *http.Request) (billingauth.UserContext, error) {
				calls++
				if req.Header.Get("Authorization") != "Bearer valid" {
					return billingauth.UserContext{}, errors.New("invalid token")
				}
				return billingauth.UserContext{UserID: tc.subject}, nil
			})
			mux := http.NewServeMux()
			runtime := &app.Runtime{} // No Solana service: proves whether the handler was reached without provider calls.
			providers := routesurface.ProviderRoutes{SolanaSigning: true}
			RegisterUserRoutes(router.NewMux(mux, "/v1", runtime), runtime, Options{Authenticator: auth, ProviderRoutes: &providers})
			req := httptest.NewRequest(http.MethodPost, "/v1/solana/recurring/enroll", strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			require.Equal(t, tc.wantStatus, w.Code, w.Body.String())
			require.Equal(t, 1, calls, "required middleware must authenticate before enrollment")
			if tc.wantStatus == http.StatusServiceUnavailable {
				require.Contains(t, w.Body.String(), "Solana recurring billing is not configured")
			}
		})
	}
}
