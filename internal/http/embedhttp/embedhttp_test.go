package embedhttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/internal/requestauth"
)

func identityAuth(id billingauth.Identity, calls *int) *billingauth.Integration {
	return &billingauth.Integration{Authentication: billingauth.AuthenticationFunc(func(context.Context, *http.Request) (billingauth.Identity, error) {
		if calls != nil {
			*calls++
		}
		return id, nil
	})}
}

func TestCapabilities(t *testing.T) {
	h := CapabilitiesHandler(nil, []RouteSet{RouteSetCheckout, RouteSetCustomer, RouteSetWebhooks}, routesurface.ProviderRoutes{Solana: true}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/billing/v1/capabilities", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "public, max-age=300", rec.Header().Get("Cache-Control"))
	var caps Capabilities
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &caps))
	require.Len(t, caps.RouteGroups, len(AllRouteSets), "every known group is reported")
	for _, rs := range AllRouteSets {
		require.Equal(t, rs == RouteSetCheckout || rs == RouteSetCustomer || rs == RouteSetWebhooks, caps.RouteGroups[string(rs)], rs)
	}
	require.Equal(t, map[string]bool{
		"solana_one_time_payments": true, "stripe_billing_portal": false,
		"solana_subscription_management": false, "provider_credential_writes": false,
		"api_host": false, "catalog_copilot": false, "metrics_ask": false, "dashboard_generation": false,
	}, caps.Features)

	req := httptest.NewRequest(http.MethodGet, "/billing/v1/capabilities", nil)
	req.Header.Set("If-None-Match", rec.Header().Get("ETag"))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotModified, rec.Code)

	// Customer features follow the mounted customer scope, not provider support.
	for _, tc := range []struct {
		scope          config.CustomerHTTPScope
		portal, solana bool
	}{
		{config.CustomerSelfService, true, true},
		{config.CustomerBillingManagement, false, true},
		{config.CustomerSubscriptionManagement, false, true},
	} {
		caps := configuredCapabilities(nil, config.HTTPConfig{CustomerRoutes: []config.CustomerRoutesConfig{{Scope: tc.scope}}}, routesurface.AllProviderRoutes())
		require.True(t, caps.RouteGroups[string(RouteSetCustomer)])
		require.Equal(t, tc.portal, caps.Features["stripe_billing_portal"], tc.scope)
		require.Equal(t, tc.solana, caps.Features["solana_subscription_management"], tc.scope)
		require.False(t, caps.Features["provider_credential_writes"], "credential writes need the merchant group")
	}

	require.Equal(t, AllRouteSets, ResolveRouteSets(nil))
	require.Equal(t, []RouteSet{RouteSetCheckout, RouteSetWebhooks}, ResolveRouteSets([]RouteSet{RouteSetCheckout, "", RouteSetCheckout, RouteSetWebhooks}))
}

// HTTP publication is refused unless each surface has the authority it needs.
func TestHTTPConfigValidation(t *testing.T) {
	authn := identityAuth(billingauth.Identity{}, nil)
	full := &billingauth.Integration{Authentication: authn.Authentication, Authorization: billingauth.AuthorizationFunc(func(context.Context, *http.Request, billingauth.Identity, billingauth.Requirement) error { return nil })}
	customer := func(c config.CustomerRoutesConfig) *config.HTTPConfig {
		return &config.HTTPConfig{CustomerRoutes: []config.CustomerRoutesConfig{c}}
	}
	for _, tc := range []struct {
		name string
		cfg  *config.HTTPConfig
		auth *billingauth.Integration
		ok   bool
	}{
		{"no HTTP", nil, nil, true},
		{"checkout needs no authenticator: a session id is its credential", &config.HTTPConfig{Checkout: &config.CheckoutConfig{}}, nil, true},
		{"checkout", &config.HTTPConfig{Checkout: &config.CheckoutConfig{}}, authn, true},
		{"merchant without authorization", &config.HTTPConfig{Merchant: true}, authn, false},
		{"merchant", &config.HTTPConfig{Merchant: true}, full, true},
		{"customer without any authenticator", customer(config.CustomerRoutesConfig{Merchant: "store"}), nil, false},
		{"native customer without merchant", customer(config.CustomerRoutesConfig{}), authn, false},
		{"native customer", customer(config.CustomerRoutesConfig{Merchant: "store"}), authn, true},
		{"unknown scope", customer(config.CustomerRoutesConfig{Scope: 9, Delegated: true}), nil, false},
		{"parameterized prefix", customer(config.CustomerRoutesConfig{Prefix: "/v1/tenants/{tenant}/me", Delegated: true}), nil, true},
	} {
		require.Equal(t, tc.ok, ValidateHTTPConfig(tc.cfg, tc.auth) == nil, tc.name)
	}
	for _, prefix := range []string{"/", "me", "/a/../b", "/a/", "/a/*", "/a b", "/a/{x.y}", "/a/b{c}", "/a/{}"} {
		require.Error(t, ValidateHTTPConfig(customer(config.CustomerRoutesConfig{Prefix: prefix, Delegated: true}), nil), prefix)
	}
}

// The combined handler refuses to mount a surface without its auth boundary,
// and only checkout routes join the permissive-CORS browser tier.
func TestNewRoutes(t *testing.T) {
	require.Panics(t, func() { (&Assembler{}).NewRoutes(Options{RouteSets: []RouteSet{RouteSetCustomer}}) })
	require.Panics(t, func() {
		(&Assembler{Authenticator: billingauth.AuthenticatorFunc(nil)}).NewRoutes(Options{RouteSets: []RouteSet{RouteSetMerchant}})
	})

	gate := billingauth.Gate(IntegrationGate(&app.Runtime{}))
	asm := &Assembler{Runtime: &app.Runtime{Config: &config.Config{}}, Authenticator: billingauth.AuthenticatorFunc(func(context.Context, *http.Request) (billingauth.UserContext, error) {
		return billingauth.UserContext{}, billingauth.ErrUnauthenticated
	}), Gate: gate}
	noWebhooks := routesurface.ProviderRoutes{}
	table := asm.NewRoutes(Options{RouteSets: []RouteSet{RouteSetCheckout, RouteSetMerchant, RouteSetWebhooks}, ProviderRoutes: &noWebhooks})
	var keys []string
	for _, e := range table.Entries {
		key := e.Method + " " + e.Path
		keys = append(keys, key)
		require.Equal(t, strings.HasPrefix(e.Path, "/billing/v1/checkout") || strings.HasPrefix(e.Path, "/billing/v1/captcha") ||
			slices.Contains([]string{"/billing/v1/products", "/billing/v1/prices", "/billing/v1/checkout-config", "/billing/v1/currencies"}, e.Path), e.Browser, key)
		require.False(t, strings.HasPrefix(e.Path, "/billing/v1/webhooks/"), "callbacks need a webhook-capable rail")
	}
	require.Contains(t, keys, "GET /billing/v1/capabilities")
	require.Contains(t, keys, "OPTIONS /billing/v1/checkout-sessions/{id}/pay")
	require.Contains(t, keys, "GET /billing/v1/merchant/payments")
	require.NotContains(t, keys, "OPTIONS /billing/v1/merchant/payments")
}

// Native customer identity is the host's explicit canonical customer UUID for
// a user session; other kinds, classes and opaque subjects never become payers.
func TestNativeCustomerIdentity(t *testing.T) {
	target := billingauth.Target{MerchantID: billing.MerchantID(uuid.New()), MerchantSlug: "store"}
	customer := uuid.NewString()
	session := billingauth.CredentialClassUserSession
	for _, tc := range []struct {
		name string
		id   billingauth.Identity
		ok   bool
	}{
		{"canonical customer", billingauth.Identity{Kind: billingauth.User, SubjectID: "opaque", Issuer: "issuer-a", CustomerID: billing.CustomerID(uuid.MustParse(customer)), CredentialClass: session}, true},
		{"no customer mapping", billingauth.Identity{Kind: billingauth.User, SubjectID: "opaque", Issuer: "issuer-a", CredentialClass: session}, false},
		{"no issuer", billingauth.Identity{Kind: billingauth.User, SubjectID: "opaque", CustomerID: billing.CustomerID(uuid.MustParse(customer)), CredentialClass: session}, false},
		{"invoker scoped", billingauth.Identity{Kind: billingauth.User, SubjectID: "opaque", Issuer: "issuer-a", CustomerID: billing.CustomerID(uuid.MustParse(customer)), CredentialClass: session, Invoker: "x"}, false},
		{"machine", billingauth.Identity{Kind: billingauth.Machine, SubjectID: "opaque", Issuer: "issuer-a", CustomerID: billing.CustomerID(uuid.MustParse(customer)), CredentialClass: session}, false},
		{"delegated", billingauth.Identity{Kind: billingauth.Delegated, SubjectID: "opaque", Issuer: "issuer-a", CustomerID: billing.CustomerID(uuid.MustParse(customer)), CredentialClass: session}, false},
		{"unknown kind", billingauth.Identity{Kind: "unknown", SubjectID: "opaque", Issuer: "issuer-a", CustomerID: billing.CustomerID(uuid.MustParse(customer)), CredentialClass: session}, false},
	} {
		calls := 0
		auth := identityAuth(tc.id, &calls)
		r := requestauth.Begin(httptest.NewRequest(http.MethodGet, "/v1/me/invoices", nil))
		p, err := nativeCustomer(auth, target).AuthenticateDelegated(r.Context(), r)
		_, userErr := integrationAuthenticator{auth: auth}.Authenticate(r.Context(), r)
		if !tc.ok {
			require.ErrorIs(t, err, billingauth.ErrUnauthenticated, tc.name)
			require.ErrorIs(t, userErr, billingauth.ErrUnauthenticated, tc.name)
			continue
		}
		require.NoError(t, err)
		require.NoError(t, userErr)
		require.Equal(t, 1, calls, "checkout and customer gates share one verified request")
		require.Equal(t, customer, p.SubjectID)
		require.Equal(t, "issuer-a", p.Issuer)
		require.Equal(t, target.MerchantID, p.MerchantID)
	}

	auth := identityAuth(billingauth.Identity{Kind: billingauth.User, SubjectID: "s", Issuer: "i", CustomerID: billing.CustomerID(uuid.MustParse(customer)), CredentialClass: session}, nil)
	r := requestauth.Begin(httptest.NewRequest(http.MethodGet, "/v1/me/invoices/x", nil))
	r = r.WithContext(merchanttarget.WithResolved(r.Context(), billingauth.Target{MerchantID: billing.MerchantID(uuid.New()), MerchantSlug: "store"}))
	_, err := nativeCustomer(auth, target).AuthenticateDelegated(r.Context(), r)
	requireGate(t, err, http.StatusConflict)
	r = requestauth.Begin(httptest.NewRequest(http.MethodGet, "/v1/me/invoices", nil))
	r.Header.Set(merchant.SelectorHeader, "elsewhere")
	_, err = nativeCustomer(auth, target).AuthenticateDelegated(r.Context(), r)
	requireGate(t, err, http.StatusConflict)
	r = requestauth.Begin(httptest.NewRequest(http.MethodGet, "/v1/me/invoices", nil))
	r.Header[http.CanonicalHeaderKey(merchant.SelectorHeader)] = []string{"store", "store"}
	_, err = nativeCustomer(auth, target).AuthenticateDelegated(r.Context(), r)
	requireGate(t, err, http.StatusBadRequest)
}

func requireGate(t *testing.T, err error, status int) {
	t.Helper()
	var gate billingauth.GateError
	require.ErrorAs(t, err, &gate)
	require.Equal(t, status, gate.Status, gate.Message)
}

func TestIntegrationGate(t *testing.T) {
	target := billingauth.Target{MerchantID: billing.MerchantID(uuid.New()), MerchantSlug: "store"}
	resolved := func(path string) *http.Request {
		r := requestauth.Begin(httptest.NewRequest(http.MethodGet, path, nil))
		return r.WithContext(merchanttarget.WithResolved(r.Context(), target))
	}
	staff := billingauth.Identity{Kind: billingauth.User, SubjectID: "staff", Issuer: "i", CredentialClass: billingauth.CredentialClassUserSession}
	allow := billingauth.AuthorizationFunc(func(_ context.Context, _ *http.Request, _ billingauth.Identity, q billingauth.Requirement) error {
		require.Equal(t, target, q.Target)
		return nil
	})
	authorize := func(id billingauth.Identity, authz billingauth.Authorization, r *http.Request, perm string) (billingauth.Principal, error) {
		auth := identityAuth(id, nil)
		auth.Authorization = authz
		return integrationGate{auth: auth, runtime: &app.Runtime{}}.Authorize(r.Context(), r, perm)
	}

	p, err := authorize(staff, allow, resolved("/v1/merchant/products"), billing.MerchantCatalogRead)
	require.NoError(t, err)
	require.Equal(t, billingauth.Principal{MerchantID: target.MerchantID, Kind: billingauth.User, Subject: "staff", UserContext: billingauth.UserContext{Merchant: "store"}}, p)

	machine := billingauth.Identity{Kind: billingauth.Machine, Issuer: "i", Permissions: []string{billing.MerchantCatalogRead}}
	p, err = authorize(machine, allow, resolved("/v1/merchant/products"), billing.MerchantCatalogRead)
	require.NoError(t, err)
	require.Equal(t, machine.Permissions, p.Permissions)
	require.Empty(t, p.UserContext.UserID)

	for _, tc := range []struct {
		authz  error
		status int
	}{
		{billingauth.GateError{Status: 403, Message: "denied"}, 403},
		{billingauth.GateError{Status: 503, Message: "unavailable"}, 503},
		{context.DeadlineExceeded, 503},
	} {
		_, err = authorize(staff, billingauth.AuthorizationFunc(func(context.Context, *http.Request, billingauth.Identity, billingauth.Requirement) error {
			return tc.authz
		}), resolved("/v1/merchant/products"), billing.MerchantCatalogRead)
		requireGate(t, err, tc.status)
	}
	_, err = authorize(staff, nil, resolved("/v1/merchant/products"), billing.MerchantCatalogRead)
	requireGate(t, err, http.StatusServiceUnavailable)
	failing := &billingauth.Integration{Authentication: billingauth.AuthenticationFunc(func(context.Context, *http.Request) (billingauth.Identity, error) {
		return billingauth.Identity{}, errors.New("bad token")
	}), Authorization: allow}
	r := resolved("/v1/merchant/products")
	_, err = integrationGate{auth: failing, runtime: &app.Runtime{}}.Authorize(r.Context(), r, billing.MerchantCatalogRead)
	requireGate(t, err, http.StatusUnauthorized)

	// The in-process host principal is bounded by its grants and its merchant.
	for _, tc := range []struct {
		host   requestauth.HostPrincipal
		status int
	}{
		{requestauth.HostPrincipal{MerchantID: target.MerchantID, Subject: "host", Permissions: []string{"merchant:*"}}, 0},
		{requestauth.HostPrincipal{MerchantID: target.MerchantID, Permissions: []string{billing.MerchantSettingsRead}}, 403},
		{requestauth.HostPrincipal{Permissions: []string{"merchant:*"}}, 403},
		{requestauth.HostPrincipal{MerchantID: billing.MerchantID(uuid.New()), Permissions: []string{"merchant:*"}}, 409},
	} {
		host := tc.host
		r := resolved("/v1/merchant/products")
		p, err := integrationGate{}.Authorize(requestauth.WithHostPrincipal(r.Context(), &host), r, billing.MerchantCatalogRead)
		if tc.status == 0 {
			require.NoError(t, err)
			require.Equal(t, target.MerchantID, p.MerchantID)
			require.Equal(t, "host", p.Subject)
		} else {
			requireGate(t, err, tc.status)
		}
	}
}

// Provider credential writes need a DB secret backend that can write; an
// explicit route override can neither grant them nor invent a Solana signer.
func TestProviderRoutesForRuntime(t *testing.T) {
	for _, source := range []string{config.SecretBackendSnapshot, config.SecretBackendDB} {
		for _, writable := range []bool{false, true} {
			for _, explicit := range []bool{false, true} {
				rt := &app.Runtime{Config: &config.Config{SecretBackend: source}, RouteCapabilities: &routesurface.RuntimeCapabilities{SecretWrite: writable}}
				var override *routesurface.ProviderRoutes
				if explicit {
					all := routesurface.AllProviderRoutes()
					override = &all
				}
				routes := ProviderRoutesForRuntime(rt, override)
				require.Equal(t, source == config.SecretBackendDB && writable, routes.SecretWrite, "%s/%v/%v", source, writable, explicit)
				require.False(t, routes.SolanaSigning)
				require.True(t, routes.Webhooks)
				require.Equal(t, writable, rt.RouteCapabilities.SecretWrite, "route gating never mutates runtime capabilities")
			}
		}
	}
}

// Native adapters mirror gin's tree: catch-all subtrees own their descendants
// and one wildcard name per position.
func TestValidateRouteTable(t *testing.T) {
	h := http.NotFoundHandler()
	entry := func(method, path string) router.Entry { return router.Entry{Method: method, Path: path, Handler: h} }
	console, customer := entry("GET", "/admin/{asset...}"), entry("GET", "/admin/custom/balance")
	for _, entries := range [][]router.Entry{
		{console, customer}, {customer, console},
		{entry("GET", "/a/{id}/x"), entry("GET", "/a/{name}/y")},
		{entry("GET", "/dup"), entry("GET", "/dup")},
	} {
		require.Error(t, ValidateRouteTable(&router.Table{Entries: entries}), "%v", entries)
	}
	require.NoError(t, ValidateRouteTable(&router.Table{Entries: []router.Entry{
		console, entry("GET", "/admin"), entry("POST", "/admin/custom/subscriptions/{id}/cancel"),
		entry("GET", "/a/{id}/x"), entry("POST", "/a/{name}/y"),
	}}))
}
