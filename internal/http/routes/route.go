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
	// and the Go client alike, each route behind the host's AdminRead or
	// AdminUpdate by its Level.
	Admin Group = "admin"
	// CatalogAdmin is the catalog, its reads and edits, document
	// applications and moving subscribers between prices, behind the host's
	// Catalog. A document skips an object whose field an edit set differently.
	CatalogAdmin Group = "catalog"
	// MerchantConfig is the merchant's own configuration: PSPs, settings,
	// billing import and export, the dashboard layout, behind the host's
	// MerchantConfig.
	MerchantConfig Group = "merchant_config"
	// Metrics is the merchant's business metrics, read-only: revenue and
	// sales, the metrics queries and the dashboard, behind the host's Metrics.
	Metrics Group = "metrics"
	// Access is what the signed-in caller may use of the staff groups:
	// any person or application the host's auth says it is, mounted with any
	// staff group.
	Access Group = "access"
	// App is the host backend's programmatic routes (/v1/app), mounted with
	// Routes.Programmatic: an application's credential, never a person's.
	// Each write takes an Idempotency-Key. SCIM provisioning is here too.
	App Group = "app"
	// Webhooks is inbound provider callbacks.
	Webhooks Group = "webhooks"
)

// Tier is what a route checks before its handler runs.
type Tier string

const (
	// AuthPublic: no credential.
	AuthPublic Tier = "public"
	// AuthCheckoutSession: the opaque ocs_ capability, resolved to its stored
	// merchant; a presented customer credential only narrows what it shows.
	AuthCheckoutSession Tier = "checkout_session"
	// AuthSessionID: the id in the path is the credential.
	AuthSessionID Tier = "session_id"
	// AuthCustomer: a person acting for itself, as the customer.
	AuthCustomer Tier = "customer"
	// AuthMerchant: a person or an application holding the route's group
	// permission in the request's merchant's scope (Can); a person also
	// signed in recently (CheckRecentSignIn) on a Sensitive route.
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
)

// Feature is the configuration a route needs to be mounted.
type Feature string

const (
	Always Feature = ""
	// FeatureSolana: a Solana PSP is armed.
	FeatureSolana Feature = "solana"
	// FeatureSolanaSigning: OpenRails can sign Solana transactions.
	FeatureSolanaSigning Feature = "solana_signing"
	// FeatureStripePortal: a Stripe PSP is armed.
	FeatureStripePortal Feature = "stripe_portal"
	// FeatureMerchantDirectory: the deployment has a merchant directory.
	FeatureMerchantDirectory Feature = "merchant_directory"
	// FeatureCatalogCopilot: llm.api_key and llm.catalog_copilot_enabled.
	FeatureCatalogCopilot Feature = "catalog_copilot"
	// FeatureMetricsAsk: llm.api_key and llm.ask_enabled.
	FeatureMetricsAsk Feature = "metrics_ask"
	// FeatureDashboardGeneration: llm.api_key.
	FeatureDashboardGeneration Feature = "dashboard_generation"
)

// Level is a customer-support or catalog route's: a read, or an update. A
// support read mounts with AdminRead, an update with AdminUpdate; a
// catalog update refuses while the catalog is not edited over HTTP.
// Lookups, previews, checks and queries sent as POST are reads.
type Level string

const (
	LevelRead   Level = "read"
	LevelUpdate Level = "update"
)

// Throttle is a route's own limiter, beside the deployment's rate limits.
type Throttle string

const (
	// ThrottleSessionRead and ThrottleSessionPay bound a checkout session per
	// session id.
	ThrottleSessionRead Throttle = "checkout_session_read"
	ThrottleSessionPay  Throttle = "checkout_session_pay"
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
}

func text(name string) Param    { return Param{Name: name, Kind: "string"} }
func integer(name string) Param { return Param{Name: name, Kind: "integer", Checked: true} }

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
	// Sensitive: the route moves money or removes access; a person needs a
	// recent sign-in for it.
	Sensitive bool
	// Limit meters the operation per human administrator, after authorization.
	Limit middleware.AdminOperation
	// Throttle is the route's own limiter.
	Throttle Throttle
	// When is the configuration the route needs to be mounted.
	When Feature
	// NoConn: the route pins no merchant database connection.
	NoConn bool
	// IdempotencyKey: the route reads the Idempotency-Key header.
	IdempotencyKey bool

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

// Staff reports a route behind a group permission: Admin's,
// CatalogAdmin's, MerchantConfig's and Metrics'.
func (r Route) Staff() bool {
	return r.Group == Admin || r.Group == CatalogAdmin || r.Group == MerchantConfig || r.Group == Metrics
}

// ClientMethod reports a route the Go Client calls: the staff, programmatic
// and access routes, and the public reads it names (the catalog's offers).
func (r Route) ClientMethod() bool {
	return r.Staff() || r.Auth == AuthApplication || r.Auth == AuthSignedIn || r.Auth == AuthPublic && r.Name != ""
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
		metaRoutes, configRoutes, checkoutRoutes, ordersRoutes, catalogRoutes, subscriptionsRoutes, entitlementsRoutes, customersRoutes,
		creditsRoutes, meteringRoutes, invoicesRoutes, paymentsRoutes, paymentMethodsRoutes, pspsRoutes,
		merchantRoutes, opsRoutes, provisioningRoutes,
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
		if (r.Group == Access) != (r.Auth == AuthSignedIn) {
			panic("routes: " + r.Key() + ": the access route, and only it, takes any signed-in caller")
		}
		if (r.Group == App) != (r.Auth == AuthApplication || r.Auth == AuthProvisioning) {
			panic("routes: " + r.Key() + ": a programmatic route, and only one, takes an application or a provisioning token")
		}
		if r.Group == App && (r.Sensitive || r.IdempotencyKey != r.AppWrite() || !strings.HasPrefix(r.Path, "/v1/app/")) {
			panic("routes: " + r.Key() + ": a programmatic route is under /v1/app, never asks for a sign-in, and each write takes an Idempotency-Key")
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

// selectorErrors are the codes of the OpenRails-Merchant selector.
var selectorErrors = []string{
	billing.CodeMerchantBindingMismatch, billing.CodeMerchantDirectoryUnavailable, billing.CodeMerchantNotFound, billing.CodeMerchantSelectorInvalid,
}

// TierErrors lists the codes a tier answers before any handler runs.
func TierErrors(tier Tier) []string {
	common := []string{billing.CodeInternalError, billing.CodeRateLimitExceeded, "captcha_required", "captcha_invalid", "database_busy"}
	// authenticated are the codes of asking who a request is: the host's
	// answer, or one of OpenRails' own Authenticators'.
	authenticated := []string{
		billing.CodeAuthenticationRequired, billing.CodeCredentialExpired, billing.CodeCredentialRevoked, billing.CodeSenderProofRequired,
		billing.CodePermissionRequired, billing.CodeAuthenticationUnavailable, billing.CodeAccessTokenInvalid,
		billing.CodeAccessTokenIssuerUnknown, billing.CodeAccessTokenMerchantNotBound, billing.CodeDPoPNonceRequired,
		billing.CodeInsufficientScope, billing.CodeMerchantUnresolved, billing.CodeHostMerchantMismatch,
	}
	staff := []string{
		billing.CodeServiceCredentialInvalid, billing.CodeServiceCredentialMerchantUnresolved, billing.CodeServiceCredentialResourceScopeDenied,
		billing.CodeHostPrincipalInvalid, billing.CodeMerchantContextMismatch, billing.CodeAuthorizationUnavailable,
	}
	var own []string
	switch tier {
	case AuthPublic, AuthSessionID:
		own = []string{billing.CodeMerchantNotFound} // the Host names no merchant
	case AuthCustomer:
		own = append([]string{billing.CodeInvokerScopedPrincipal}, authenticated...)
	case AuthMerchant:
		own = append(append([]string{billing.CodeStepUpRequired, billing.CodeStepUpUnavailable}, staff...), authenticated...)
	case AuthSignedIn:
		own = append(slices.Clone(staff), authenticated...)
	case AuthApplication:
		own = append(append([]string{billing.CodeApplicationRequired}, staff...), authenticated...)
	default:
		return sortedCodes(common)
	}
	return sortedCodes(append(append(common, own...), selectorErrors...))
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
