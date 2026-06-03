package ginroutes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
)

// These tests pin the SELF-SERVICE route table (RegisterSelfServiceRoutes) for
// the browser-direct `/v1/self/*` surface that doujins/hentai0 call with
// delegated tokens (issues #215/#216 consumer gap). The gap closed here: resume,
// change-tier, and update-subscription-payment-method were previously mounted
// ONLY on the legacy login-JWT `/me/*` group, so a browser holding a delegated
// token could cancel a subscription but never undo it.
//
// The assertions exercise the per-route permission gates, which run BEFORE the
// wrapped handler — so no Runtime/services are needed: a denied request (403) or
// an unauthenticated one (401) never reaches the handler body. A route that was
// NOT mounted would 404 instead, which is exactly the regression we guard.

// fakeDelegatedResolver implements ginmw.DelegatedResolver: it returns a
// fixed ResolvedDelegated carrying the supplied permission set.
type fakeDelegatedResolver struct {
	permissions []string
	err         error
}

func (f fakeDelegatedResolver) ResolveDelegated(context.Context, string) (*controlplane.ResolvedDelegated, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &controlplane.ResolvedDelegated{
		Tenant:           "acme-org",
		DelegatedSubject: "user-42",
		Permissions:      f.permissions,
	}, nil
}

func newSelfRouter(t *testing.T, perms []string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	group := e.Group("/v1/self")
	// rt==nil: TenantDBConn is skipped, and the wrapped handlers are never
	// reached on the 401/403 paths these tests assert.
	RegisterSelfServiceRoutes(group, nil, ginmw.DelegatedSelfRequired(fakeDelegatedResolver{permissions: perms}))
	return e
}

func newTenantAdminRouter(t *testing.T, perms []string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	group := e.Group("/v1/tenant-admin")
	RegisterTenantAdminRoutes(group, nil, ginmw.DelegatedSelfRequired(fakeDelegatedResolver{permissions: perms}))
	return e
}

func doSelf(e *gin.Engine, method, path string, withAuth bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	if withAuth {
		req.Header.Set("Authorization", "Bearer delegated.jwt.token")
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

// The resume / change-tier / payment-method mutations are now MOUNTED on the
// self surface. Without the required permission the per-route gate returns 403
// (NOT 404) — proving the route exists and is gated, not absent.
func TestSelfService_SubscriptionMutationsMountedAndGated(t *testing.T) {
	// A read-only token: holds neither the cancel scope nor the payment scope.
	readOnly := []string{controlplane.PermSelfBillingRead}

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"resume", http.MethodPost, "/v1/self/subscriptions/sub_123/resume"},
		{"change-tier", http.MethodPost, "/v1/self/subscriptions/sub_123/change-tier"},
		{"update-payment-method", http.MethodPut, "/v1/self/subscriptions/sub_123/payment-method"},
		{"cancel", http.MethodPost, "/v1/self/subscriptions/sub_123/cancel"},
		{"solana-cancel-tx", http.MethodPost, "/v1/self/subscriptions/sub_123/solana-cancel-tx"},
		{"solana-cancel-confirm", http.MethodPost, "/v1/self/subscriptions/sub_123/solana-cancel"},
		{"solana-tier-change", http.MethodPost, "/v1/self/subscriptions/sub_123/solana-tier-change"},
		{"solana-tier-change-confirm", http.MethodPost, "/v1/self/subscriptions/sub_123/solana-tier-change/confirm"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newSelfRouter(t, readOnly)
			w := doSelf(e, tc.method, tc.path, true)
			require.Equal(t, http.StatusForbidden, w.Code,
				"%s must be mounted and gated (403, not 404): %s", tc.name, w.Body.String())
		})
	}
}

// resume and change-tier ride the SAME subscription-cancel scope as cancel: a
// token holding openrails:self:subscriptions:cancel passes the gate (so it does
// NOT 403/404 at the gate). With rt==nil the handler would panic if reached, so
// we only assert the gate admits the request — proven by it not being 403.
func TestSelfService_CancelScopeAdmitsResumeAndChangeTier(t *testing.T) {
	perms := []string{controlplane.PermSelfSubscriptionCancel}
	for _, path := range []string{
		"/v1/self/subscriptions/sub_123/resume",
		"/v1/self/subscriptions/sub_123/change-tier",
	} {
		e := newSelfRouter(t, perms)
		// Recover the handler panic (nil runtime) so we can assert the GATE
		// admitted the request rather than rejecting it.
		func() {
			defer func() { _ = recover() }()
			w := doSelf(e, http.MethodPost, path, true)
			require.NotEqual(t, http.StatusForbidden, w.Code, "cancel scope must admit %s", path)
			require.NotEqual(t, http.StatusNotFound, w.Code, "%s must be mounted", path)
		}()
	}
}

// payment-method update requires the payment-methods:manage scope — the cancel
// scope must NOT be sufficient for it (and vice versa), confirming the gates are
// distinct.
func TestSelfService_PaymentMethodRequiresManageScope(t *testing.T) {
	// Holding only the cancel scope: payment-method update is forbidden.
	e := newSelfRouter(t, []string{controlplane.PermSelfSubscriptionCancel})
	w := doSelf(e, http.MethodPut, "/v1/self/subscriptions/sub_123/payment-method", true)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}

// No token at all is rejected by the delegated auth middleware before any gate.
func TestSelfService_ResumeRejectedWithoutToken(t *testing.T) {
	e := newSelfRouter(t, []string{controlplane.PermSelfSubscriptionCancel})
	w := doSelf(e, http.MethodPost, "/v1/self/subscriptions/sub_123/resume", false)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestTenantAdmin_OperationalListsMountedAndGated(t *testing.T) {
	readless := []string{controlplane.PermSelfBillingRead}

	cases := []struct {
		name string
		path string
	}{
		{"subscriptions-list", "/v1/tenant-admin/subscriptions"},
		{"subscription-detail", "/v1/tenant-admin/subscriptions/sub_123"},
		{"repair-alerts", "/v1/tenant-admin/repair-alerts"},
		{"manual-rebill-attempts", "/v1/tenant-admin/manual-rebill-attempts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTenantAdminRouter(t, readless)
			w := doSelf(e, http.MethodGet, tc.path, true)
			require.Equal(t, http.StatusForbidden, w.Code,
				"%s must be mounted and gated (403, not 404): %s", tc.name, w.Body.String())
		})
	}
}
