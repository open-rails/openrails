// Package routes is OpenRails' HTTP surface: the route catalog, and the code
// that mounts it.
//
// Every route is declared once, as a Route in its resource's file. The
// declaration mounts the route (its gates are built from it), and generates
// the Go Client's method (client_gen.go), api/openapi.json, the TypeScript
// wire types of billing-ui and the admin console, and the route tables under
// docs/api (go run ./scripts/contracts -write). A route that is not in the
// catalog cannot be mounted.
package routes

//go:generate go run ../../../scripts/contracts -write

import (
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/wireform"
)

const (
	GET, POST, PUT, PATCH, DELETE = http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete
)

// Group is the route group a deployment publishes a route with.
type Group string

const (
	// Meta is the process surface: health, metrics, capability discovery.
	Meta Group = "meta"
	// Payment is the public buyer-facing surface: a wallet paying an invoice.
	Payment Group = "payment"
	// Customer is a customer acting on its own account (/v1/me).
	Customer Group = "customer"
	// Admin is customer support, staff work on customers: staff, machines
	// and the Go client alike, each route behind billing.MerchantCustomerRead
	// or MerchantCustomerManage by its Level, or their root: twins.
	Admin Group = "admin"
	// CatalogAdmin is the catalog, its reads and edits, document
	// applications and moving subscribers between prices at /v1/catalog, behind
	// billing.MerchantCatalogRead for reads and MerchantCatalogManage for edits,
	// or their root: twins. A document skips an
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
	// scope, or its root: name in the root scope (Can). Admin changes do not
	// require a recent sign-in.
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
	// these routes and is their grant, or an application holding the
	// route's permission, as AuthApplication.
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
	// FeatureStripePortal: a Stripe PSP is armed. Where it is mounted before
	// that is known (a server's many merchants, PSPs added later), a request
	// whose merchant has none answers route_not_found.
	FeatureStripePortal Feature = "stripe_portal"
	// FeatureMerchantConfigManages: Vault holds merchant configuration, so it
	// can change; a file is read-only.
	FeatureMerchantConfigManages Feature = "merchant_config_edits"
	// FeatureCustomerProvisioning: OpenRails keeps the pushed customer directory.
	FeatureCustomerProvisioning Feature = "customer_provisioning"
)

// Level is a customer-support, catalog or merchant-config route's: a read, or an update. A
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

// MerchantInvoice: the merchant of the invoice its {id} names.
const MerchantInvoice MerchantSource = "invoice"

// PayURLAccess is how the checkout origin serves a route under a
// pay URL's secret, which acts as its invoice's customer.
type PayURLAccess string

const (
	// PayURLPublic: a public route.
	PayURLPublic PayURLAccess = "public"
	// PayURLMerchant: a customer route about the invoice's merchant.
	PayURLMerchant PayURLAccess = "merchant"
	// PayURLInvoice: a customer route whose {id} is the secret's invoice.
	PayURLInvoice PayURLAccess = "invoice"
	// PayURLSavedCards: a customer route served only when the pay URL
	// offers saved cards.
	PayURLSavedCards PayURLAccess = "saved_cards"
)

// ClientSource is how a route's Go Client method is written.
type ClientSource string

// HandWritten: the method is client_handwritten.go's, not generated: a
// stream, or a read the client answers from memory itself.
const HandWritten ClientSource = "hand_written"

// Reply is one success outcome of a route: its status and body. Body is a
// zero value of the body's type; nil for none.
type Reply struct {
	Status int
	Body   any
}

// Stream is a body that is not JSON: an archive, a script.
type Stream struct{ ContentType string }

// codes lists a route's error codes; each must be in billing's registry.
func codes(list ...billing.ErrorCode) []billing.ErrorCode {
	for _, code := range list {
		if _, ok := billing.LookupErrorCode(code); !ok {
			panic("routes: error code " + string(code) + " is not registered in billing")
		}
	}
	slices.Sort(list)
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
	// Name is the route's Go client method.
	Name string
	// Doc is a client route's method comment and OpenAPI description: a
	// sentence beginning with Name.
	Doc string
	// Args holds a zero value of each path wildcard's type, in path order:
	// the client method's arguments, each a string or a Stringer.
	Args []any
	// Cache is the read cache that may answer a client GET; empty reads live.
	Cache billing.Read
	// Client is how the client method is written: generated, or HandWritten.
	Client ClientSource
	// Level is an admin route's: read or write.
	Level Level
	// Permission is an application or provisioning route's: the pair its
	// caller holds one of. A staff route's follows from its Group and Level
	// (Needs) unless it names its own. A tier that checks one never goes
	// without (authority).
	Permission Pair
	// Sensitive: a customer operation needs a recent sign-in. Only a
	// customer route declares it.
	Sensitive bool
	// Person: a merchant operation only a user acting in person, or the
	// in-process host, may take; an API key or service token holding the
	// permission is refused person_required.
	Person bool
	// Limit meters the operation per human administrator, after authorization.
	Limit middleware.AdminOperation
	// When is the configuration the route needs to be mounted.
	When Feature
	// EventStore: the route needs the event store. It is mounted on every
	// deployment; without a store it answers event_store_required once its
	// caller is authenticated.
	EventStore bool
	// NoConn: the route pins no merchant database connection.
	NoConn bool
	// Merchant is where a public route finds its merchant; a public route
	// that reads no merchant's rows is NoConn instead.
	Merchant MerchantSource
	// IdempotencyKey: the route reads the Idempotency-Key header.
	IdempotencyKey bool
	// PayURL is what a pay URL's secret opens of the route on the
	// checkout origin; none is not served there.
	PayURL PayURLAccess
	// SubmitsCheckout: the customer asks to pay the {id} checkout, which is
	// recorded before any gate may refuse the request.
	SubmitsCheckout bool

	// Query is a zero value of the query's type, its parameters the type's
	// `form` fields (wireform), nil for none; Request is a zero value of the
	// JSON body's type (nil for none); Responses is every success outcome.
	Query     any
	Request   any
	Responses []Reply
	// Errors lists the codes the route's handler and the services it calls
	// name; the codes every route of its kind answers are in ErrorSets.
	Errors []billing.ErrorCode

	// Handler serves the route; Bind builds the handler from the assembly
	// when it needs more than the request. Exactly one is set.
	Handler router.Handler
	Bind    func(*Env) router.Handler
}

// QueryParams lists the route's query parameters, sorted by name.
func (r Route) QueryParams() ([]wireform.Param, error) {
	if r.Query == nil {
		return nil, nil
	}
	return wireform.Params(reflect.TypeOf(r.Query))
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
	return r.Staff() || r.Auth == AuthApplication || r.Auth == AuthSignedIn || r.Name == "GetPublicConfig"
}

// clientShape requires a client route's method to be declared whole, and
// leaves the client fields of any other route empty.
func (r Route) clientShape() error {
	if !r.ClientMethod() {
		if r.Doc != "" || r.Args != nil || r.Cache != "" || r.Client != "" {
			return fmt.Errorf("Doc, Args, Cache and Client are a client route's")
		}
		return nil
	}
	if !strings.HasPrefix(r.Doc, r.Name+" ") {
		return fmt.Errorf("a client route's Doc is a sentence beginning with its Name, %s", r.Name)
	}
	wildcards := strings.Count(r.Path, "{")
	if len(r.Args) != wildcards {
		return fmt.Errorf("Args holds %d values for %d path wildcards", len(r.Args), wildcards)
	}
	for _, arg := range r.Args {
		if _, ok := arg.(fmt.Stringer); !ok && reflect.TypeOf(arg).Kind() != reflect.String {
			return fmt.Errorf("Args: %T is neither a string nor a Stringer", arg)
		}
	}
	if r.Cache != "" && r.Method != GET {
		return fmt.Errorf("only a GET is cached")
	}
	switch r.Client {
	case "", HandWritten:
	default:
		return fmt.Errorf("unknown Client %q", r.Client)
	}
	stream := false
	if _, ok := r.Request.(Stream); ok {
		stream = true
	}
	for _, reply := range r.Responses {
		if _, ok := reply.Body.(Stream); ok {
			stream = true
		}
	}
	if stream && r.Client != HandWritten {
		return fmt.Errorf("a streamed body's client method is HandWritten")
	}
	return nil
}

// CatalogUpdate reports a catalog edit or document application.
func (r Route) CatalogUpdate() bool { return r.Group == CatalogAdmin && r.Level == LevelUpdate }

// h adapts a handler func to the neutral router.Handler type.
func h(fn func(r *httprequest.Request)) router.Handler { return router.Handler(fn) }

// Catalog is OpenRails' whole HTTP surface: every route any configuration
// can mount, in a stable order. It needs no database.
func Catalog() []Route { return allRoutes }

var allRoutes, index = func() ([]Route, map[string]Route) {
	var all []Route
	for _, resource := range [][]Route{
		metaRoutes, configRoutes, invoicesRoutes, cartsRoutes, catalogRoutes, subscriptionsRoutes, entitlementsRoutes, customersRoutes,
		creditsRoutes, meteringRoutes, limitsRoutes, paymentsRoutes, paymentMethodsRoutes, agreementsRoutes, pspsRoutes,
		merchantRoutes, messagesRoutes, opsRoutes, provisioningRoutes,
	} {
		all = append(all, resource...)
	}
	byKey := make(map[string]Route, len(all))
	for _, r := range all {
		if _, dup := byKey[r.Key()]; dup {
			panic("routes: declared twice: " + r.Key())
		}
		if err := r.validate(); err != nil {
			panic("routes: " + r.Key() + ": " + err.Error())
		}
		byKey[r.Key()] = r
	}
	return all, byKey
}()

// permissionFree are the tiers that check no permission, each by its own
// rule: a public route nothing, a provider callback its signature, a
// customer route the customer itself, the access read any signed-in caller.
// Every other tier admits only a caller holding the route's pair.
var permissionFree = []Tier{AuthPublic, AuthProvider, AuthCustomer, AuthSignedIn}

// authority refuses a route whose gate would admit a caller without a
// declared permission: a route of a tier that checks one names its pair, by
// its staff group and level or as its own Permission, and no route names one
// its tier never checks.
func (r Route) authority() error {
	switch r.Auth {
	case AuthPublic, AuthProvider, AuthCustomer, AuthSignedIn, AuthMerchant, AuthApplication, AuthProvisioning:
	default:
		return fmt.Errorf("unknown auth tier %q", r.Auth)
	}
	free := slices.Contains(permissionFree, r.Auth)
	switch needs := r.Needs(); {
	case !free && needs == (Pair{}):
		return fmt.Errorf("a %s route checks a permission: a merchant route's is its staff group's, an application or provisioning route names its own", r.Auth)
	case free && (needs != (Pair{}) || r.Permission != (Pair{})):
		return fmt.Errorf("a %s route checks no permission, so it cannot name one", r.Auth)
	}
	programmatic := r.Auth == AuthApplication || r.Auth == AuthProvisioning
	switch {
	case programmatic && r.Permission == (Pair{}):
		return fmt.Errorf("a programmatic route names its permission")
	case !programmatic && !r.Staff() && r.Permission != (Pair{}):
		return fmt.Errorf("only a staff or programmatic route names its own permission")
	case r.Sensitive && r.Auth != AuthCustomer:
		return fmt.Errorf("only a customer operation asks for a recent sign-in")
	case r.Sensitive && r.PayURL != "":
		return fmt.Errorf("a pay URL proves no recent sign-in, so a sensitive route is not served under one")
	case (r.Auth == AuthProvider) != (r.Group == Webhooks):
		return fmt.Errorf("a provider callback, and only one, is checked by its signature")
	}
	if read := r.Needs().reads(); r.Method != GET && !free && read != queryRoutes[r.Key()] {
		return fmt.Errorf("a route other than a GET writes, and needs a manage permission, unless queryRoutes lists it as a read")
	}
	return r.Needs().check()
}

// queryRoutes are the routes other than a GET that only read: previews,
// checks and queries whose body is the question. Every other route other than
// a GET writes.
var queryRoutes = map[string]bool{
	"POST /v1/admin/subscriptions/{id}/change/preview": true,
	"POST /v1/catalog/price-migrations/preview":        true,
	"POST /v1/merchant/psps/routing-preview":           true,
	"POST /v1/merchant/messages/preview":               true,
	"POST /v1/metrics/query":                           true,
	"POST /v1/app/entitlements/check":                  true,
}

// validate is every rule a catalog declaration keeps; the catalog refuses to
// load with a route that breaks one.
func (r Route) validate() error {
	if _, err := r.QueryParams(); err != nil {
		return err
	}
	if err := r.clientShape(); err != nil {
		return err
	}
	if (r.Handler == nil) == (r.Bind == nil) {
		return fmt.Errorf("needs exactly one of Handler and Bind")
	}
	leveled := r.Group == Admin || r.Group == CatalogAdmin || r.Group == MerchantConfig
	appRead := r.Group == App && r.Level == LevelRead && r.Method == POST
	if leveled != (r.Level == LevelRead || r.Level == LevelUpdate) && !appRead || !leveled && r.Level != "" && !appRead {
		return fmt.Errorf("an admin, catalog or merchant-config route reads or updates; a programmatic POST may say it reads")
	}
	if (r.Group == Access) != (r.Auth == AuthSignedIn) {
		return fmt.Errorf("the access route, and only it, takes any signed-in caller")
	}
	if (r.Group == App) != (r.Auth == AuthApplication || r.Auth == AuthProvisioning) {
		return fmt.Errorf("a programmatic route, and only one, takes an application or a provisioning token")
	}
	if r.Person && r.Auth != AuthMerchant {
		return fmt.Errorf("only a merchant route asks for a person")
	}
	if r.Group == App && !strings.HasPrefix(r.Path, "/v1/app/") {
		return fmt.Errorf("a programmatic route is under /v1/app")
	}
	if err := r.authority(); err != nil {
		return err
	}
	switch r.Merchant {
	case "":
		if r.Auth == AuthPublic && !r.NoConn {
			return fmt.Errorf("a public route names its merchant in the request, or reads no merchant's rows (NoConn)")
		}
	case MerchantInvoice:
		if r.Auth != AuthPublic || r.NoConn || !strings.Contains(r.Path, "{id}") {
			return fmt.Errorf("only a public route names its merchant, by the invoice its {id} names")
		}
	default:
		return fmt.Errorf("unknown merchant source %s", r.Merchant)
	}
	switch r.PayURL {
	case "":
	case PayURLPublic:
		if r.Auth != AuthPublic {
			return fmt.Errorf("a public pay URL route takes no credential")
		}
	case PayURLInvoice, PayURLSavedCards, PayURLMerchant:
		if r.Auth != AuthCustomer || (r.PayURL == PayURLInvoice) != strings.Contains(r.Path, "{id}") {
			return fmt.Errorf("a pay URL customer route is a customer route; an invoice route names the invoice as {id}")
		}
	default:
		return fmt.Errorf("unknown pay URL access %s", r.PayURL)
	}
	return nil
}

// requestShapeErrors are the codes any route with a JSON body or a query can
// answer before its handler runs.
var requestShapeErrors = []billing.ErrorCode{
	billing.CodeInvalidParam, billing.CodeInvalidQuery, billing.CodeInvalidRequestBody, billing.CodeRequestBodyTooLarge,
	billing.CodeUnknownField, billing.CodeUnsupportedMediaType,
}

// TierErrors lists the codes a tier answers before any handler runs.
func TierErrors(tier Tier) []billing.ErrorCode {
	common := []billing.ErrorCode{billing.CodeInternalError, billing.CodeRateLimitExceeded, billing.CodeCaptchaRequired, billing.CodeCaptchaInvalid, billing.CodeDatabaseBusy}
	// authenticated are the codes of asking who a request is: the host's
	// answer, or the in-process host's.
	authenticated := []billing.ErrorCode{
		billing.CodeAuthenticationRequired, billing.CodeCredentialExpired, billing.CodeCredentialRevoked,
		billing.CodePermissionRequired, billing.CodeAuthenticationUnavailable, billing.CodeAuthorizationUnavailable,
		billing.CodeMerchantUnresolved, billing.CodeMerchantMismatch, billing.CodeInvalidParam,
	}
	staff := []billing.ErrorCode{billing.CodeHostPrincipalInvalid}
	var own []billing.ErrorCode
	switch tier {
	case AuthCustomer:
		own = append([]billing.ErrorCode{billing.CodeInvokerScopedPrincipal, billing.CodeCustomerIssuerMismatch, billing.CodeCustomerTenantMismatch}, authenticated...)
	case AuthMerchant:
		own = append(staff, authenticated...)
	case AuthSignedIn:
		own = append(slices.Clone(staff), authenticated...)
	case AuthApplication, AuthProvisioning:
		own = append(append([]billing.ErrorCode{billing.CodeApplicationRequired}, staff...), authenticated...)
	default:
		// No credential: a public route names its own merchant, a provider's
		// callback its PSP.
		return sortedCodes(common)
	}
	return sortedCodes(append(common, own...))
}

func sortedCodes(codes []billing.ErrorCode) []billing.ErrorCode {
	out := slices.Clone(codes)
	slices.Sort(out)
	return slices.Compact(out)
}

// ErrorSets names the shared code sets a route answers besides its own
// Errors: its tier's, request-shape and event-store codes
// where they apply. ErrorSet lists each set's codes.
func (r Route) ErrorSets() []string {
	sets := []string{"tier:" + string(r.Auth)}
	if r.Request != nil || r.Query != nil {
		sets = append(sets, "request")
	}
	if r.EventStore {
		sets = append(sets, "event_store")
	}
	return sets
}

// ErrorSet lists the codes of a set ErrorSets names.
func ErrorSet(name string) []billing.ErrorCode {
	switch name {
	case "request":
		return requestShapeErrors
	case "event_store":
		return []billing.ErrorCode{billing.CodeEventStoreRequired}
	}
	if tier, ok := strings.CutPrefix(name, "tier:"); ok {
		return TierErrors(Tier(tier))
	}
	return nil
}
