package policy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/pkg/authprovider"
)

// fakePlatformChecker simulates the control plane's live platform-superadmin
// check. allow maps user IDs that hold openrails:platform:superadmin in the
// platform org. A tenant operator admin is simply absent from the map.
type fakePlatformChecker struct {
	allow map[string]bool
	err   error
}

func (f fakePlatformChecker) HasPlatformSuperadmin(_ context.Context, userID string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.allow[userID], nil
}

func runPlatformGate(t *testing.T, checker PlatformSuperadminChecker, uc authprovider.UserContext) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rr)
	c.Set("billing.user_context", uc)
	c.Request = httptest.NewRequest(http.MethodGet, "/platform/tenants", nil)
	PlatformSuperadminRequired(checker)(c)
	if !c.IsAborted() {
		rr.WriteHeader(http.StatusOK)
	}
	return rr
}

func TestPlatformSuperadminRequired_AllowsPlatformIdentity(t *testing.T) {
	checker := fakePlatformChecker{allow: map[string]bool{"platform-admin": true}}
	rr := runPlatformGate(t, checker, authprovider.UserContext{
		UserID: "platform-admin",
		Org:    "openrails-platform",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("platform identity should pass: got %d body=%q", rr.Code, rr.Body.String())
	}
}

func TestPlatformSuperadminRequired_DeniesTenantOperatorAdmin(t *testing.T) {
	// A tenant operator admin holds openrails:admin in a tenant operator org, but
	// NOT openrails:platform:superadmin in the platform org -> absent from allow.
	checker := fakePlatformChecker{allow: map[string]bool{"platform-admin": true}}
	rr := runPlatformGate(t, checker, authprovider.UserContext{
		UserID:   "tenant-operator-admin",
		Org:      "tenant-acme",
		OrgRoles: []string{"admin", "owner"},
	})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("tenant operator admin must be denied platform gate: got %d", rr.Code)
	}
	if !contains(rr.Body.String(), "platform_superadmin_required") {
		t.Errorf("body %q must contain platform_superadmin_required", rr.Body.String())
	}
}

func TestPlatformSuperadminRequired_Unauthenticated(t *testing.T) {
	checker := fakePlatformChecker{allow: map[string]bool{}}
	rr := runPlatformGate(t, checker, authprovider.UserContext{}) // empty UserID
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing user context must be 401: got %d", rr.Code)
	}
}

func TestPlatformSuperadminRequired_NilCheckerFailsClosed(t *testing.T) {
	rr := runPlatformGate(t, nil, authprovider.UserContext{UserID: "u1"})
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("nil checker must fail closed (500): got %d", rr.Code)
	}
}

func TestPlatformSuperadminRequired_CheckerErrorFailsClosed(t *testing.T) {
	checker := fakePlatformChecker{err: errors.New("boom")}
	rr := runPlatformGate(t, checker, authprovider.UserContext{UserID: "u1"})
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("checker error must fail closed (500): got %d", rr.Code)
	}
}
