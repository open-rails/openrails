package embed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

func TestReviewCustomerExposureCookieAdmission(t *testing.T) {
	calls := 0
	authn := billingauth.DelegatedAuthenticatorFunc(func(_ context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
		calls++
		if _, err := r.Cookie("session"); err != nil {
			return nil, billingauth.ErrUnauthenticated
		}
		return &billingauth.DelegatedPrincipal{MerchantID: "11111111-1111-4111-8111-111111111111", SubjectID: "22222222-2222-4222-8222-222222222222"}, nil
	})
	runtime := reviewRuntime(nil, nil)
	require.NoError(t, runtime.configureHTTP(HTTPConfig{CustomerRoutes: []CustomerRoutesConfig{{Prefix: "/portal", Scope: CustomerSubscriptionManagement, DelegatedAuthenticator: authn}}}))
	mux := reviewMount(t, runtime)
	admission, err := billingauth.CookieAuthentication("https://portal.example")
	require.NoError(t, err)
	for _, tc := range []struct {
		name, origin, bearer string
		admitted             bool
		code, authCalls      int
	}{
		{name: "ambient cookie stripped", code: 401, authCalls: 1},
		{name: "missing origin", admitted: true, code: 403},
		{name: "foreign origin", origin: "https://other.example", admitted: true, code: 403},
		{name: "admitted session", origin: "https://portal.example", admitted: true, code: 400, authCalls: 1},
		{name: "explicit credential cannot fall back", origin: "https://portal.example", bearer: "Bearer invalid", admitted: true, code: 401, authCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls = 0
			req := httptest.NewRequest(http.MethodPost, "/api/pay/portal/subscriptions/not-id/cancel", strings.NewReader("{}"))
			req.AddCookie(&http.Cookie{Name: "session", Value: "valid"})
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.bearer != "" {
				req.Header.Set("Authorization", tc.bearer)
			}
			var target http.Handler = mux
			if tc.admitted {
				target = admission(target)
			}
			rec := httptest.NewRecorder()
			target.ServeHTTP(rec, req)
			require.Equal(t, tc.code, rec.Code, rec.Body.String())
			require.Equal(t, tc.authCalls, calls)
		})
	}
}
