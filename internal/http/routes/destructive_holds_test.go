package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/stretchr/testify/require"
)

func TestIncidentClearRequiresInteractiveRootOperator(t *testing.T) {
	human := authtest.User(userA)
	key := human
	key.Credential = billingauth.Credential{Kind: billingauth.CredentialAPIKey, ID: "personal"}
	delegated := human
	delegated.Invoker = billingauth.Invoker{Issuer: "external", ID: "delegate"}
	for _, tt := range []struct {
		name     string
		who      billingauth.Identity
		operator bool
		status   int
	}{
		{"config manager", human, false, http.StatusForbidden},
		{"application", authtest.Application("ops-app"), true, http.StatusForbidden},
		{"personal API key", key, true, http.StatusForbidden},
		{"delegated user", delegated, true, http.StatusForbidden},
		{"interactive root operator", human, true, http.StatusServiceUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			perms := map[billing.Permission]bool{billing.RootConfigManage: true}
			if tt.operator {
				perms[billing.RootAdminRateLimitsUnlock] = true
			}
			auth := &recordingAuth{who: tt.who, allowed: perms}
			rt := gatedRuntime(t)
			env := newEnv(rt, Options{Auth: auth, Groups: everyGroup, live: live})
			route, ok := Lookup(http.MethodPost, "/v1/merchant/destructive-holds/{id}/clear")
			require.True(t, ok)
			table := &router.Table{}
			router.NewMux(table, "", rt).Handle(route.Method, route.Path, env.Guarded(route), env.gates(route)...)
			req := httptest.NewRequest(http.MethodPost, "/v1/merchant/destructive-holds/dsh_00000000-0000-4000-8000-000000000001/clear", strings.NewReader(`{"reason":"reviewed"}`))
			req.Header.Set("Authorization", "Bearer fixture")
			rec := httptest.NewRecorder()
			table.Handler().ServeHTTP(rec, req)
			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			// Authorized reaches the deliberately unwired incident store. Others stop
			// before it; this does not claim a database clear succeeded.
		})
	}
}
