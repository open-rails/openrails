// Package routes is OpenRails' HTTP surface: the route catalog, and the code
// that mounts it.
//
// Every route is declared once, as a Route in its resource's file. The
// declaration mounts the route (its gates are built from it), and generates
// api/openapi.json, the TypeScript wire types of billing-ui and the admin
// console, and the route tables under docs/api (go run ./scripts/contracts
// -write). A route that is not in the catalog cannot be mounted.
package routes

//go:generate go run ../../../scripts/contracts -write

import (
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
)

const (
	GET, POST, PUT, PATCH, DELETE = http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete
)

// Group is the route group a deployment publishes a route with.
type Group string

const (
	// Meta is the process surface: health, metrics, capability discovery.
	Meta Group = "meta"
	// Checkout is the buyer-facing surface: products, prices, checkout.
	Checkout Group = "checkout"
	// Customer is a customer acting on its own account (/v1/me).
	Customer Group = "customer"
	// Admin is customer support, staff work on customers: staff, machines
	// and the Go client alike, each route behind billing.MerchantCustomerRead
	// or MerchantCustomerManage by its Level, or their root: twins.
	Admin Group = "admin"
	// CatalogAdmin is the catalog, its reads and edits, document
	// applications and moving subscribers between prices at /v1/catalog, behind
	// billing.MerchantCatalogManage or its root: twin. A document skips an
	// object whose field an edit set differently.
	CatalogAdmin Group = "catalog"
	// MerchantConfig is the merchant's own configuration: PSPs, settings,
	// findings, billing import and export, the dashboard layout at /v1/merchant, behind
	// billing.MerchantConfigManage or its root: twin.
	MerchantConfig Group = "merchant_config"
	// Metrics is the merchant's business metrics, read-only: revenue and
	// sales, the metrics queries and the dashboard, behind
	// billing.MerchantMetricsRead or its root: twin.
	Metrics Group = "metrics"
	// Access is what the signed-in caller may use of the staff groups:
	// any person or application the host's auth says it is, mounted with any
	// staff group.
	Access Group = "access"
	// App is the host backend's programmatic routes (/v1/app), mounted with
	// Routes.Programmatic: an application's credential, never a person's,
	// each route behind its own permission (Route.Permission). Each write
	// takes an Idempotency-Key. SCIM provisioning is here too.
	App Group = "app"
	// Webhooks is inbound provider callbacks.
	Webhooks Group = "webhooks"
	// Network is the openrails.dev account across its linked merchants
	// (/v1/network): the network portal's API.
	Network Group = "network"
)

// Tier is what a route checks before its handler runs.
type Tier string

const (
	// AuthPublic: no credential.
	AuthPublic Tier = "public"
	// AuthCustomer: a person acting for itself, as the customer.
	AuthCustomer Tier = "customer"
	// AuthMerchant: a person or an application holding the route's
	// permission (Needs): its merchant: name in the request's merchant's
	// scope, or its root: name in the root scope (Can); a person also signed
	// in recently (CheckRecentSignIn) on a Sensitive route.
	AuthMerchant Tier = "merchant"
	// AuthSignedIn: a person or an application at the request's merchant; no
	// permission.
	AuthSignedIn Tier = "signed_in"
	// AuthApplication: an application at the request's merchant; a person is
	// refused whatever it holds.
	AuthApplication Tier = "application"
	// AuthProvider: the payment provider's own signature on the payload.
	AuthProvider Tier = "provider_signature"
	// AuthProvisioning: the merchant's provisioning token, which opens only
	// these routes, or what AuthApplication admits.
	AuthProvisioning Tier = "provisioning"
	// AuthNetwork: the deployment's own account acting for itself, signed in
	// to it (not through a merchant's client), having accepted the network
	// terms; under one of its links, as the customer that link names. A
	// Sensitive route needs a recent sign-in.
	AuthNetwork Tier = "network"
)

// Feature is the configuration a route needs to be mounted.
type Feature string

const (
	Always Feature = ""
	// FeatureSolana: a Solana PSP is armed.
	FeatureSolana Feature = "solana"
	// FeatureSolanaSigning: OpenRails can sign Solana transactions.
	FeatureSolanaSigning Feature = "solana_signing"
	// FeatureStripePortal: a Stripe PSP is armed. Where it is mounted before
	// that is known (a server's many merchants, PSPs added later), a request
	// whose merchant has none answers route_not_found.
	FeatureStripePortal Feature = "stripe_portal"
	// FeatureMerchantConfigManages: Vault holds merchant configuration, so it
	// can change; a file is read-only.
	FeatureMerchantConfigManages Feature = "merchant_config_edits"
	// FeatureCatalogCopilot: llm.api_key and llm.catalog_copilot_enabled.
	FeatureCatalogCopilot Feature = "catalog_copilot"
	// FeatureMetricsAsk: llm.api_key and llm.ask_enabled.
	FeatureMetricsAsk Feature = "metrics_ask"
	// FeatureDashboardGeneration: llm.api_key.
	FeatureDashboardGeneration Feature = "dashboard_generation"
	// FeatureCustomerNetwork: the deployment runs the customer network
	// (openrails.dev).
	FeatureCustomerNetwork Feature = "customer_network"
)

// Level is a customer-support or catalog route's: a read, or an update. A
// support read needs billing.MerchantCustomerRead, an update
// MerchantCustomerManage, or their root: twins; a catalog update refuses
// while the catalog is not edited over HTTP.
// Lookups, previews, checks and queries sent as POST are reads.
type Level string

const (
	LevelRead   Level = "read"
	LevelUpdate Level = "update"
)

// MerchantSource is where a public route, which takes no credential, finds
// the merchant whose rows it reads.
type MerchantSource string

const (
	// MerchantQuery: the merchant id its ?merchant= parameter names.
	MerchantQuery MerchantSource = "query"
	// MerchantOrder: the merchant of the order its {id} names.
	MerchantOrder MerchantSource = "order"
)

// CheckoutAccess is how the hosted checkout host serves a route under a
// checkout URL's secret, which acts as its order's customer.
type CheckoutAccess string

const (
	// CheckoutPublic: a public route.
	CheckoutPublic CheckoutAccess = "public"
	// CheckoutMerchant: a customer route about the order's merchant.
	CheckoutMerchant CheckoutAccess = "merchant"
	// CheckoutOrder: a customer route whose {id} is the secret's order.
	CheckoutOrder CheckoutAccess = "order"
	// CheckoutSavedCards: a customer route served only when the checkout
	// offers saved cards.
	CheckoutSavedCards CheckoutAccess = "saved_cards"
	// CheckoutLink: a customer route the order's customer uses to link its
	// openrails.dev account, the checkout URL proving the merchant's side.
	CheckoutLink CheckoutAccess = "link"
)

// Reply is one success outcome of a route: its status and body. Body is a
// zero value of the body's type; nil for none.
type Reply struct {
	Status int
	Body   any
}

// Stream is a body that is not JSON: an archive, a script.
type Stream struct{ ContentType string }

// Param is one query parameter.
type Param struct {
	Name string
	// Kind is string, integer, boolean, date-time, ids or strings (a
	// repeated parameter).
	Kind string
	// Checked: the mount refuses a value that is not a non-negative integer
	// (400 invalid_query) before the handler reads it.
	Checked bool
	// Required: the route refuses a request without it.
	Required bool
}

func text(name string) Param    { return Param{Name: name, Kind: "string"} }
func integer(name string) Param { return Param{Name: name, Kind: "integer", Checked: true} }

// required is p, which its route refuses a request without.
func required(p Param) Param { p.Required = true; return p }

// repeated is a parameter given once per value, for values that may hold a
// comma.
func repeated(name string) Param { return Param{Name: name, Kind: "strings"} }

// pageParams are a cursor-paged list's query: Request.Page reads them.
var pageParams = []Param{text("cursor"), integer("limit")}

// idsParam names up to billing.MaxBatchItems records of a list, comma
// separated. The mount refuses a longer list, and any other parameter beside
// it: the answer is the named records, whatever their state, in one page.
var idsParam = Param{Name: "ids", Kind: "ids"}

// params lists a route's query parameters, sorted by name.
func params(parts ...any) []Param {
	var out []Param
	for _, part := range parts {
		switch p := part.(type) {
		case Param:
			out = append(out, p)
		case []Param:
			out = append(out, p...)
		default:
			panic(fmt.Sprintf("routes: query part %T", part))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// queryOf lists the parameters a handler binds from v's `form` tags
// (Request.BindQuery), which refuses a malformed one itself.
func queryOf(v any) []Param {
	var out []Param
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return
		}
		for i := range t.NumField() {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("form"), ",")
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if name == "" || name == "-" {
				if ft.Kind() == reflect.Struct && ft.String() != "time.Time" {
					walk(ft)
				}
				continue
			}
			kind := "string"
			switch {
			case ft.String() == "time.Time":
				kind = "date-time"
			case ft.Kind() == reflect.Bool:
				kind = "boolean"
			case ft.Kind() >= reflect.Int && ft.Kind() <= reflect.Uint64:
				kind = "integer"
			}
			out = append(out, Param{Name: name, Kind: kind})
		}
	}
	walk(reflect.TypeOf(v))
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// codes lists a route's error codes; each must be in billing's registry.
func codes(list ...string) []string {
	for _, code := range list {
		if _, ok := billing.LookupErrorCode(code); !ok {
			panic("routes: error code " + code + " is not registered in billing")
		}
	}
	sort.Strings(list)
	return list
}

// Route is one route of the HTTP API: the catalog entry that mounts it, gates
// it and documents it.
type Route struct {
	Method string
	// Path is the route from the API root, with ServeMux wildcards:
	// "/v1/admin/payments/{id}". A deployment's mount prefix is not part of
	// it.
	Path  string
	Group Group
	// Auth is the tier the route enforces before its handler runs.
	Auth Tier
	// Name is the route's Go client method: every staff, programmatic and
	// access route has one, and a public route may (ClientMethod).
	Name string
	// Level is an admin route's: read or write.
	Level Level
	// Permission is a programmatic route's: the pair its caller holds one
	// of. A staff route's follows from its Group and Level (Needs).
	Permission Pair
	// Sensitive: the route moves money or removes access; a person needs a
	// recent sign-in for it.
	Sensitive bool
	// Limit meters the operation per human administrator, after authorization.
	Limit middleware.AdminOperation
	// When is the configuration the route needs to be mounted.
	When Feature
	// NoConn: the route pins no merchant database connection.
	NoConn bool
	// Merchant is where a public route finds its merchant; a public route
	// that reads no merchant's rows is NoConn instead.
	Merchant MerchantSource
	// IdempotencyKey: the route reads the Idempotency-Key header.
	IdempotencyKey bool
	// Checkout is what a checkout URL's secret opens of the route on the
	// hosted checkout host; none is not served there.
	Checkout CheckoutAccess

	// Query lists the query parameters; Request is a zero value of the JSON
	// body's type (nil for none); Responses is every success outcome.
	Query     []Param
	Request   any
	Responses []Reply
	// Errors lists the codes the route's handler and the services it calls
	// name; the codes every route of its kind answers are in ErrorSets.
	Errors []string

	// Handler serves the route; Bind builds the handler from the assembly
	// when it needs more than the request. Exactly one is set.
	Handler router.Handler
	Bind    func(*Env) router.Handler
}

// Key is the route's identity: "GET /v1/admin/payments/{id}".
func (r Route) Key() string { return r.Method + " " + r.Path }

// Staff reports a route of a staff group: Admin, CatalogAdmin,
// MerchantConfig and Metrics.
func (r Route) Staff() bool {
	return r.Group == Admin || r.Group == CatalogAdmin || r.Group == MerchantConfig || r.Group == Metrics
}

// ClientMethod reports a route the Go Client calls: the staff, programmatic
// and access routes.
func (r Route) ClientMethod() bool {
	return r.Staff() || r.Auth == AuthApplication || r.Auth == AuthSignedIn
}

// CatalogUpdate reports a catalog edit or document application.
func (r Route) CatalogUpdate() bool { return r.Group == CatalogAdmin && r.Level == LevelUpdate }

// AppWrite reports a programmatic write: it replays by Idempotency-Key. A
// check sent as POST is a read (LevelRead), and a SCIM write is idempotent by
// its own protocol.
func (r Route) AppWrite() bool {
	return r.Auth == AuthApplication && r.Method != GET && r.Level != LevelRead
}

// h adapts a handler func to the neutral router.Handler type.
func h(fn func(r *httprequest.Request)) router.Handler { return router.Handler(fn) }

// Catalog is OpenRails' whole HTTP surface: every route any configuration
// can mount, in a stable order. It needs no database.
func Catalog() []Route { return allRoutes }

// Lookup returns the catalog's route for a method and path.
func Lookup(method, path string) (Route, bool) {
	r, ok := index[method+" "+path]
	return r, ok
}

var allRoutes, index = func() ([]Route, map[string]Route) {
	var all []Route
	for _, resource := range [][]Route{
		metaRoutes, configRoutes, ordersRoutes, cartsRoutes, catalogRoutes, subscriptionsRoutes, entitlementsRoutes, customersRoutes,
		creditsRoutes, meteringRoutes, paymentsRoutes, paymentMethodsRoutes, agreementsRoutes, pspsRoutes,
		merchantRoutes, opsRoutes, provisioningRoutes, customerNetworkRoutes, networkRoutes(),
	} {
		all = append(all, resource...)
	}
	byKey := make(map[string]Route, len(all))
	for _, r := range all {
		if _, dup := byKey[r.Key()]; dup {
			panic("routes: declared twice: " + r.Key())
		}
		if (r.Handler == nil) == (r.Bind == nil) {
			panic("routes: " + r.Key() + " needs exactly one of Handler and Bind")
		}
		leveled := r.Group == Admin || r.Group == CatalogAdmin
		appRead := r.Group == App && r.Level == LevelRead && r.Method == POST
		if leveled != (r.Level == LevelRead || r.Level == LevelUpdate) && !appRead || !leveled && r.Level != "" && !appRead {
			panic("routes: " + r.Key() + ": an admin or catalog route, and only one, reads or updates; a programmatic POST may say it reads")
		}
		if (r.Group == Network) != (r.Auth == AuthNetwork) || r.Group == Network && !strings.HasPrefix(r.Path, "/v1/network/") {
			panic("routes: " + r.Key() + ": a network route, and only one, is the account's, under /v1/network")
		}
		if (r.Group == Access) != (r.Auth == AuthSignedIn) {
			panic("routes: " + r.Key() + ": the access route, and only it, takes any signed-in caller")
		}
		if (r.Group == App) != (r.Auth == AuthApplication || r.Auth == AuthProvisioning) {
			panic("routes: " + r.Key() + ": a programmatic route, and only one, takes an application or a provisioning token")
		}
		if r.Group == App && (r.Sensitive || r.IdempotencyKey != r.AppWrite() || !strings.HasPrefix(r.Path, "/v1/app/")) {
			panic("routes: " + r.Key() + ": a programmatic route is under /v1/app, never asks for a sign-in, and each write takes an Idempotency-Key")
		}
		if (r.Auth == AuthApplication) != (r.Permission != Pair{}) {
			panic("routes: " + r.Key() + ": a programmatic route, and only one, names its permission")
		}
		if err := r.Needs().check(); err != nil {
			panic("routes: " + r.Key() + ": " + err.Error())
		}
		switch r.Merchant {
		case "":
			if r.Auth == AuthPublic && !r.NoConn {
				panic("routes: " + r.Key() + ": a public route names its merchant in the request, or reads no merchant's rows (NoConn)")
			}
		case MerchantQuery, MerchantOrder:
			named := slices.Contains(r.Query, required(text("merchant")))
			if r.Auth != AuthPublic || r.NoConn || (r.Merchant == MerchantQuery) != named || r.Merchant == MerchantOrder && !strings.Contains(r.Path, "{id}") {
				panic("routes: " + r.Key() + ": only a public route names its merchant, as ?merchant= or by the order its {id} names")
			}
		default:
			panic("routes: " + r.Key() + ": unknown merchant source " + string(r.Merchant))
		}
		switch r.Checkout {
		case "":
		case CheckoutPublic:
			if r.Auth != AuthPublic {
				panic("routes: " + r.Key() + ": a public checkout route takes no credential")
			}
		case CheckoutOrder, CheckoutSavedCards, CheckoutLink, CheckoutMerchant:
			if r.Auth != AuthCustomer || (r.Checkout == CheckoutOrder) != strings.Contains(r.Path, "{id}") {
				panic("routes: " + r.Key() + ": a checkout customer route is a customer route; an order route names the order as {id}")
			}
		default:
			panic("routes: " + r.Key() + ": unknown checkout access " + string(r.Checkout))
		}
		byKey[r.Key()] = r
	}
	return all, byKey
}()

// requestShapeErrors are the codes any route with a JSON body or a query can
// answer before its handler runs.
var requestShapeErrors = []string{
	billing.CodeInvalidParam, billing.CodeInvalidQuery, billing.CodeInvalidRequestBody, billing.CodeRequestBodyTooLarge,
	billing.CodeUnknownField, billing.CodeUnsupportedMediaType,
}

// TierErrors lists the codes a tier answers before any handler runs.
func TierErrors(tier Tier) []string {
	common := []string{billing.CodeInternalError, billing.CodeRateLimitExceeded, "captcha_required", "captcha_invalid", "database_busy"}
	// authenticated are the codes of asking who a request is: the host's
	// answer, or the in-process host's.
	authenticated := []string{
		billing.CodeAuthenticationRequired, billing.CodeCredentialExpired, billing.CodeCredentialRevoked,
		billing.CodePermissionRequired, billing.CodeAuthenticationUnavailable, billing.CodeAuthorizationUnavailable,
		billing.CodeMerchantUnresolved,
	}
	staff := []string{billing.CodeHostPrincipalInvalid}
	var own []string
	switch tier {
	case AuthCustomer:
		// network_account_linked: an account linked to another customer of
		// the merchant, where the customer network runs.
		own = append([]string{billing.CodeInvokerScopedPrincipal, billing.CodeCustomerIssuerMismatch, billing.CodeNetworkAccountLinked}, authenticated...)
	case AuthMerchant:
		own = append(append([]string{billing.CodeStepUpRequired, billing.CodeStepUpUnavailable}, staff...), authenticated...)
	case AuthSignedIn:
		own = append(slices.Clone(staff), authenticated...)
	case AuthApplication:
		own = append(append([]string{billing.CodeApplicationRequired}, staff...), authenticated...)
	case AuthNetwork:
		own = append([]string{billing.CodeNetworkTermsRequired, billing.CodeStepUpRequired, billing.CodeStepUpUnavailable, billing.CodeResourceNotFound}, authenticated...)
	default:
		// No credential: a public route names its own merchant, a provider's
		// callback its PSP.
		return sortedCodes(common)
	}
	return sortedCodes(append(common, own...))
}

func sortedCodes(codes []string) []string {
	out := slices.Clone(codes)
	sort.Strings(out)
	return slices.Compact(out)
}

// ErrorSets names the shared code sets a route answers besides its own
// Errors: its tier's, and the request-shape and programmatic-write codes
// where they apply. ErrorSet lists each set's codes.
func (r Route) ErrorSets() []string {
	sets := []string{"tier:" + string(r.Auth)}
	if r.Request != nil || len(r.Query) > 0 {
		sets = append(sets, "request")
	}
	if r.AppWrite() {
		sets = append(sets, "app_write")
	}
	return sets
}

// ErrorSet lists the codes of a set ErrorSets names.
func ErrorSet(name string) []string {
	switch name {
	case "request":
		return requestShapeErrors
	case "app_write":
		return []string{"idempotency_key_in_progress", "idempotency_key_required", billing.CodeIdempotencyKeyReused}
	}
	if tier, ok := strings.CutPrefix(name, "tier:"); ok {
		return TierErrors(Tier(tier))
	}
	return nil
}

// AllErrors is every code the route can answer.
func (r Route) AllErrors() []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range append([][]string{r.Errors}, mapSets(r.ErrorSets())...) {
		for _, code := range list {
			if !seen[code] {
				seen[code] = true
				out = append(out, code)
			}
		}
	}
	sort.Strings(out)
	return out
}

func mapSets(names []string) [][]string {
	out := make([][]string, len(names))
	for i, name := range names {
		out[i] = ErrorSet(name)
	}
	return out
}
