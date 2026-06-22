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
	"github.com/open-rails/openrails/internal/dbtest"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	merchantpkg "github.com/open-rails/openrails/pkg/merchant"
)

// These tests pin the SELF-SERVICE route table (RegisterSelfServiceRoutes) for
// the browser-direct `/v1/me/*` surface that host apps call with
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
	merchantID  merchantpkg.ID
	err         error
}

func (f fakeDelegatedResolver) ResolveDelegated(context.Context, string, string) (*controlplane.ResolvedDelegated, error) {
	if f.err != nil {
		return nil, f.err
	}
	merchantID := f.merchantID
	if merchantID.IsZero() {
		merchantID = dbtest.TestMerchantID
	}
	return &controlplane.ResolvedDelegated{
		Merchant:         "acme-org",
		MerchantID:       merchantID,
		MerchantSlug:     "acme-org",
		DelegatedSubject: "user-42",
		Permissions:      f.permissions,
	}, nil
}

func newSelfRouter(t *testing.T, perms []string) *gin.Engine {
	t.Helper()
	return newSelfRouterWithResolver(t, fakeDelegatedResolver{permissions: perms})
}

func newSelfRouterWithResolver(t *testing.T, resolver ginmw.DelegatedResolver) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	group := e.Group("/v1/me")
	// rt==nil: MerchantDBConn is skipped, and the wrapped handlers are never
	// reached on the 401/403 paths these tests assert.
	RegisterSelfServiceRoutes(group, nil, ginmw.DelegatedSelfRequired(resolver))
	return e
}

func newOrgTreasuryRouter(t *testing.T, perms []string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	group := e.Group("/v1/orgs")
	RegisterOrgTreasuryRoutes(group, nil, ginmw.DelegatedSelfRequired(fakeDelegatedResolver{permissions: perms}))
	return e
}

func doSelf(e *gin.Engine, method, path string, withAuth bool) *httptest.ResponseRecorder {
	token := ""
	if withAuth {
		token = "delegated.jwt.token"
	}
	return doSelfBearer(e, method, path, token)
}

func doSelfBearer(e *gin.Engine, method, path, token string) *httptest.ResponseRecorder {
	return doSelfBearerBody(e, method, path, token, "{}")
}

func doSelfBearerBody(e *gin.Engine, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

func TestSelfService_PermissionlessPrincipalReachesMountedSelfRoutes(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"resume", http.MethodPost, "/v1/me/subscriptions/sub_123/resume"},
		{"change-tier", http.MethodPost, "/v1/me/subscriptions/sub_123/change-tier"},
		{"update-payment-method", http.MethodPut, "/v1/me/subscriptions/sub_123/payment-method"},
		{"cancel", http.MethodPost, "/v1/me/subscriptions/sub_123/cancel"},
		{"solana-cancel-tx", http.MethodPost, "/v1/me/subscriptions/sub_123/solana-cancel-tx"},
		{"solana-cancel-confirm", http.MethodPost, "/v1/me/subscriptions/sub_123/solana-cancel"},
		{"solana-tier-change", http.MethodPost, "/v1/me/subscriptions/sub_123/solana-tier-change"},
		{"solana-tier-change-confirm", http.MethodPost, "/v1/me/subscriptions/sub_123/solana-tier-change/confirm"},
		{"balance", http.MethodGet, "/v1/me/balance"},
		{"transactions", http.MethodGet, "/v1/me/transactions"},
		{"settings", http.MethodPut, "/v1/me/settings"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newSelfRouter(t, nil)
			func() {
				defer func() { _ = recover() }()
				w := doSelf(e, tc.method, tc.path, true)
				require.NotEqual(t, http.StatusUnauthorized, w.Code, "%s authenticated principal rejected", tc.name)
				require.NotEqual(t, http.StatusForbidden, w.Code, "%s must not require self permissions", tc.name)
				require.NotEqual(t, http.StatusNotFound, w.Code, "%s must be mounted", tc.name)
			}()
		})
	}
}

// No token at all is rejected by the delegated auth middleware before any gate.
func TestSelfService_ResumeRejectedWithoutToken(t *testing.T) {
	e := newSelfRouter(t, nil)
	w := doSelf(e, http.MethodPost, "/v1/me/subscriptions/sub_123/resume", false)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestSelfService_RejectsServiceCredential(t *testing.T) {
	e := newSelfRouterWithResolver(t, fakeDelegatedResolver{err: controlplane.ErrDelegatedInvalid})
	w := doSelfBearer(e, http.MethodPost, "/v1/me/subscriptions/sub_123/resume", "openrails_st_keyid_secret")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "delegated_token_invalid")
}

func TestOrgTreasurySpendDelegationsMountedAndGated(t *testing.T) {
	e := newOrgTreasuryRouter(t, nil)
	w := doSelf(e, http.MethodGet, "/v1/orgs/acme-org/spend-delegations", false)
	require.Equal(t, http.StatusUnauthorized, w.Code)

	e = newOrgTreasuryRouter(t, []string{controlplane.PermMerchantCustomerSettingsRead})
	w = doSelf(e, http.MethodGet, "/v1/orgs/acme-org/spend-delegations", true)
	require.Equal(t, http.StatusForbidden, w.Code, "merchant:* must not satisfy customer treasury")

	e = newOrgTreasuryRouter(t, []string{controlplane.PermCustomerSpendDelegationsRead})
	w = doSelf(e, http.MethodGet, "/v1/orgs/other-org/spend-delegations", true)
	require.Equal(t, http.StatusForbidden, w.Code, "caller must be scoped to the org in the path")

	w = doSelf(e, http.MethodGet, "/v1/orgs/acme-org/spend-delegations", true)
	require.NotEqual(t, http.StatusUnauthorized, w.Code, w.Body.String())
	require.NotEqual(t, http.StatusForbidden, w.Code, w.Body.String())
	require.NotEqual(t, http.StatusNotFound, w.Code, w.Body.String())

	w = doSelf(e, http.MethodPut, "/v1/orgs/acme-org/spend-delegations", true)
	require.Equal(t, http.StatusForbidden, w.Code, "read permission must not satisfy update")
}

func TestOrgTreasurySpendDelegationsRejectPersonalCustomerID(t *testing.T) {
	e := newOrgTreasuryRouter(t, []string{controlplane.PermCustomerSpendDelegationsUpdate})
	body := `{
		"customer_id": "11111111-1111-1111-1111-111111111111",
		"delegations": [{
			"scope": "invoker",
			"scope_key": "22222222-2222-2222-2222-222222222222",
			"windows": [{"key": "day", "window_seconds": 86400, "limit": 1000, "currency": "USD"}]
		}]
	}`
	w := doSelfBearerBody(e, http.MethodPut, "/v1/orgs/acme-org/spend-delegations", "delegated.jwt.token", body)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "customer_id")

	body = `{
		"delegations": [{
			"scope": "invoker",
			"scope_key": "22222222-2222-2222-2222-222222222222",
			"customer_id": "11111111-1111-1111-1111-111111111111",
			"windows": [{"key": "day", "window_seconds": 86400, "limit": 1000, "currency": "USD"}]
		}]
	}`
	w = doSelfBearerBody(e, http.MethodPut, "/v1/orgs/acme-org/spend-delegations", "delegated.jwt.token", body)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "customer_id")
}
