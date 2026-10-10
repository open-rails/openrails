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
var scimPathShape = regexp.MustCompile(`^/scim/v2(/([A-Z][A-Za-z]*|\{[a-z_]+\}))+$`)

// groupPaths is where each group's routes live.
var groupPaths = map[Group][]string{
	Admin:          {"/v1/admin/"},
	CatalogWrite:   {"/v1/admin/catalog/", "/v1/admin/customers/"},
	MerchantConfig: {"/v1/admin/"},
	Customer:       {"/v1/me/"},
	ControlPlane:   {"/v1/merchant/", "/v1/merchants"},
	Platform:       {"/v1/platform/"},
	Provisioning:   {"/scim/v2/"},
	Webhooks:       {"/v1/webhooks/"},
}

// pathParams are the names a path parameter takes: a resource's own id is
// {id}, its customer {customer_id}; the rest name an identity the caller
// chose, by what it is.
var pathParams = []string{"id", "customer_id", "product_key", "key", "meter_key", "request_id", "operation_id", "scope", "scope_key", "entitlement", "user_id", "rail", "account_id"}

var pathParam = regexp.MustCompile(`\{([a-z_]+)\}`)

// documents are the request bodies not named ...Params: what the route
// stores or runs, sent whole, and the Solana Pay transaction request.
var documents = []string{"Application", "DeclaredBilling", "MetricsQuery", "CollectionPaymentMethod", "SolanaPayPostRequest"}

// Every catalog entry is a complete declaration: a tier with the permission
// it checks, at least one success, registered error codes.
func TestCatalogDeclarations(t *testing.T) {
	require.Len(t, Catalog(), 213)
	for _, r := range Catalog() {
		key := r.Key()
		require.Contains(t, []string{GET, POST, PUT, PATCH, DELETE}, r.Method, key)
		if r.Group == Provisioning {
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
		switch {
		case r.Staff():
			require.Empty(t, r.Perm, "%s: a staff route checks its bundle's permission", key)
		case r.Auth == AuthMerchant:
			require.True(t, strings.HasPrefix(r.Perm, "merchant:"), "%s: a control-plane route checks the server's merchant: permission, not %q", key, r.Perm)
		case r.Auth == AuthOperator:
			require.True(t, strings.HasPrefix(r.Perm, "root:"), "%s: %q", key, r.Perm)
		default:
			require.Empty(t, r.Perm, "%s: tier %s checks no permission", key, r.Auth)
		}
		if r.Limit != "" {
			require.Equal(t, AuthMerchant, r.Auth, "%s: the operation limiter keys the authorized principal", key)
		}
		if r.InvokerScoped {
			require.Equal(t, Customer, r.Group, key)
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
			require.Contains(t, []string{"string", "integer", "boolean", "date-time", "ids"}, p.Kind, key)
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
	require.Equal(t, 20, lists)
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
	handler := router.Handler(func(*httprequest.Request) {})
	providers := routesurface.AllProviderRoutes()
	opts := Options{Auth: authtest.Deny{}, ProviderRoutes: &providers, Permissions: staffPermissions, Capabilities: &billing.Capabilities{}, External: External{
		Live: raw, Ready: raw, Metrics: raw, CaptchaStatus: raw, CaptchaScript: raw,
		ListMerchants: handler, CreateMerchant: handler, RenameMerchant: handler, CreateAPIKey: handler, ListAPIKeys: handler, RevokeAPIKey: handler,
		ListTeam: handler, ListTeamInvites: handler, InviteTeamMember: handler, RevokeTeamInvite: handler, ChangeTeamRole: handler, RemoveTeamMember: handler,
		ListFederatedGrants: handler, CreateFederatedGrant: handler, RevokeFederatedGrant: handler, ListMyFederatedGrants: handler, AcceptFederatedGrant: handler,
		MerchantCreationEnabled: true,
	}}
	customers := CustomerMount{Auth: authtest.Deny{}, Providers: providers}

	RegisterMetaRoutes(at(""), opts)
	RegisterUserRoutes(at("/v1"), rt, opts)
	RegisterStaffRoutes(at("/v1"), rt, opts)
	RegisterControlPlaneRoutes(at("/v1"), rt, opts)
	RegisterWebhookRoutes(at("/v1/webhooks"), rt)
	RegisterProvisioningRoutes(at("/scim/v2"), rt, Options{Provisioning: func(*http.Request) (billing.MerchantID, error) { return billing.MerchantID{}, nil }})
	RegisterCustomerRoutes(at("/v1/me"), rt, customers)
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
		"catalog_copilot POST /v1/admin/catalog/ask",
		"dashboard_generation POST /v1/admin/dashboard/widgets/generate",
		"merchant_directory GET /v1/admin/api-host",
		"merchant_directory POST /v1/admin/api-host/verify",
		"merchant_directory PUT /v1/admin/api-host",
		"metrics_ask POST /v1/admin/metrics/ask",
	}, sorted(unmounted))

	// The archive routes are also a surface of their own (the database-only
	// CLI runtime).
	archive := map[string]int{}
	RegisterStaffRoutesUnder(recorder{base: "/v1", seen: archive}, rt, opts, "/v1/admin/billing-archive")
	require.Equal(t, map[string]int{"GET /v1/admin/billing-archive": 1, "POST /v1/admin/billing-archive": 1}, archive)
}

func sorted(list []string) []string {
	out := slices.Clone(list)
	sort.Strings(out)
	return out
}

// A route without its handler or its feature is not mounted, and a staff
// route only with its bundle's permission.
func TestMountHonorsConfiguration(t *testing.T) {
	seen := map[string]int{}
	RegisterMetaRoutes(recorder{seen: seen}, Options{Capabilities: &billing.Capabilities{}})
	require.Equal(t, map[string]int{"GET /v1/config": 1}, seen, "an embedded host supplies no health routes")

	seen = map[string]int{}
	none := routesurface.ProviderRoutes{}
	RegisterUserRoutes(recorder{base: "/v1", seen: seen}, nil, Options{ProviderRoutes: &none})
	require.NotContains(t, seen, "GET /v1/solana/tokens")
	require.NotContains(t, seen, "GET /v1/captcha/status")
	require.Contains(t, seen, "GET /v1/catalog/products")

	reads, staff, edits, configuration := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	mount := func(seen map[string]int, perms Permissions) {
		RegisterStaffRoutes(recorder{base: "/v1", seen: seen}, &app.Runtime{Config: &config.Config{}}, Options{Auth: authtest.Deny{}, Permissions: perms})
	}
	mount(reads, Permissions{AdminRead: "r"})
	mount(staff, Permissions{AdminRead: "r", AdminWrite: "w"})
	mount(edits, Permissions{AdminRead: "r", CatalogWrite: "e"})
	mount(configuration, Permissions{MerchantConfig: "c"})
	require.Contains(t, reads, "GET /v1/admin/payments")
	require.Contains(t, reads, "POST /v1/admin/subscriptions/{id}/change-tier/preview", "a preview is a read")
	require.NotContains(t, reads, "POST /v1/admin/payments/{id}/refunds", "a write needs AdminWrite")
	require.Contains(t, staff, "POST /v1/admin/payments/{id}/refunds")
	require.NotContains(t, staff, "POST /v1/admin/catalog/products")
	require.Contains(t, staff, "GET /v1/admin/catalog/products")
	require.Contains(t, staff, "POST /v1/admin/catalog/offers/lookup", "a lookup is a read")
	require.Contains(t, edits, "POST /v1/admin/catalog/products")
	require.Contains(t, edits, "GET /v1/admin/catalog/products", "catalog reads are AdminRead's")
	require.NotContains(t, edits, "POST /v1/admin/payments/{id}/refunds")
	require.NotContains(t, configuration, "POST /v1/admin/catalog/products")
	require.NotContains(t, configuration, "GET /v1/admin/catalog/products")
	require.Contains(t, configuration, "GET /v1/admin/psps", "a configuration read is MerchantConfig's")
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
