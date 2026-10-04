package routes

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routesurface"
)

// untypedBudget is how many routes still accept or answer a body no Go type
// declares. It only goes down: a lane that types a route lowers it.
const untypedBudget = 54

var pathShape = regexp.MustCompile(`^/$|^(/([a-z0-9][a-z0-9.:-]*|\{[a-z_]+\}))+$`)

// Every catalog entry is a complete declaration: a tier with the permission
// it checks, at least one success, registered error codes.
func TestCatalogDeclarations(t *testing.T) {
	require.Len(t, Catalog(), 269)
	untyped := 0
	for _, r := range Catalog() {
		key := r.Key()
		require.Contains(t, []string{GET, POST, PUT, PATCH, DELETE}, r.Method, key)
		require.Regexp(t, pathShape, r.Path, key)
		require.NotEmpty(t, r.Group, key)
		require.NotEmpty(t, r.Auth, key)
		switch r.Auth {
		case AuthMerchant:
			require.True(t, strings.HasPrefix(r.Perm, "merchant:"), "%s: a merchant route checks a merchant: permission, not %q", key, r.Perm)
		case AuthOperator:
			require.True(t, strings.HasPrefix(r.Perm, "root:"), "%s: %q", key, r.Perm)
		default:
			require.Empty(t, r.Perm, "%s: tier %s checks no permission", key, r.Auth)
		}
		if r.Also != "" {
			require.Equal(t, AuthMerchant, r.Auth, key)
		}
		if r.Limit != "" {
			require.Equal(t, AuthMerchant, r.Auth, "%s: the operation limiter keys the authorized principal", key)
		}
		if r.Scope != ScopeSelfService || r.InvokerScoped {
			require.Equal(t, Customer, r.Group, key)
		}
		if r.Method == GET {
			require.Nil(t, r.Request, "%s: a GET has no body", key)
		}
		require.NotEmpty(t, r.Responses, "%s declares no success", key)
		seen := map[int]bool{}
		for _, reply := range r.Responses {
			require.Contains(t, []int{200, 201, 202, 204}, reply.Status, key)
			if reply.Status == http.StatusNoContent {
				require.Nil(t, reply.Body, "%s: 204 answers no body", key)
			}
			seen[reply.Status] = true
		}
		require.True(t, sort.StringsAreSorted(r.Errors), "%s: Errors is sorted", key)
		for _, code := range r.AllErrors() {
			_, ok := billing.LookupErrorCode(code)
			require.True(t, ok, "%s: %s is not a registered error code", key, code)
		}
		for _, p := range r.Query {
			require.Contains(t, []string{"string", "integer", "boolean", "date-time"}, p.Kind, key)
		}
		got, ok := Lookup(r.Method, r.Path)
		require.True(t, ok, key)
		require.Equal(t, key, got.Key())
		if r.Untyped() {
			untyped++
		}
	}
	require.Equal(t, untypedBudget, untyped, "routes with an untyped body: lower untypedBudget when a route gains a type, and never raise it")
}

type recorder struct {
	router.Router
	base string
	seen map[string]int
}

func (rec recorder) Handle(method, path string, _ router.Handler, _ ...router.Middleware) {
	if path == "/{$}" {
		path = "/"
	}
	rec.seen[method+" "+rec.base+path]++
}

func (rec recorder) Group(string, ...router.Middleware) router.Router {
	panic("the catalog mounts no groups")
}

// Mounting every group mounts the catalog: each route exactly once on the
// surface that owns it, and nothing the catalog does not declare. Only the
// routes whose configuration this runtime lacks stay out.
func TestRegistrationsMountTheWholeCatalog(t *testing.T) {
	rt := &app.Runtime{Config: &config.Config{AllowCatalogUpdates: true}}
	seen := map[string]int{}
	at := func(base string) router.Router { return recorder{base: base, seen: seen} }
	raw := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	handler := router.Handler(func(*httprequest.Request) {})
	providers := routesurface.AllProviderRoutes()
	opts := Options{ProviderRoutes: &providers, External: External{
		Banner: raw, Live: raw, Ready: raw, Metrics: raw, Capabilities: raw, CaptchaStatus: raw, CaptchaScript: raw,
		ListMerchants: handler, CreateMerchant: handler, RenameMerchant: handler, CreateAPIKey: handler, ListAPIKeys: handler, RevokeAPIKey: handler,
		ListTeam: handler, ListTeamInvites: handler, InviteTeamMember: handler, RevokeTeamInvite: handler, ChangeTeamRole: handler, RemoveTeamMember: handler,
		MerchantCreationEnabled: true,
	}}
	pass := func(next router.Handler) router.Handler { return next }

	RegisterMetaRoutes(at(""), opts)
	RegisterUserRoutes(at("/v1"), rt, opts)
	RegisterMerchantRoutes(at("/v1"), rt, opts)
	RegisterControlPlaneRoutes(at("/v1"), rt, opts)
	RegisterWebhookRoutes(at("/v1/webhooks"), rt)
	RegisterSelfServiceRoutes(at("/v1/me"), rt, pass, providers)
	RegisterPlatformRoutes(at("/v1/platform"), rt, PlatformOptions{})

	var unmounted []string
	for _, r := range Catalog() {
		switch seen[r.Key()] {
		case 1:
		case 0:
			unmounted = append(unmounted, string(r.When)+" "+r.Key())
		default:
			t.Errorf("%s is mounted %d times", r.Key(), seen[r.Key()])
		}
		delete(seen, r.Key())
	}
	require.Empty(t, seen, "mounted outside the catalog")
	// A bare runtime has no merchant directory and no LLM.
	require.Equal(t, []string{
		"catalog_copilot POST /v1/merchant/catalog/ask",
		"catalog_copilot POST /v1/merchant/catalog/copilot/confirm",
		"dashboard_generation POST /v1/merchant/dashboard/widgets/generate",
		"merchant_directory GET /v1/merchant/api-host",
		"merchant_directory POST /v1/merchant/api-host/verify",
		"merchant_directory PUT /v1/merchant/api-host",
		"metrics_ask POST /v1/merchant/metrics/ask",
	}, sorted(unmounted))

	// The archive routes are also a surface of their own (the database-only
	// CLI runtime), and a narrower customer exposure serves a subset.
	archive := map[string]int{}
	RegisterMerchantRoutesUnder(recorder{base: "/v1", seen: archive}, rt, opts, "/v1/merchant/billing-archive")
	require.Equal(t, map[string]int{"GET /v1/merchant/billing-archive": 1, "POST /v1/merchant/billing-archive": 1}, archive)
	management, subscriptions := map[string]int{}, map[string]int{}
	RegisterCustomerBillingManagementRoutes(recorder{base: "/v1/me", seen: management}, rt, pass, providers)
	RegisterCustomerSubscriptionManagementRoutes(recorder{base: "/v1/me", seen: subscriptions}, rt, pass)
	require.Len(t, subscriptions, 4)
	require.Len(t, management, 35)
	for key := range subscriptions {
		require.Contains(t, management, key, "each scope includes the narrower ones")
	}
	require.NotContains(t, management, "POST /v1/me/checkout")
	require.Contains(t, management, "POST /v1/me/checkout/{id}/confirm")
}

func sorted(list []string) []string {
	out := slices.Clone(list)
	sort.Strings(out)
	return out
}

// A route without its handler or its feature is not mounted, and a catalog
// write is mounted only where the deployment allows it.
func TestMountHonorsConfiguration(t *testing.T) {
	seen := map[string]int{}
	RegisterMetaRoutes(recorder{seen: seen}, Options{External: External{Capabilities: http.NotFoundHandler()}})
	require.Equal(t, map[string]int{"GET /v1/capabilities": 1}, seen, "an embedded host supplies no health routes")

	seen = map[string]int{}
	none := routesurface.ProviderRoutes{}
	RegisterUserRoutes(recorder{base: "/v1", seen: seen}, nil, Options{ProviderRoutes: &none})
	require.NotContains(t, seen, "GET /v1/solana/config")
	require.NotContains(t, seen, "GET /v1/captcha/status")
	require.Contains(t, seen, "GET /v1/products")

	closed, open := map[string]int{}, map[string]int{}
	RegisterMerchantRoutes(recorder{base: "/v1", seen: closed}, &app.Runtime{Config: &config.Config{}}, Options{})
	RegisterMerchantRoutes(recorder{base: "/v1", seen: open}, &app.Runtime{Config: &config.Config{}}, Options{InProcess: true})
	require.NotContains(t, closed, "POST /v1/merchant/catalog/products")
	require.Contains(t, closed, "GET /v1/merchant/catalog/products")
	require.Contains(t, closed, "POST /v1/merchant/catalog/offers/lookup", "a lookup is a read")
	require.Contains(t, open, "POST /v1/merchant/catalog/products")
}

// A declared integer query parameter that is not a non-negative integer is
// refused before the handler reads it; an absent one is the handler's default.
func TestDeclaredIntegerQueriesAreStrict(t *testing.T) {
	route, ok := Lookup(GET, "/v1/merchant/payment-attempts")
	require.True(t, ok)
	require.Equal(t, []string{"limit"}, checkedParams(route.Query))

	reached := 0
	table := &router.Table{}
	router.NewMux(table, "", nil).Handle(GET, "/list", func(r *httprequest.Request) {
		reached++
		r.SuccessJSON(map[string]string{"limit": r.Query("limit")})
	}, strictQueryMW([]string{"limit", "offset"}))
	h := table.Handler()
	for query, status := range map[string]int{
		"": 200, "limit=5&offset=0": 200, "limit=": 200, "other=x": 200,
		"limit=ten": 400, "limit=-1": 400, "offset=1.5": 400, "limit=5&offset=x": 400,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(GET, "/list?"+query, nil))
		require.Equal(t, status, rec.Code, query)
		if status == 400 {
			require.Contains(t, rec.Body.String(), `"code":"invalid_query"`, query)
		}
	}
	require.Equal(t, 4, reached)
}
