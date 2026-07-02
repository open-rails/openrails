package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/http/router"
)

// #692: the operator findings queue is mounted on the shared merchant action
// surface (RegisterMerchantActionRoutes), so BOTH deployments expose it —
// standalone at /v1/merchant/* and embedded at /billing/v1/merchant/*. This
// pins the permission gates on both prefixes and the unauthenticated 401.
func TestFindingsRoutesPermissionGatedOnBothSurfaces(t *testing.T) {
	const findingID = "11111111-1111-1111-1111-111111111111"
	for _, prefix := range []string{"/v1/merchant", "/billing/v1/merchant"} {
		t.Run(prefix, func(t *testing.T) {
			mux := http.NewServeMux()
			checker := &merchantActionChecker{}
			RegisterMerchantActionRoutes(router.NewMux(mux, prefix, nil), nil, Options{
				Gate: NewGate(GateOptions{
					Authenticator:          merchantActionAuth{},
					AdminPermissionChecker: checker,
				}),
			})

			tests := []struct {
				method, path, perm string
			}{
				{http.MethodGet, prefix + "/findings", controlplane.PermMerchantRepairAlertsRead},
				{http.MethodGet, prefix + "/findings/" + findingID, controlplane.PermMerchantRepairAlertsRead},
				{http.MethodPost, prefix + "/findings/" + findingID + "/resolve", controlplane.PermMerchantFindingsResolve},
			}
			for _, tc := range tests {
				checker.perm = ""
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
				mux.ServeHTTP(rec, req)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("%s %s: expected 403 before handler execution, got %d", tc.method, tc.path, rec.Code)
				}
				if checker.perm != tc.perm {
					t.Fatalf("%s %s: expected %q permission gate, got %q", tc.method, tc.path, tc.perm, checker.perm)
				}
			}
		})
	}
}

// Unauthenticated requests (no credential at all, no resolvers wired) are
// rejected 401 before any handler runs.
func TestFindingsRoutesUnauthenticatedRejected(t *testing.T) {
	mux := http.NewServeMux()
	RegisterMerchantActionRoutes(router.NewMux(mux, "/billing/v1/merchant", nil), nil, Options{
		Gate: NewGate(GateOptions{}),
	})
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/billing/v1/merchant/findings"},
		{http.MethodPost, "/billing/v1/merchant/findings/11111111-1111-1111-1111-111111111111/resolve"},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: expected 401 unauthenticated, got %d", tc.method, tc.path, rec.Code)
		}
	}
}
