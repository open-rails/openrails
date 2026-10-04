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
	// Treasury is a customer acting on an account it co-manages
	// (/v1/customers/{customer_id}).
	Treasury Group = "customer_treasury"
	// Merchant is the merchant API: staff, machines and the Go client alike,
	// each route gated by its merchant permission.
	Merchant Group = "merchant"
	// CatalogOwned is a creator managing its own catalog (/v1/catalog).
	CatalogOwned Group = "catalog_owned"
	// ControlPlane is the standalone server's merchant accounts, team and
	// API keys.
	ControlPlane Group = "control_plane"
	// Platform is the standalone operator tier (/v1/platform).
	Platform Group = "platform"
	// Webhooks is inbound provider callbacks.
	Webhooks Group = "webhooks"
)

// Tier is what a route checks before its handler runs.
type Tier string

const (
	// AuthPublic: no credential.
	AuthPublic Tier = "public"
	// AuthOptional: a user credential is read when present.
	AuthOptional Tier = "optional"
	// AuthSessionID: the id in the path is the credential.
	AuthSessionID Tier = "session_id"
	// AuthUser: any signed-in user.
	AuthUser Tier = "user"
	// AuthCustomer: a customer principal, delegated or native.
	AuthCustomer Tier = "customer"
	// AuthCustomerGrant: a customer principal holding Perm, a customer: grant.
	AuthCustomerGrant Tier = "customer_grant"
	// AuthMerchant: a credential holding Perm, a merchant: permission, on the
	// request's merchant (API key, service JWT, delegated token or user
	// session).
	AuthMerchant Tier = "merchant"
	// AuthOperator: a human session holding Perm, a root: grant.
	AuthOperator Tier = "operator"
	// AuthProvider: the payment provider's own signature on the payload.
	AuthProvider Tier = "provider_signature"
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
	// FeatureHostedCheckout: the deployment publishes hosted checkout.
	FeatureHostedCheckout Feature = "hosted_checkout"
	// FeatureMerchantDirectory: the deployment has a merchant directory.
	FeatureMerchantDirectory Feature = "merchant_directory"
	// FeatureCatalogCopilot: llm.api_key and llm.catalog_copilot_enabled.
	FeatureCatalogCopilot Feature = "catalog_copilot"
	// FeatureMetricsAsk: llm.api_key and llm.ask_enabled.
	FeatureMetricsAsk Feature = "metrics_ask"
	// FeatureDashboardGeneration: llm.api_key.
	FeatureDashboardGeneration Feature = "dashboard_generation"
	// FeatureMerchantCreation: a hosted merchant-creation policy is declared.
	FeatureMerchantCreation Feature = "merchant_creation"
)

// CustomerScope is how much of the customer surface an exposure serves; each
// scope includes the ones after it.
type CustomerScope string

const (
	// ScopeSelfService: every customer route, purchases included.
	ScopeSelfService CustomerScope = ""
	// ScopeBillingManagement: history, access, payment methods and the
	// agreements the customer already has.
	ScopeBillingManagement CustomerScope = "billing_management"
	// ScopeSubscriptionManagement: cancel, resume and pick the paying card.
	ScopeSubscriptionManagement CustomerScope = "subscription_management"
)

// Throttle is a route's own limiter, beside the deployment's rate limits.
type Throttle string

const (
	// ThrottleSessionRead and ThrottleSessionPay bound hosted checkout per
	// session id.
	ThrottleSessionRead Throttle = "checkout_session_read"
	ThrottleSessionPay  Throttle = "checkout_session_pay"
	// ThrottleMerchantCreation bounds merchant creation per address and user.
	ThrottleMerchantCreation Throttle = "merchant_creation"
)

// Reply is one success outcome of a route: its status and body. Body is a
// zero value of the body's type; nil for none.
type Reply struct {
	Status int
	Body   any
}

// Untyped is a JSON object whose shape no Go type declares yet. A route that
// answers or accepts one is not frozen: its lane replaces it with a type.
type Untyped map[string]any

// Message is the {message} body some writes still answer.
type Message struct {
	Message string `json:"message"`
}

// PathPage is the offset list that echoes its request path.
type PathPage[T any] struct {
	Object  string `json:"object"`
	Data    []T    `json:"data"`
	Total   int64  `json:"total"`
	Limit   int    `json:"limit"`
	Offset  int    `json:"offset"`
	HasMore bool   `json:"has_more"`
	URL     string `json:"url"`
}

// Stream is a body that is not JSON: an archive, a script.
type Stream struct{ ContentType string }

// Param is one query parameter.
type Param struct {
	Name string
	// Kind is string, integer, boolean or date-time.
	Kind string
	// Checked: the mount refuses a value that is not a non-negative integer
	// (400 invalid_query) before the handler reads it.
	Checked bool
}

func text(name string) Param    { return Param{Name: name, Kind: "string"} }
func integer(name string) Param { return Param{Name: name, Kind: "integer", Checked: true} }

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
	// "/v1/merchant/payments/{id}". A deployment's mount prefix is not part
	// of it.
	Path  string
	Group Group
	// Auth is the tier the route enforces before its handler runs.
	Auth Tier
	// Perm is the permission Auth checks: a merchant: permission
	// (AuthMerchant), a customer: grant (AuthCustomerGrant) or a root: grant
	// (AuthOperator).
	Perm string
	// Also is a second merchant permission the route needs.
	Also string
	// Limit meters the operation per human administrator, after authorization.
	Limit middleware.AdminOperation
	// Throttle is the route's own limiter.
	Throttle Throttle
	// When is the configuration the route needs to be mounted.
	When Feature
	// Scope is the narrowest customer exposure that serves a Customer route.
	Scope CustomerScope
	// InvokerScoped: an invoker-scoped credential, which spends a customer's
	// balance without being the customer, may call this Customer route.
	InvokerScoped bool
	// CatalogWrite: the route changes the catalog over HTTP, which a
	// deployment enables (Config.AllowCatalogUpdates); it is not mounted
	// otherwise.
	CatalogWrite bool
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

// Untyped reports whether the route accepts or answers a body no Go type
// declares.
func (r Route) Untyped() bool {
	if _, ok := r.Request.(Untyped); ok {
		return true
	}
	for _, reply := range r.Responses {
		if _, ok := reply.Body.(Untyped); ok {
			return true
		}
	}
	return false
}

// Key is the route's identity: "GET /v1/merchant/payments/{id}".
func (r Route) Key() string { return r.Method + " " + r.Path }

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
		metaRoutes, checkoutRoutes, catalogRoutes, subscriptionsRoutes, entitlementsRoutes, customersRoutes,
		creditsRoutes, meteringRoutes, invoicesRoutes, paymentsRoutes, paymentMethodsRoutes, pspsRoutes,
		merchantRoutes, opsRoutes, platformRoutes,
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
	var own []string
	switch tier {
	case AuthUser:
		own = []string{billing.CodeAuthenticationRequired, billing.CodeCredentialExpired, billing.CodeCredentialRevoked, billing.CodeSenderProofRequired}
	case AuthCustomer, AuthCustomerGrant:
		own = append([]string{
			billing.CodeAuthenticationRequired, billing.CodeCredentialExpired, billing.CodeCredentialRevoked, billing.CodeSenderProofRequired,
			billing.CodeDelegatedTokenInvalid, billing.CodeDelegatedTokenExpired, billing.CodeDelegatedTokenRevoked, billing.CodeDelegatedPrincipalInvalid,
			billing.CodeDelegatedMerchantUnresolved, billing.CodeDelegatedVerificationUnavailable, billing.CodeInvokerScopedPrincipal,
		}, selectorErrors...)
		if tier == AuthCustomerGrant {
			own = append(own, billing.CodePermissionRequired, billing.CodeCustomerScopeMismatch, billing.CodeStepUpRequired, billing.CodeStepUpUnavailable, billing.CodeAuthorizationUnavailable)
		}
	case AuthMerchant:
		own = append([]string{
			billing.CodeAuthenticationRequired, billing.CodeCredentialExpired, billing.CodeCredentialRevoked, billing.CodeCredentialIdentityMismatch,
			billing.CodeSenderProofRequired, billing.CodeServiceCredentialInvalid, billing.CodeServiceCredentialMerchantUnresolved,
			billing.CodeServiceCredentialResourceScopeDenied, billing.CodeDelegatedTokenInvalid, billing.CodeDelegatedPrincipalInvalid,
			billing.CodeDelegatedVerificationUnavailable, billing.CodeHostPrincipalInvalid, billing.CodePermissionRequired,
			billing.CodeMerchantUnresolved, billing.CodeHostMerchantMismatch, billing.CodeMerchantContextMismatch, billing.CodeStepUpRequired,
			billing.CodeStepUpUnavailable, billing.CodeAuthenticationUnavailable, billing.CodeAuthorizationUnavailable,
		}, selectorErrors...)
	case AuthOperator:
		own = []string{billing.CodeAuthenticationRequired, billing.CodeCredentialExpired, billing.CodeCredentialRevoked, billing.CodePermissionRequired}
	}
	out := append(common, own...)
	sort.Strings(out)
	return out
}

// ErrorSets names the shared code sets a route answers besides its own
// Errors: its tier's, and the request-shape and catalog-write codes where they
// apply. ErrorSet lists each set's codes.
func (r Route) ErrorSets() []string {
	sets := []string{"tier:" + string(r.Auth)}
	if r.Request != nil || len(r.Query) > 0 {
		sets = append(sets, "request")
	}
	if r.CatalogWrite {
		sets = append(sets, "catalog_write")
	}
	if r.Group == CatalogOwned {
		sets = append(sets, "owned_catalog")
	}
	return sets
}

// ErrorSet lists the codes of a set ErrorSets names.
func ErrorSet(name string) []string {
	switch name {
	case "request":
		return requestShapeErrors
	case "catalog_write":
		return []string{"catalog_declared", "catalog_updates_disabled"}
	case "owned_catalog":
		return []string{billing.CodeCatalogOwnerRequired, "catalog_not_found", "catalog_scope_mismatch"}
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
