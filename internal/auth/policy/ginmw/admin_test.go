package ginmw

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/internal/auth/policy"
	"github.com/open-rails/openrails/pkg/authprovider"
)

func init() {
	gin.SetMode(gin.TestMode)
}

const permAdmin = "openrails:admin"

// fakeAdminChecker is a test double for the live policy.AdminPermissionChecker.
type fakeAdminChecker struct {
	allow map[string]bool // "tenant|user|perm" -> true
	err   error
}

func (f fakeAdminChecker) HasAdminPermission(_ context.Context, tenantSlug, userID, perm string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.allow[tenantSlug+"|"+userID+"|"+perm], nil
}

// TestAdminPermissionRequired covers the #312 hardcut: admin authority is the
// LIVE openrails:admin permission held in the CALLER'S OWN tenant — there is no
// claim-based operator-tenant gate and no JWT-role fallback.
func TestAdminPermissionRequired(t *testing.T) {
	checker := fakeAdminChecker{allow: map[string]bool{
		"acme|user-1|" + permAdmin: true,
	}}

	cases := []struct {
		name       string
		checker    policy.AdminPermissionChecker
		uc         authprovider.UserContext
		wantStatus int
		wantBody   string
	}{
		{
			name:       "no user context -> 401",
			checker:    checker,
			uc:         authprovider.UserContext{},
			wantStatus: http.StatusUnauthorized,
			wantBody:   "authentication required",
		},
		{
			name:       "nil checker -> 500 (verifier-only mode fails closed)",
			checker:    nil,
			uc:         authprovider.UserContext{UserID: "user-1", Tenant: "acme"},
			wantStatus: http.StatusInternalServerError,
			wantBody:   "authorization unavailable",
		},
		{
			name:       "authenticated but lacks live admin permission -> 403",
			checker:    checker,
			uc:         authprovider.UserContext{UserID: "user-1", Tenant: "globex"},
			wantStatus: http.StatusForbidden,
			wantBody:   "admin_permission_required",
		},
		{
			name:       "holds live admin permission in own tenant -> 200",
			checker:    checker,
			uc:         authprovider.UserContext{UserID: "user-1", Tenant: "acme"},
			wantStatus: http.StatusOK,
		},
		{
			name:       "checker error -> 500",
			checker:    fakeAdminChecker{err: errors.New("authkit down")},
			uc:         authprovider.UserContext{UserID: "user-1", Tenant: "acme"},
			wantStatus: http.StatusInternalServerError,
			wantBody:   "failed to check permission",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rr)
			c.Set("billing.user_context", tc.uc)
			c.Request = httptest.NewRequest(http.MethodGet, "/admin/test", nil)

			AdminPermissionRequired(tc.checker, permAdmin)(c)

			if !c.IsAborted() {
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
