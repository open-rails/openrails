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

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
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
	h := CapabilitiesHandler([]RouteSet{RouteSetCheckout, RouteSetCustomer, RouteSetWebhooks}, routesurface.ProviderRoutes{Solana: true})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/billing/v1/capabilities", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "public, max-age=300", rec.Header().Get("Cache-Control"))
	var caps Capabilities
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &caps))
	require.Len(t, caps.RouteGroups, len(AllRouteSets), "every known group is reported")
	for _, rs := range AllRouteSets {
		require.Equal(t, rs == RouteSetCheckout || rs == RouteSetCustomer || rs == RouteSetWebhooks, caps.RouteGroups[rs], rs)
	}
	require.Equal(t, map[string]bool{
		"solana_one_time_payments": true, "stripe_billing_portal": false,
		"solana_subscription_management": false, "provider_credential_writes": false,
	}, caps.Features)

	req := httptest.NewRequest(http.MethodGet, "/billing/v1/capabilities", nil)
	req.Header.Set("If-None-Match", rec.Header().Get("ETag"))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotModified, rec.Code)

	// Customer features follow the mounted customer scope, not provider support.
	for _, tc := range []struct {
		scope          CustomerHTTPScope
		portal, solana bool
	}{
		{CustomerSelfService, true, true},
		{CustomerBillingManagement, false, true},
		{CustomerSubscriptionManagement, false, false},
	} {
		caps := configuredCapabilities(HTTPConfig{CustomerRoutes: []CustomerRoutesConfig{{Scope: tc.scope}}}, routesurface.AllProviderRoutes())
		require.True(t, caps.RouteGroups[RouteSetCustomer])
		require.Equal(t, tc.portal, caps.Features["stripe_billing_portal"], tc.scope)
		require.Equal(t, tc.solana, caps.Features["solana_subscription_management"], tc.scope)
		require.False(t, caps.Features["provider_credential_writes"], "credential writes need merchant_config")
	}

	require.Equal(t, EmbeddedDefaultRouteSets, ResolveRouteSets(nil))
	require.Equal(t, []RouteSet{RouteSetCheckout, RouteSetWebhooks}, ResolveRouteSets([]RouteSet{RouteSetCheckout, "", RouteSetCheckout, RouteSetWebhooks}))
}

// HTTP publication is refused unless each surface has the authority it needs.
func TestHTTPConfigValidation(t *testing.T) {
	authn := identityAuth(billingauth.Identity{}, nil)
	full := &billingauth.Integration{Authentication: authn.Authentication, Authorization: billingauth.AuthorizationFunc(func(context.Context, *http.Request, billingauth.Identity, billingauth.Requirement) error { return nil })}
	host := billingauth.DelegatedAuthenticatorFunc(func(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) { return nil, nil })
	customer := func(c CustomerRoutesConfig) *HTTPConfig {
		return &HTTPConfig{CustomerRoutes: []CustomerRoutesConfig{c}}
	}
	for _, tc := range []struct {
		name string
		cfg  *HTTPConfig
		auth *billingauth.Integration
		ok   bool
	}{
		{"no HTTP", nil, nil, true},
		{"checkout without authentication", &HTTPConfig{Checkout: true}, nil, false},
		{"checkout", &HTTPConfig{Checkout: true}, authn, true},
		{"management without authorization", &HTTPConfig{MerchantAdmin: true}, authn, false},
		{"merchant API without authorization", &HTTPConfig{MerchantAPI: true}, authn, false},
		{"management", &HTTPConfig{MerchantAdmin: true, Catalog: true, MerchantConfig: true, MerchantAPI: true}, full, true},
		{"customer without any authenticator", customer(CustomerRoutesConfig{Merchant: "store"}), nil, false},
		{"native customer without merchant", customer(CustomerRoutesConfig{}), authn, false},
		{"native customer", customer(CustomerRoutesConfig{Merchant: "store"}), authn, true},
		{"treasury off the canonical mount", customer(CustomerRoutesConfig{Prefix: "/v1/tenant/me", Treasury: true, DelegatedAuthenticator: host}), nil, false},
		{"unknown scope", customer(CustomerRoutesConfig{Scope: 9, DelegatedAuthenticator: host}), nil, false},
		{"parameterized prefix", customer(CustomerRoutesConfig{Prefix: "/v1/tenants/{tenant}/me", DelegatedAuthenticator: host}), nil, true},
	} {
		require.Equal(t, tc.ok, ValidateHTTPConfig(tc.cfg, tc.auth) == nil, tc.name)
	}
	for _, prefix := range []string{"/", "me", "/a/../b", "/a/", "/a/*", "/a b", "/a/{x.y}", "/a/b{c}", "/a/{}"} {
		require.Error(t, ValidateHTTPConfig(customer(CustomerRoutesConfig{Prefix: prefix, DelegatedAuthenticator: host}), nil), prefix)
	}
}

// The combined handler refuses to mount a surface without its auth boundary,
// and only checkout routes join the permissive-CORS browser tier.
func TestNewRoutes(t *testing.T) {
	require.Panics(t, func() { (&Assembler{}).NewRoutes(Options{RouteSets: []RouteSet{RouteSetCheckout}}) })
	require.Panics(t, func() {
		(&Assembler{Authenticator: billingauth.AuthenticatorFunc(nil)}).NewRoutes(Options{RouteSets: []RouteSet{RouteSetMerchantAdmin}})
	})

	gate := billingauth.Gate(IntegrationGate(&app.Runtime{}))
	asm := &Assembler{Runtime: &app.Runtime{Config: &config.Config{}}, Authenticator: billingauth.AuthenticatorFunc(func(context.Context, *http.Request) (billingauth.UserContext, error) {
		return billingauth.UserContext{}, billingauth.ErrUnauthenticated
	}), Gate: gate}
	noWebhooks := routesurface.ProviderRoutes{}
	table := asm.NewRoutes(Options{RouteSets: []RouteSet{RouteSetCheckout, RouteSetMerchantAdmin, RouteSetWebhooks}, ProviderRoutes: &noWebhooks})
	var keys []string
	for _, e := range table.Entries {
		key := e.Method + " " + e.Path
		keys = append(keys, key)
		require.Equal(t, strings.HasPrefix(e.Path, "/billing/v1/checkout") || strings.HasPrefix(e.Path, "/billing/v1/captcha") ||
			slices.Contains([]string{"/billing/v1/products", "/billing/v1/prices", "/billing/v1/checkout-config", "/billing/v1/currencies"}, e.Path), e.Browser, key)
		require.NotContains(t, e.Path, "/webhooks/", "callbacks need a webhook-capable rail")
	}
	require.Contains(t, keys, "GET /billing/v1/capabilities")
	require.Contains(t, keys, "OPTIONS /billing/v1/checkout")
	require.Contains(t, keys, "GET /billing/v1/merchant/payments")
	require.NotContains(t, keys, "OPTIONS /billing/v1/merchant/payments")
}

// Native customer identity is the host's explicit canonical customer UUID for
// a user session; other kinds, classes and opaque subjects never become payers.
func TestNativeCustomerIdentity(t *testing.T) {
	target := billingauth.Target{MerchantID: merchant.ID(uuid.New()), MerchantSlug: "store"}
	customer := uuid.NewString()
	session := billingauth.CredentialClassUserSession
	for _, tc := range []struct {
		name string
		id   billingauth.Identity
		ok   bool
	}{
		{"canonical customer", billingauth.Identity{Kind: billingauth.NativeUser, SubjectID: "opaque", Issuer: "issuer-a", CustomerID: customer, CredentialClass: session}, true},
		{"no customer mapping", billingauth.Identity{Kind: billingauth.NativeUser, SubjectID: "opaque", Issuer: "issuer-a", CredentialClass: session}, false},
		{"opaque customer", billingauth.Identity{Kind: billingauth.NativeUser, SubjectID: "opaque", Issuer: "issuer-a", CustomerID: "user-1", CredentialClass: session}, false},
		{"non-canonical uuid", billingauth.Identity{Kind: billingauth.NativeUser, SubjectID: "opaque", Issuer: "issuer-a", CustomerID: strings.ToUpper(customer), CredentialClass: session}, false},
		{"no issuer", billingauth.Identity{Kind: billingauth.NativeUser, SubjectID: "opaque", CustomerID: customer, CredentialClass: session}, false},
		{"invoker scoped", billingauth.Identity{Kind: billingauth.NativeUser, SubjectID: "opaque", Issuer: "issuer-a", CustomerID: customer, CredentialClass: session, Invoker: "x"}, false},
		{"machine", billingauth.Identity{Kind: billingauth.Machine, SubjectID: "opaque", Issuer: "issuer-a", CustomerID: customer, CredentialClass: session}, false},
		{"delegated", billingauth.Identity{Kind: billingauth.DelegatedUser, SubjectID: "opaque", Issuer: "issuer-a", CustomerID: customer, CredentialClass: session}, false},
		{"unknown kind", billingauth.Identity{Kind: "unknown", SubjectID: "opaque", Issuer: "issuer-a", CustomerID: customer, CredentialClass: session}, false},
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
		require.Equal(t, target.MerchantID.String(), p.MerchantID)
	}

	auth := identityAuth(billingauth.Identity{Kind: billingauth.NativeUser, SubjectID: "s", Issuer: "i", CustomerID: customer, CredentialClass: session}, nil)
	r := requestauth.Begin(httptest.NewRequest(http.MethodGet, "/v2/me/invoices/x", nil))
	r = r.WithContext(merchanttarget.WithResolved(r.Context(), billingauth.Target{MerchantID: merchant.ID(uuid.New()), MerchantSlug: "store"}))
	_, err := nativeCustomer(auth, target).AuthenticateDelegated(r.Context(), r)
	requireGate(t, err, http.StatusConflict)
	r = requestauth.Begin(httptest.NewRequest(http.MethodGet, "/v1/me/invoices", nil))
	r.Header.Set(merchant.SlugHeader, "store")
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
	target := billingauth.Target{MerchantID: merchant.ID(uuid.New()), MerchantSlug: "store"}
	resolved := func(path string) *http.Request {
		r := requestauth.Begin(httptest.NewRequest(http.MethodGet, path, nil))
		return r.WithContext(merchanttarget.WithResolved(r.Context(), target))
	}
	staff := billingauth.Identity{Kind: billingauth.NativeUser, SubjectID: "staff", Issuer: "i", CredentialClass: billingauth.CredentialClassUserSession}
	allow := billingauth.AuthorizationFunc(func(_ context.Context, _ *http.Request, _ billingauth.Identity, q billingauth.Requirement) error {
		require.Equal(t, target, q.Target)
		return nil
	})
	authorize := func(id billingauth.Identity, authz billingauth.Authorization, r *http.Request, perm string) (billingauth.Principal, error) {
		auth := identityAuth(id, nil)
		auth.Authorization = authz
		return integrationGate{auth: auth, runtime: &app.Runtime{}}.Authorize(r.Context(), r, perm)
	}

	p, err := authorize(staff, allow, resolved("/v2/merchant/products"), permissions.MerchantCatalogRead)
	require.NoError(t, err)
	require.Equal(t, billingauth.Principal{MerchantID: target.MerchantID, Subject: "staff", UserContext: billingauth.UserContext{Merchant: "store"}}, p)

	_, err = authorize(staff, allow, resolved("/v1/catalog"), permissions.MerchantCatalogOwnRead)
	requireGate(t, err, http.StatusForbidden)
	owned := staff
	owned.CustomerID = uuid.NewString()
	p, err = authorize(owned, allow, resolved("/v1/catalog"), permissions.MerchantCatalogOwnRead)
	require.NoError(t, err)
	require.Equal(t, owned.CustomerID, p.Subject, "personal catalogs key on the canonical customer, never the issuer subject")
	withOwner := resolved("/v1/catalog")
	withOwner.Header.Set("OpenRails-Catalog-Owner", "b3duZXI")
	p, err = authorize(staff, allow, withOwner, permissions.MerchantCatalogOwnRead)
	require.NoError(t, err)
	require.Empty(t, p.Subject, "an explicit owner leaves the live administrator check to the route")

	machine := billingauth.Identity{Kind: billingauth.Machine, Issuer: "i", Permissions: []string{permissions.MerchantCatalogRead}}
	p, err = authorize(machine, allow, resolved("/v2/merchant/products"), permissions.MerchantCatalogRead)
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
		}), resolved("/v2/merchant/products"), permissions.MerchantCatalogRead)
		requireGate(t, err, tc.status)
	}
	_, err = authorize(staff, nil, resolved("/v2/merchant/products"), permissions.MerchantCatalogRead)
	requireGate(t, err, http.StatusServiceUnavailable)
	failing := &billingauth.Integration{Authentication: billingauth.AuthenticationFunc(func(context.Context, *http.Request) (billingauth.Identity, error) {
		return billingauth.Identity{}, errors.New("bad token")
	}), Authorization: allow}
	r := resolved("/v2/merchant/products")
	_, err = integrationGate{auth: failing, runtime: &app.Runtime{}}.Authorize(r.Context(), r, permissions.MerchantCatalogRead)
	requireGate(t, err, http.StatusUnauthorized)

	// The in-process host principal is bounded by its grants and its merchant.
	for _, tc := range []struct {
		host   requestauth.HostPrincipal
		status int
	}{
		{requestauth.HostPrincipal{MerchantID: target.MerchantID, Subject: "host", Permissions: []string{"merchant:*"}}, 0},
		{requestauth.HostPrincipal{MerchantID: target.MerchantID, Permissions: []string{permissions.MerchantSettingsRead}}, 403},
		{requestauth.HostPrincipal{Permissions: []string{"merchant:*"}}, 403},
		{requestauth.HostPrincipal{MerchantID: merchant.ID(uuid.New()), Permissions: []string{"merchant:*"}}, 409},
	} {
		host := tc.host
		r := resolved("/v2/merchant/products")
		p, err := integrationGate{}.Authorize(requestauth.WithHostPrincipal(r.Context(), &host), r, permissions.MerchantCatalogRead)
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
