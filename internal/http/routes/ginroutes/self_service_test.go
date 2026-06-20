package ginroutes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	"github.com/open-rails/openrails/internal/merchants"
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

func newMerchantAdminRouter(t *testing.T, perms []string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	group := e.Group("/v1/admin")
	RegisterAdminRoutes(group, nil, ginmw.DelegatedSelfRequired(fakeDelegatedResolver{permissions: perms}))
	return e
}

func newMerchantAdminRouterWithRuntime(t *testing.T, perms []string, merchantID merchantpkg.ID, svc *merchants.Service) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	group := e.Group("/v1/admin")
	rt := &app.Runtime{Merchants: svc}
	RegisterAdminRoutes(group, rt, ginmw.DelegatedSelfRequired(fakeDelegatedResolver{permissions: perms, merchantID: merchantID}))
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
		{"solana-wallet-link", http.MethodPut, "/v1/me/wallets/solana"},
		{"solana-wallet-unlink", http.MethodDelete, "/v1/me/wallets/solana"},
		{"usdc-funding-create", http.MethodPost, "/v1/me/usdc-funding-sessions"},
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

func TestMerchantAdmin_OperationalListsMountedAndGated(t *testing.T) {
	readless := []string{}

	cases := []struct {
		name string
		path string
	}{
		{"subscriptions-list", "/v1/admin/subscriptions"},
		{"subscription-detail", "/v1/admin/subscriptions/sub_123"},
		{"repair-alerts", "/v1/admin/repair-alerts"},
		{"manual-rebill-attempts", "/v1/admin/manual-rebill-attempts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newMerchantAdminRouter(t, readless)
			w := doSelf(e, http.MethodGet, tc.path, true)
			require.Equal(t, http.StatusForbidden, w.Code,
				"%s must be mounted and gated (403, not 404): %s", tc.name, w.Body.String())
		})
	}
}

func TestMerchantAdmin_PaymentWriteRoutesMountedAndGated(t *testing.T) {
	readOnly := []string{controlplane.PermMerchantBillingRead}

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"off-channel-payment", http.MethodPost, "/v1/admin/users/user_123/payments/off-channel"},
		{"payment-refund", http.MethodPost, "/v1/admin/payments/pay_123/refunds"},
		{"subscription-cancel", http.MethodDelete, "/v1/admin/subscriptions/sub_123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newMerchantAdminRouter(t, readOnly)
			w := doSelfBearerBody(e, tc.method, tc.path, "delegated.jwt.token", `{}`)
			require.Equal(t, http.StatusForbidden, w.Code,
				"%s must be mounted and gated (403, not 404): %s", tc.name, w.Body.String())
		})
	}
}

func TestMerchantAdmin_SecretRoutesMountedAndGated(t *testing.T) {
	readless := []string{controlplane.PermMerchantBillingRead}

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"secret-status", http.MethodGet, "/v1/admin/secrets"},
		{"secret-registry", http.MethodGet, "/v1/admin/secrets/registry"},
		{"secret-put", http.MethodPut, "/v1/admin/secrets/stripe/secret_key"},
		{"secret-delete", http.MethodDelete, "/v1/admin/secrets/stripe/secret_key"},
		{"secret-validate", http.MethodPost, "/v1/admin/secrets/validate/stripe/secret_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newMerchantAdminRouter(t, readless)
			w := doSelf(e, tc.method, tc.path, true)
			require.Equal(t, http.StatusForbidden, w.Code,
				"%s must be mounted and gated (403, not 404): %s", tc.name, w.Body.String())
		})
	}
}

func TestMerchantAdmin_ConfigurationRoutesMountedAndGated(t *testing.T) {
	readless := []string{controlplane.PermMerchantBillingRead}

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"configuration-get", http.MethodGet, "/v1/admin/merchant-configuration"},
		{"configuration-put", http.MethodPut, "/v1/admin/merchant-configuration"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newMerchantAdminRouter(t, readless)
			w := doSelf(e, tc.method, tc.path, true)
			require.Equal(t, http.StatusForbidden, w.Code,
				"%s must be mounted and gated (403, not 404): %s", tc.name, w.Body.String())
		})
	}
}

func TestMerchantAdmin_ConfigurationPermissionsAreDistinct(t *testing.T) {
	cases := []struct {
		name   string
		perms  []string
		method string
		path   string
	}{
		{"read", []string{controlplane.PermMerchantConfigurationRead}, http.MethodGet, "/v1/admin/merchant-configuration"},
		{"write", []string{controlplane.PermMerchantConfigurationWrite}, http.MethodPut, "/v1/admin/merchant-configuration"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newMerchantAdminRouter(t, tc.perms)
			w := doSelf(e, tc.method, tc.path, true)
			require.NotEqual(t, http.StatusForbidden, w.Code, w.Body.String())
			require.NotEqual(t, http.StatusNotFound, w.Code, w.Body.String())
		})
	}
}

func TestMerchantAdmin_SecretPermissionsAreDistinct(t *testing.T) {
	cases := []struct {
		name   string
		perms  []string
		method string
		path   string
	}{
		{"list", []string{controlplane.PermMerchantSecretsList}, http.MethodGet, "/v1/admin/secrets"},
		{"registry", []string{controlplane.PermMerchantSecretsList}, http.MethodGet, "/v1/admin/secrets/registry"},
		{"write", []string{controlplane.PermMerchantSecretsWrite}, http.MethodPut, "/v1/admin/secrets/stripe/secret_key"},
		{"delete", []string{controlplane.PermMerchantSecretsDelete}, http.MethodDelete, "/v1/admin/secrets/stripe/secret_key"},
		{"test", []string{controlplane.PermMerchantSecretsTest}, http.MethodPost, "/v1/admin/secrets/validate/stripe/secret_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newMerchantAdminRouter(t, tc.perms)
			w := doSelf(e, tc.method, tc.path, true)
			require.NotEqual(t, http.StatusForbidden, w.Code, w.Body.String())
			require.NotEqual(t, http.StatusNotFound, w.Code, w.Body.String())
		})
	}
}

func TestMerchantAdmin_WritableRegistryExcludesOpenRailsInternalSecrets(t *testing.T) {
	require.True(t, merchantSecretWritable(merchants.SecretStripeSecretKey))
	require.False(t, merchantSecretWritable(merchants.SecretSolanaPrivateKey))

	registry := merchantWritableSecretRegistry()
	for _, def := range registry {
		require.True(t, def.MerchantWritable, "merchant-admin registry exposed non-writable secret %s", def.Name)
		require.NotEqual(t, merchants.SecretSolanaPrivateKey, def.Name)
	}
}

func TestMerchantAdmin_SecretRoutesWriteOnlyRuntimeBehavior(t *testing.T) {
	store := merchants.NewMemorySecretStore()
	svc, err := merchants.NewSecretManagementService(store)
	require.NoError(t, err)

	merchantA := dbtest.TestMerchantID
	merchantB, err := merchantpkg.ParseID("22222222-2222-2222-2222-222222222222")
	require.NoError(t, err)
	perms := []string{
		controlplane.PermMerchantSecretsList,
		controlplane.PermMerchantSecretsWrite,
		controlplane.PermMerchantSecretsDelete,
		controlplane.PermMerchantSecretsTest,
	}

	eA := newMerchantAdminRouterWithRuntime(t, perms, merchantA, svc)
	w := doSelfBearerBody(eA, http.MethodPut, "/v1/admin/secrets/stripe/secret_key", "delegated.jwt.token", `{"value":"sk_test_route_secret"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NotContains(t, w.Body.String(), "sk_test_route_secret")
	got, err := store.Get(context.Background(), merchantA, merchants.SecretStripeSecretKey)
	require.NoError(t, err)
	require.Equal(t, "sk_test_route_secret", got.Value)

	scopedName, err := merchants.ProviderAccountSecretName("stripe", "live", "acct_route_secret", "secret_key")
	require.NoError(t, err)
	w = doSelfBearerBody(eA, http.MethodPut, "/v1/admin/secrets/"+scopedName, "delegated.jwt.token", `{"value":"sk_test_scoped_route_secret"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NotContains(t, w.Body.String(), "sk_test_scoped_route_secret")
	got, err = store.Get(context.Background(), merchantA, scopedName)
	require.NoError(t, err)
	require.Equal(t, "sk_test_scoped_route_secret", got.Value)

	w = doSelfBearer(eA, http.MethodGet, "/v1/admin/secrets", "delegated.jwt.token")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"configured":true`)
	require.Contains(t, w.Body.String(), scopedName)
	require.NotContains(t, w.Body.String(), "sk_test_route_secret")
	require.NotContains(t, w.Body.String(), "sk_test_scoped_route_secret")

	w = doSelfBearerBody(eA, http.MethodPost, "/v1/admin/secrets/validate/nmi/mobius/production_key", "delegated.jwt.token", `{"value":"mobius-production-key"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NotContains(t, w.Body.String(), "mobius-production-key")

	w = doSelfBearerBody(eA, http.MethodPut, "/v1/admin/secrets/solana/private_key", "delegated.jwt.token", `{"value":"private"}`)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())

	eB := newMerchantAdminRouterWithRuntime(t, perms, merchantB, svc)
	w = doSelfBearer(eB, http.MethodGet, "/v1/admin/secrets", "delegated.jwt.token")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NotContains(t, w.Body.String(), `"version":1`)
	if _, err := store.Get(context.Background(), merchantB, merchants.SecretStripeSecretKey); err == nil {
		t.Fatal("merchant B should not see merchant A's Stripe secret")
	}

	w = doSelfBearer(eA, http.MethodDelete, "/v1/admin/secrets/stripe/secret_key", "delegated.jwt.token")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NotContains(t, w.Body.String(), "sk_test_route_secret")
	_, err = store.Get(context.Background(), merchantA, merchants.SecretStripeSecretKey)
	require.ErrorIs(t, err, merchants.ErrSecretNotFound)

	w = doSelfBearer(eA, http.MethodDelete, "/v1/admin/secrets/"+scopedName, "delegated.jwt.token")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NotContains(t, w.Body.String(), "sk_test_scoped_route_secret")
	_, err = store.Get(context.Background(), merchantA, scopedName)
	require.ErrorIs(t, err, merchants.ErrSecretNotFound)
}
