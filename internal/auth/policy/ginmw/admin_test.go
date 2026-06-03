package ginmw

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/pkg/authprovider"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// TestOperatorAdminRequired_OperatorOrgMode focuses on the operator-org branch,
// which is the ONLY admin-authority path after the hardcut (the legacy
// global-`admin` DB fallback has been removed).
func TestOperatorAdminRequired_OperatorOrgMode(t *testing.T) {
	cfg := &config.Config{
		Auth: &config.AuthConfig{
			OperatorOrgSlug:       "acme",
			OperatorOrgAdminRoles: []string{"admin", "owner"},
		},
	}

	cases := []struct {
		name       string
		uc         authprovider.UserContext
		wantStatus int
		wantBody   string // substring check
	}{
		{
			name:       "no user context attached -> 401",
			uc:         authprovider.UserContext{}, // empty UserID
			wantStatus: http.StatusUnauthorized,
			wantBody:   "authentication required",
		},
		{
			name: "user has no org claim -> 403 operator_org_required",
			uc: authprovider.UserContext{
				UserID: "user-1",
				// Org and OrgRoles intentionally empty
			},
			wantStatus: http.StatusForbidden,
			wantBody:   "operator_org_required",
		},
		{
			name: "user has org but it does not match operator slug -> 403 operator_org_mismatch",
			uc: authprovider.UserContext{
				UserID:   "user-1",
				Org:      "globex",
				OrgRoles: []string{"admin"},
			},
			wantStatus: http.StatusForbidden,
			wantBody:   "operator_org_mismatch",
		},
		{
			name: "user in correct org but lacks admin role -> 403 operator_org_role_required",
			uc: authprovider.UserContext{
				UserID:   "user-1",
				Org:      "acme",
				OrgRoles: []string{"member", "viewer"},
			},
			wantStatus: http.StatusForbidden,
			wantBody:   "operator_org_role_required",
		},
		{
			name: "user in correct org with admin role -> 200 (passes through)",
			uc: authprovider.UserContext{
				UserID:   "user-1",
				Org:      "acme",
				OrgRoles: []string{"admin"},
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "user in correct org with owner role -> 200 (default admin-equivalent set)",
			uc: authprovider.UserContext{
				UserID:   "user-1",
				Org:      "acme",
				OrgRoles: []string{"owner"},
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "case-insensitive role match -> 200",
			uc: authprovider.UserContext{
				UserID:   "user-1",
				Org:      "acme",
				OrgRoles: []string{"ADMIN"},
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "case-insensitive org match -> 200",
			uc: authprovider.UserContext{
				UserID:   "user-1",
				Org:      "Acme",
				OrgRoles: []string{"admin"},
			},
			wantStatus: http.StatusOK,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			c, engine := gin.CreateTestContext(rr)

			// Attach UserContext to the gin context the way real auth middleware would.
			c.Set("billing.user_context", tc.uc)

			// Build a real request because the middleware reads c.Request.Context().
			req := httptest.NewRequest(http.MethodGet, "/admin/test", nil)
			c.Request = req

			handler := OperatorAdminRequired(cfg, nil)
			handler(c)

			// If the middleware did not abort, call a stub final handler.
			if !c.IsAborted() {
				engine.GET("/admin/test", func(c *gin.Context) {
					c.String(http.StatusOK, "ok")
				})
				// Pretend the next handler ran successfully.
				c.String(http.StatusOK, "ok")
			}

			if rr.Code != tc.wantStatus {
				t.Errorf("status: got %d want %d (body=%q)", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if tc.wantBody != "" && !contains(rr.Body.String(), tc.wantBody) {
				t.Errorf("body %q does not contain %q", rr.Body.String(), tc.wantBody)
			}
		})
	}
}

// TestOperatorAdminRequired_NoOperatorOrgFailsClosed proves the hardcut: with no
// operator org configured there is NO DB-role fallback — OperatorAdminRequired
// fails closed with admin_required WITHOUT consulting profiles.user_roles (db ==
// nil here, so any DB consultation would panic). Authority comes from the
// operator-org permission layer (OperatorPermissionRequired) at the route.
func TestOperatorAdminRequired_NoOperatorOrgFailsClosed(t *testing.T) {
	cfg := &config.Config{
		Auth: &config.AuthConfig{
			ControlPlane: &config.ControlPlaneConfig{Enabled: true},
		},
	}

	rr := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rr)
	c.Set("billing.user_context", authprovider.UserContext{UserID: "global-admin-user"})
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/test", nil)

	// db is nil: if the middleware fell through to a DB role check it would panic.
	OperatorAdminRequired(cfg, nil)(c)

	if !c.IsAborted() {
		t.Fatal("expected no-operator-org config to abort (fail closed), but it passed through")
	}
	if rr.Code != http.StatusForbidden {
		t.Errorf("status: got %d want 403", rr.Code)
	}
	if !contains(rr.Body.String(), "admin_required") {
		t.Errorf("body %q does not contain admin_required", rr.Body.String())
	}
}

func contains(s, substr string) bool {
	return len(substr) == 0 || (len(s) >= len(substr) && (indexOf(s, substr) >= 0))
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
