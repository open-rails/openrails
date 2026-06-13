package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// TestRequiredMWPinsUserContextForHandler is the regression test for the cozy-art
// E2E finding: requiredMW authenticated the bearer, but the handler's
// r.UserContext() came back empty (the net/http Transport cached its own
// *http.Request and never saw the UserContext requiredMW stored). Handlers then
// returned 401 "User authentication required" despite a valid token.
func TestRequiredMWPinsUserContextForHandler(t *testing.T) {
	authn := billingauth.AuthenticatorFunc(func(_ context.Context, r *http.Request) (billingauth.UserContext, error) {
		if r.Header.Get("Authorization") == "" {
			return billingauth.UserContext{}, billingauth.ErrUnauthenticated
		}
		return billingauth.UserContext{UserID: "00000000-0000-4000-8000-000000000042", Username: "ada"}, nil
	})

	var sawUser string
	var sawOK bool
	mux := http.NewServeMux()
	rr := router.NewMux(mux, "/billing/v1", &app.Runtime{})
	opts := Options{Authenticator: authn}
	rr.Handle(http.MethodGet, "/me/credits", func(r *httprequest.Request) {
		uc, ok := r.UserContext() // exactly what the real handlers call
		sawOK = ok
		if ok {
			sawUser = uc.UserID
		}
		r.SuccessJSON(map[string]any{"ok": ok})
	}, opts.requiredMW())

	handler := middleware.ChainHTTP(mux,
		middleware.SecurityHeadersHTTP(),
		middleware.CORSHTTP(nil),
		middleware.BodyLimitHTTP(middleware.DefaultMaxBodyBytes),
		middleware.ResolveTenantHTTP(dbtest.TestTenantID),
	)

	req := httptest.NewRequest(http.MethodGet, "/billing/v1/me/credits", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !sawOK || sawUser != "00000000-0000-4000-8000-000000000042" {
		t.Fatalf("handler r.UserContext() = (%q, %v), want (00000000-0000-4000-8000-000000000042, true) — UserContext not pinned for the handler", sawUser, sawOK)
	}
}
