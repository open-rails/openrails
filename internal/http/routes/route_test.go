package routes

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/internal/config"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routesurface"
)

var pathShape = regexp.MustCompile(`^/$|^(/([a-z0-9][a-z0-9.:-]*|\{[a-z_]+\}))+$`)

// scimPathShape is SCIM's: RFC 7644 names its endpoints in CamelCase.
var scimPathShape = regexp.MustCompile(`^/v1/app/scim/v2(/([A-Z][A-Za-z]*|\{[a-z_]+\}))+$`)

// groupPaths is where each group's routes live.
var groupPaths = map[Group][]string{
	Admin:          {"/v1/admin/"},
	CatalogAdmin:   {"/v1/catalog/"},
	Access:         {"/v1/admin/access/"},
	MerchantConfig: {"/v1/merchant/"},
	Metrics:        {"/v1/metrics/"},
	App:            {"/v1/app/"},
	Customer:       {"/v1/me/"},
	Network:        {"/v1/network/"},
	Webhooks:       {"/v1/webhooks/"},
}

// pathParams are the names a path parameter takes: a resource's own id is
// {id}, its customer {customer_id}; the rest name an identity the caller
// chose, by what it is.
var pathParams = []string{"id", "customer_id", "product_key", "key", "meter_key", "request_id", "operation_id", "reference", "entitlement", "user_id", "rail", "account_id", "currency", "price_id", "link_id"}

var pathParam = regexp.MustCompile(`\{([a-z_]+)\}`)

// documents are the request bodies not named ...Params: what the route
// stores or runs, sent whole, and the Solana Pay transaction request.
var documents = []string{"Application", "DeclaredBilling", "MetricsQuery", "SolanaPayPostRequest"}

// Every catalog entry is a complete declaration: a tier with the permission
// it checks, at least one success, registered error codes.
func TestCatalogDeclarations(t *testing.T) {
	for _, r := range Catalog() {
		key := r.Key()
		require.Contains(t, []string{GET, POST, PUT, PATCH, DELETE}, r.Method, key)
		if r.Auth == AuthProvisioning {
			require.Regexp(t, scimPathShape, r.Path, key)
		} else {
			require.Regexp(t, pathShape, r.Path, key)
		}
		require.NotEmpty(t, r.Group, key)
		require.NotEmpty(t, r.Auth, key)
		if prefixes, ok := groupPaths[r.Group]; ok {
			require.True(t, slices.ContainsFunc(prefixes, func(prefix string) bool { return strings.HasPrefix(r.Path+"/", prefix) }), "%s: a %s route is under %v", key, r.Group, prefixes)
		}
		for _, param := range pathParam.FindAllStringSubmatch(r.Path, -1) {
			require.Contains(t, pathParams, param[1], "%s: name the path parameter {id}, or add what it is to pathParams", key)
		}
		require.Equal(t, r.Staff(), r.Auth == AuthMerchant, "%s: a staff route, and only one, is behind its group's permission", key)
		if r.Limit != "" {
			require.Equal(t, AuthMerchant, r.Auth, "%s: the operation limiter keys the authorized principal", key)
		}
		if r.Method == GET {
			require.Nil(t, r.Request, "%s: a GET has no body", key)
		}
		if body := reflect.TypeOf(r.Request); body != nil && body.Kind() == reflect.Struct && body.PkgPath() != reflect.TypeOf(Stream{}).PkgPath() {
			require.True(t, strings.HasSuffix(body.Name(), "Params") || slices.Contains(documents, body.Name()), "%s: name the request %s ...Params", key, body.Name())
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
			require.Contains(t, []string{"string", "integer", "boolean", "date-time", "ids", "strings"}, p.Kind, key)
		}
		got, ok := Lookup(r.Method, r.Path)
		require.True(t, ok, key)
		require.Equal(t, key, got.Key())
	}
}

// Every staff list of records with stable ids reads named ones by its ids
// filter: the one way to fetch several known records.
func TestMerchantListsTakeIDs(t *testing.T) {
	typedID := reflect.TypeFor[interface{ UUID() uuid.UUID }]()
	lists := 0
	for _, r := range Catalog() {
		if !r.Staff() || r.Method != GET || len(r.Responses) == 0 || r.Responses[0].Body == nil {
			continue
		}
		page := reflect.TypeOf(r.Responses[0].Body)
		if !strings.HasPrefix(page.Name(), "ListPage[") {
			continue
		}
		items, _ := page.FieldByName("Items")
		if items.Type.Elem().Kind() != reflect.Struct {
			continue
		}
		id, ok := items.Type.Elem().FieldByName("ID")
		if !ok || !id.Type.Implements(typedID) {
			continue
		}
		lists++
		require.Contains(t, r.Query, idsParam, "%s lists records with ids: declare idsParam", r.Key())
	}
	require.NotZero(t, lists)
}

type recorder struct {
	router.Router
	base string
	seen map[string]int
}

func (rec recorder) Handle(method, path string, _ router.Handler, _ ...router.Middleware) {
	rec.seen[method+" "+rec.base+path]++
}

func (rec recorder) Group(string, ...router.Middleware) router.Router {
	panic("the catalog mounts no groups")
}

// Mounting every group mounts the catalog: each route exactly once on the
// surface that owns it, and nothing the catalog does not declare. Only the
// routes whose configuration this runtime lacks stay out.
func TestRegistrationsMountTheWholeCatalog(t *testing.T) {
	rt := &app.Runtime{Config: &config.Config{}}
	seen := map[string]int{}
	at := func(base string) router.Router { return recorder{base: base, seen: seen} }
	raw := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	providers := routesurface.AllProviderRoutes()
	opts := Options{Auth: authtest.Deny{}, ProviderRoutes: &providers, Groups: everyGroup, Capabilities: &billing.Capabilities{}, External: External{
		Live: raw, Ready: raw,
	}}
	customers := CustomerMount{Auth: authtest.Deny{}, Providers: providers}

	RegisterMetaRoutes(at(""), opts)
	RegisterUserRoutes(at("/v1"), rt, opts)
	RegisterStaffRoutes(at("/v1"), rt, opts)
	RegisterWebhookRoutes(at("/v1/webhooks"), rt)
	RegisterAppRoutes(at("/v1"), rt, opts)
	RegisterCustomerRoutes(at("/v1/me"), rt, customers)

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
	// A bare runtime has no merchant directory, no LLM, no customer network
	// and merchant configuration in a file.
	require.Equal(t, []string{
		"catalog_copilot POST /v1/catalog/ask",
		"customer_network DELETE /v1/network/links/{link_id}",
		"customer_network DELETE /v1/network/links/{link_id}/payment-methods/{id}",
		"customer_network GET /v1/app/customers",
		"customer_network GET /v1/network/links",
		"customer_network GET /v1/network/links/{link_id}/agreements",
		"customer_network GET /v1/network/links/{link_id}/orders/{id}",
		"customer_network GET /v1/network/links/{link_id}/payment-methods",
		"customer_network GET /v1/network/links/{link_id}/subscriptions/{id}",
		"customer_network GET /v1/network/orders",
		"customer_network GET /v1/network/subscriptions",
		"customer_network POST /v1/me/network-link",
		"customer_network POST /v1/me/network-link/challenges",
		"customer_network POST /v1/network/links/{link_id}/agreements/{id}/revoke",
		"customer_network POST /v1/network/links/{link_id}/orders/{id}/confirm",
		"customer_network POST /v1/network/links/{link_id}/orders/{id}/pay",
		"customer_network POST /v1/network/links/{link_id}/payment-methods",
		"customer_network POST /v1/network/links/{link_id}/subscriptions/{id}/cancel",
		"customer_network POST /v1/network/links/{link_id}/subscriptions/{id}/resume",
		"customer_network PUT /v1/network/links/{link_id}/subscriptions/{id}/payment-method",
		"merchant_config_edits DELETE /v1/merchant/alert-webhooks/{id}",
		"merchant_config_edits PATCH /v1/merchant/alert-webhooks/{id}",
		"merchant_config_edits PATCH /v1/merchant/configuration",
		"merchant_config_edits PATCH /v1/merchant/psps/{id}",
		"merchant_config_edits POST /v1/merchant/alert-webhooks",
		"merchant_config_edits POST /v1/merchant/psps",
		"metrics_ask POST /v1/metrics/ask",
	}, sorted(unmounted))

	// The archive routes are also a surface of their own (the database-only
	// CLI runtime).
	archive := map[string]int{}
	RegisterStaffRoutesUnder(recorder{base: "/v1", seen: archive}, rt, opts, "/v1/merchant/billing-archive")
	require.Equal(t, map[string]int{"GET /v1/merchant/billing-archive": 1, "POST /v1/merchant/billing-archive": 1}, archive)
}

func sorted(list []string) []string {
	out := slices.Clone(list)
	sort.Strings(out)
	return out
}

// A route without its handler or its feature is not mounted, and a staff
// route only with its group.
func TestMountHonorsConfiguration(t *testing.T) {
	seen := map[string]int{}
	RegisterMetaRoutes(recorder{seen: seen}, Options{Capabilities: &billing.Capabilities{}})
	require.Equal(t, map[string]int{"GET /v1/config": 1}, seen, "an embedded host supplies no health routes")

	seen = map[string]int{}
	none := routesurface.ProviderRoutes{}
	RegisterUserRoutes(recorder{base: "/v1", seen: seen}, nil, Options{ProviderRoutes: &none})
	require.NotContains(t, seen, "GET /v1/checkout-attempts/{id}/solana-pay")
	require.Contains(t, seen, "GET /v1/products")

	staff, edits, configuration := map[string]int{}, map[string]int{}, map[string]int{}
	mount := func(seen map[string]int, groups config.RouteGroups) {
		RegisterStaffRoutes(recorder{base: "/v1", seen: seen}, &app.Runtime{Config: &config.Config{}}, Options{Auth: authtest.Deny{}, Groups: groups})
	}
	metrics := map[string]int{}
	mount(staff, config.RouteGroups{Admin: true})
	mount(edits, config.RouteGroups{Catalog: true})
	mount(configuration, config.RouteGroups{MerchantConfig: true})
	mount(metrics, config.RouteGroups{Metrics: true})
	require.Contains(t, staff, "GET /v1/admin/payments")
	require.Contains(t, staff, "POST /v1/admin/subscriptions/{id}/change/preview")
	require.Contains(t, staff, "POST /v1/admin/payments/{id}/refunds", "the admin group mounts its changes with its reads")
	require.NotContains(t, staff, "GET /v1/catalog/products", "the catalog is its own group")
	require.NotContains(t, staff, "POST /v1/catalog/price-migrations")
	require.NotContains(t, staff, "POST /v1/metrics/query")
	require.Contains(t, staff, "GET /v1/admin/entitlements", "a lookup is a read")
	require.Contains(t, edits, "POST /v1/catalog/products")
	require.Contains(t, edits, "GET /v1/catalog/products", "catalog reads are Catalog's")
	require.Contains(t, edits, "POST /v1/catalog/price-migrations")
	require.NotContains(t, edits, "GET /v1/admin/payments")
	require.NotContains(t, configuration, "GET /v1/catalog/products")
	require.Contains(t, configuration, "GET /v1/merchant/psps", "a configuration read is MerchantConfig's")
	require.Contains(t, configuration, "PUT /v1/merchant/dashboard", "the layout is configuration")
	require.NotContains(t, configuration, "GET /v1/metrics/dashboard")
	require.Contains(t, metrics, "GET /v1/metrics/dashboard")
	require.Contains(t, metrics, "POST /v1/metrics/query")
	require.NotContains(t, metrics, "GET /v1/admin/payments")
	for _, seen := range []map[string]int{staff, edits, configuration, metrics} {
		require.Contains(t, seen, "GET /v1/admin/access", "every staff group brings the access read")
	}
}

// A declared integer query parameter that is not a non-negative integer is
// refused before the handler reads it; an absent one is the handler's default.
func TestDeclaredIntegerQueriesAreStrict(t *testing.T) {
	route, ok := Lookup(GET, "/v1/admin/payment-attempts")
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

// A list's ids filter names 1 to MaxBatchItems records, as one comma list,
// and nothing else beside it.
func TestIDsFilterIsBoundedAndAlone(t *testing.T) {
	ids := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = "pay_" + uuid.NewString()
		}
		return strings.Join(parts, ",")
	}
	for query, want := range map[string]int{
		"":                                    http.StatusOK,
		"status=open&limit=5":                 http.StatusOK,
		"ids=" + ids(1):                       http.StatusOK,
		"ids=" + ids(billing.MaxBatchItems):   http.StatusOK,
		"ids=" + ids(billing.MaxBatchItems+1): http.StatusBadRequest,
		"ids=":                                http.StatusBadRequest,
		"ids=" + ids(1) + "&ids=" + ids(1):    http.StatusBadRequest,
		"ids=" + ids(2) + "&status=open":      http.StatusBadRequest,
		"ids=" + ids(2) + "&limit=1":          http.StatusBadRequest,
	} {
		rec := httptest.NewRecorder()
		called := false
		idsMW(func(r *httprequest.Request) { called = true; r.Status(http.StatusOK) })(httprequest.NewHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/payments?"+query, nil), nil))
		require.Equal(t, want, rec.Code, query)
		require.Equal(t, want == http.StatusOK, called, query)
		if want != http.StatusOK {
			require.Contains(t, rec.Body.String(), `"invalid_query"`, query)
			require.Contains(t, rec.Body.String(), `"ids"`, query)
		}
	}
}

// billing.Permissions is the route catalog's permissions and no more: every
// staff and programmatic route names a pair of them, and each guards a route.
func TestExportedPermissionsAreTheCatalogs(t *testing.T) {
	used := map[billing.Permission]bool{}
	for _, r := range Catalog() {
		pair := r.Needs()
		require.Equal(t, r.Staff() || r.Auth == AuthApplication, pair != (Pair{}), r.Key())
		require.NoError(t, pair.check(), r.Key())
		for _, name := range pair.Names() {
			used[name] = true
		}
	}
	for _, perm := range billing.Permissions() {
		require.True(t, used[perm], "%s guards no route", perm)
		delete(used, perm)
	}
	require.Empty(t, used, "a route's permission is not in billing.Permissions")

	// A route about no one merchant names its root: permission alone; any
	// other pair is one permission in both personas.
	require.NoError(t, Pair{Root: billing.RootCustomerRead}.check())
	for _, bad := range []Pair{
		{Merchant: billing.MerchantCustomerRead},
		{Merchant: billing.MerchantCustomerRead, Root: billing.RootCustomerManage},
		{Merchant: billing.RootCustomerRead, Root: billing.RootCustomerRead},
		{Merchant: billing.MerchantCustomerRead, Root: billing.RootMerchantsRead},
	} {
		require.Error(t, bad.check(), "%v", bad)
	}
}

// Staff groups have separate paths. Registering every group does not bring
// back the former admin or operator aliases for management and metrics.
func TestManagementRoutesHaveNoLegacyAliases(t *testing.T) {
	rt := gatedRuntime(t)
	table := merchantSurface(rt, Options{Auth: authtest.Deny{}, Groups: everyGroup, live: live})
	keys := routeKeys(table)
	var management int
	for _, route := range Catalog() {
		var previous []string
		switch route.Group {
		case CatalogAdmin:
			require.True(t, strings.HasPrefix(route.Path, "/v1/catalog/"), route.Key())
			suffix := strings.TrimPrefix(route.Path, "/v1/catalog/")
			if !strings.HasPrefix(suffix, "price-migrations") {
				suffix = "catalog/" + suffix
			}
			previous = []string{"/v1/admin/" + suffix, "/v1/operator/" + suffix}
		case MerchantConfig:
			require.True(t, strings.HasPrefix(route.Path, "/v1/merchant/"), route.Key())
			suffix := strings.TrimPrefix(route.Path, "/v1/merchant/")
			previous = []string{"/v1/admin/" + suffix, "/v1/operator/" + suffix}
		case Metrics:
			require.True(t, strings.HasPrefix(route.Path, "/v1/metrics/"), route.Key())
			suffix := strings.TrimPrefix(route.Path, "/v1/metrics/")
			if !strings.HasPrefix(suffix, "dashboard") {
				suffix = "metrics/" + suffix
			}
			previous = []string{"/v1/admin/" + suffix}
		default:
			continue
		}
		management++
		for _, old := range previous {
			_, found := Lookup(route.Method, old)
			require.False(t, found, "%s %s must not remain as an alias", route.Method, old)
			require.NotContains(t, keys, route.Method+" "+old)
		}
	}
	require.Positive(t, management)
}

func TestProvisioningConsumerAndCredentialsShareCapability(t *testing.T) {
	for _, hostDirectory := range []bool{false, true} {
		for _, programmatic := range []bool{false, true} {
			rt := &app.Runtime{Config: &config.Config{}, HostUserInfo: hostDirectory}
			seen := map[string]int{}
			opts := Options{Auth: authtest.Deny{}, Groups: config.RouteGroups{MerchantConfig: true, Programmatic: programmatic}}
			RegisterStaffRoutes(recorder{base: "/v1", seen: seen}, rt, opts)
			RegisterAppRoutes(recorder{base: "/v1", seen: seen}, rt, opts)
			for _, key := range []string{"POST /v1/app/scim/v2/Users", "GET /v1/merchant/provisioning-tokens", "POST /v1/merchant/provisioning-tokens", "DELETE /v1/merchant/provisioning-tokens/{id}"} {
				require.Equal(t, !hostDirectory && programmatic, seen[key] == 1, key)
			}
		}
	}
}
