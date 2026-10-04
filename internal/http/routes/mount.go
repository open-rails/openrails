package routes

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/catalogpolicy"
	"github.com/open-rails/openrails/internal/config"
	httphandlers "github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/merchant"
)

// Options is what an assembly supplies to mount catalog routes.
type Options struct {
	// Authenticator is the framework-neutral auth boundary behind AuthUser and
	// AuthOptional routes (issue #282/#285).
	Authenticator billingauth.Authenticator

	// Gate protects AuthMerchant routes. AuthKit/control-plane and embedded
	// host auth are adapters behind this one interface.
	Gate billingauth.Gate

	// ProviderRoutes controls provider-specific public routes. Nil preserves the
	// broad standalone surface; embedded single-merchant mounts pass an explicit
	// value derived from configured PSPs.
	ProviderRoutes *routesurface.ProviderRoutes

	// AdminLimiter is the #111 per-human-admin operation limiter. It runs after
	// Gate has resolved the effective principal, so counters key the authorized
	// user rather than an untrusted token claim or source IP.
	AdminLimiter *middleware.AdminOperationLimiter

	// InProcess marks the embedded Client's own handler: catalog mutations are
	// registered for its host principal whatever AllowCatalogUpdates says.
	InProcess bool

	// External are the handlers the assembly owns.
	External External
}

// External are the handlers an assembly owns: built from its configuration, or
// from packages the catalog must not import (the standalone control plane). A
// route bound to one is mounted where the assembly supplies it.
type External struct {
	// Meta: the process surface.
	Banner, Live, Ready, Metrics, Capabilities http.Handler
	// Captcha discovery, beside the checkout routes.
	CaptchaStatus, CaptchaScript http.Handler

	// The standalone control plane's merchant accounts, API keys and team.
	ListMerchants, CreateMerchant, RenameMerchant      router.Handler
	CreateAPIKey, ListAPIKeys, RevokeAPIKey            router.Handler
	ListTeam, ListTeamInvites, InviteTeamMember        router.Handler
	RevokeTeamInvite, ChangeTeamRole, RemoveTeamMember router.Handler
	MerchantCreationEnabled                            bool
	MerchantCreationLimit                              router.Middleware
}

// Env is one assembly mounting routes: its options and the gates built from
// them.
type Env struct {
	Options
	Runtime *app.Runtime
	// Customer authenticates AuthCustomer and AuthCustomerGrant routes.
	Customer router.Middleware
	// Root and Unlocker serve the platform tier.
	Root     RootPermissionChecker
	Unlocker AdminRateLimitUnlocker
	// providers is ProviderRoutes resolved.
	providers routesurface.ProviderRoutes
}

func newEnv(rt *app.Runtime, opts Options) *Env {
	env := &Env{Options: opts, Runtime: rt, providers: routesurface.AllProviderRoutes()}
	if opts.ProviderRoutes != nil {
		env.providers = *opts.ProviderRoutes
	}
	return env
}

// external binds a route to a handler the assembly owns.
func external(pick func(*External) http.Handler) func(*Env) router.Handler {
	return func(e *Env) router.Handler {
		handler := pick(&e.External)
		if handler == nil {
			return nil
		}
		return httprequest.FromHTTP(handler)
	}
}

// controlPlane binds a route to a control-plane handler the assembly owns.
func controlPlane(pick func(*External) router.Handler) func(*Env) router.Handler {
	return func(e *Env) router.Handler { return pick(&e.External) }
}

// gated binds a handler that asks the assembly's Gate a further question.
func gated(build func(billingauth.Gate) func(*httprequest.Request)) func(*Env) router.Handler {
	return func(e *Env) router.Handler { return router.Handler(build(e.Gate)) }
}

func (e *Env) config() *config.Config {
	if e.Runtime == nil {
		return nil
	}
	return e.Runtime.Config
}

// enabled reports whether the assembly's configuration mounts a feature.
func (e *Env) enabled(f Feature) bool {
	rt := e.Runtime
	switch f {
	case Always:
		return true
	case FeatureSolana:
		return e.providers.Solana
	case FeatureSolanaSigning:
		return e.providers.SolanaSigning
	case FeatureStripePortal:
		return e.providers.StripePortal
	case FeatureHostedCheckout:
		return HostedCheckoutPublished(rt)
	case FeatureMerchantDirectory:
		return rt != nil && rt.Merchants != nil
	case FeatureCatalogCopilot:
		return rt != nil && rt.CopilotService.Configured()
	case FeatureMetricsAsk:
		return rt != nil && rt.DashboardService.AskConfigured()
	case FeatureDashboardGeneration:
		return rt != nil && rt.DashboardService.NLConfigured()
	case FeatureMerchantCreation:
		return e.External.MerchantCreationEnabled
	}
	panic("routes: unknown feature " + string(f))
}

// HostedCheckoutPublished reports whether this runtime serves hosted checkout
// sessions: the standalone surface always, an embedded host with
// Config.HTTP.Checkout.
func HostedCheckoutPublished(rt *app.Runtime) bool {
	if rt == nil || rt.Config == nil {
		return true
	}
	cfg := rt.Config
	return cfg.ControlPlane != nil || cfg.HTTP == nil || cfg.HTTP.Checkout != nil
}

// mount registers the selected catalog routes on rr, which is rooted at base.
// A route is mounted when its feature is configured, its handler is supplied
// and, for a catalog write, the deployment allows it.
func (e *Env) mount(rr router.Router, base string, selected func(Route) bool) {
	for _, route := range Catalog() {
		if !selected(route) || !e.enabled(route.When) {
			continue
		}
		if route.CatalogWrite && !e.InProcess && !catalogpolicy.Enabled(e.config()) {
			continue
		}
		handler := route.Handler
		if route.Bind != nil {
			handler = route.Bind(e)
		}
		if handler == nil {
			continue
		}
		path, ok := strings.CutPrefix(route.Path, base)
		if !ok {
			panic("routes: " + route.Key() + " is not under " + base)
		}
		if route.Path == "/" {
			// ServeMux: the exact root, not every unmatched path.
			path = "/{$}"
		}
		rr.Handle(route.Method, path, handler, e.gates(route)...)
	}
}

// gates builds the middleware a route's declaration asks for, outermost
// first.
func (e *Env) gates(route Route) []router.Middleware {
	var conn []router.Middleware
	if e.Runtime != nil && e.Runtime.DB != nil && !route.NoConn {
		// Pin a merchant-scoped DB connection for the request so
		// merchant-owned queries share one merchant session (#227).
		conn = append(conn, middleware.MerchantDBConnMW(e.Runtime.DB))
	}
	var mw []router.Middleware
	switch route.Auth {
	case AuthPublic, AuthSessionID, AuthProvider:
		mw = conn
	case AuthOptional:
		mw = append(conn, e.optionalMW())
	case AuthUser:
		mw = append(conn, e.requiredMW())
	case AuthCustomer:
		// or#930: an invoker-scoped principal spends the payer's money without
		// being the payer; only a route that says so admits it.
		mw = append([]router.Middleware{e.Customer}, conn...)
		if !route.InvokerScoped {
			mw = append(mw, middleware.PayerScopedRequired())
		}
	case AuthCustomerGrant:
		mw = append([]router.Middleware{e.Customer, middleware.PayerScopedRequired(), middleware.CustomerScopeRequired()}, conn...)
		mw = append(mw, middleware.RequirePermission(route.Perm))
	case AuthMerchant:
		if route.CatalogWrite {
			mw = append(mw, catalogWriteGuardMW(e.config()))
		}
		mw = append(mw, e.merchantPermissionMW(route.Perm))
		if route.Also != "" {
			mw = append(mw, e.merchantPermissionMW(route.Also))
		}
		// The authorization gate stays outermost; the user-keyed operation
		// limiter runs before any merchant DB connection is pinned.
		if e.AdminLimiter != nil && route.Limit != "" {
			mw = append(mw, e.AdminLimiter.AdminRateLimitMW(route.Limit))
		}
		mw = append(mw, conn...)
		if route.Group == CatalogOwned {
			mw = append(mw, ownerCatalogScopeMW(e.Runtime, e.Gate))
		}
	case AuthOperator:
		mw = []router.Middleware{e.platformPermissionMW(route.Perm)}
	default:
		panic("routes: " + route.Key() + " declares no auth tier")
	}
	switch route.Throttle {
	case ThrottleSessionRead:
		mw = append(mw, middleware.CheckoutSessionRateLimit(e.Runtime, "checkout-session-read", middleware.CheckoutSessionReadsPerMinute))
	case ThrottleSessionPay:
		mw = append(mw, middleware.CheckoutSessionRateLimit(e.Runtime, "checkout-session-pay", middleware.CheckoutSessionPaysPerMinute))
	case ThrottleMerchantCreation:
		if e.External.MerchantCreationLimit != nil {
			mw = append(mw, e.External.MerchantCreationLimit)
		}
	}
	if checked := checkedParams(route.Query); len(checked) > 0 {
		mw = append(mw, strictQueryMW(checked))
	}
	return mw
}

func checkedParams(params []Param) []string {
	var out []string
	for _, p := range params {
		if p.Checked {
			out = append(out, p.Name)
		}
	}
	return out
}

// strictQueryMW refuses a declared integer query parameter that is not a
// non-negative integer, so a handler never reads a malformed one as its
// default.
func strictQueryMW(names []string) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			for _, name := range names {
				raw := strings.TrimSpace(r.Query(name))
				if raw == "" {
					continue
				}
				if n, err := strconv.Atoi(raw); err != nil || n < 0 {
					r.AbortAPIError(api.Coded(billing.CodeInvalidQuery, name+" is invalid").WithParam(name))
					return
				}
			}
			next(r)
		}
	}
}

// requiredMW authenticates via the assembly's Authenticator, aborts 401 on
// failure, and pins the resulting UserContext on the request.
func (opts Options) requiredMW() router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			a := opts.Authenticator
			if a == nil {
				r.AbortCode(billing.CodeInternalError, "authentication disabled")
				return
			}
			uc, err := a.Authenticate(r.Request.Context(), r.Request)
			if err != nil {
				r.AbortGate(billingauth.Unauthenticated(err))
				return
			}
			if verr := uc.ValidateSubject(); verr != nil {
				r.AbortCode(billing.CodeAuthenticationRequired, verr.Error())
				return
			}
			r.SetUserContext(uc)
			next(r)
		}
	}
}

// optionalMW attempts authentication and pins the UserContext when it
// succeeds, but never aborts.
func (opts Options) optionalMW() router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			if a := opts.Authenticator; a != nil {
				if uc, err := a.Authenticate(r.Request.Context(), r.Request); err == nil && uc.ValidateSubject() == nil {
					r.SetUserContext(uc)
				}
			}
			next(r)
		}
	}
}

// merchantPermissionMW asks the Gate for perm on the request's merchant, then
// pins the merchant and the principal.
func (opts Options) merchantPermissionMW(perm string) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			if opts.Gate == nil {
				r.AbortCode(billing.CodeInternalError, "authorization unavailable")
				return
			}
			principal, err := opts.Gate.Authorize(r.Request.Context(), r.Request, perm)
			if err != nil {
				r.AbortGate(err)
				return
			}
			if !middleware.EnforceMerchantBinding(r, principal.MerchantID) {
				return
			}
			// Per operation, so every route serving it asks for the same
			// recent sign-in.
			if billing.RequiresRecentSignIn(perm) {
				if err := opts.Gate.RequireRecentSignIn(r.Request.Context(), r.Request, principal); err != nil {
					r.AbortGate(err)
					return
				}
			}
			if r.Request != nil && !principal.MerchantID.IsZero() {
				r.Request = r.Request.WithContext(merchant.WithID(r.Request.Context(), principal.MerchantID))
			}
			if !principal.MerchantID.IsZero() {
				r.Set("openrails.merchant_id", principal.MerchantID)
			}
			if principal.UserContext.UserID != "" {
				r.SetUserContext(principal.UserContext)
			}
			// The full gate-resolved principal (existing consumers only check
			// presence; #757 api-key handlers read Permissions for no-escalation).
			r.Set(httphandlers.MerchantRoutePrincipalContextKey, principal)
			next(r)
		}
	}
}

// RequireUser requires an authenticated user session.
func (opts Options) RequireUser() router.Middleware { return opts.requiredMW() }

// RequireMerchantPermission applies the same billing merchant gate to host routes.
func (opts Options) RequireMerchantPermission(permission string) router.Middleware {
	return opts.merchantPermissionMW(permission)
}

func under(prefix string) func(Route) bool {
	return func(r Route) bool { return r.Path == prefix || strings.HasPrefix(r.Path, prefix+"/") }
}

func in(group Group, also ...func(Route) bool) func(Route) bool {
	return func(r Route) bool {
		if r.Group != group {
			return false
		}
		for _, cond := range also {
			if !cond(r) {
				return false
			}
		}
		return true
	}
}

// The registrations below mount one group of the catalog on a router rooted
// at that group's path. They decide which routes a deployment publishes; what
// each route is comes from the catalog.

// RegisterMetaRoutes mounts the process surface the assembly supplies
// handlers for, on a router rooted at the mount's root.
func RegisterMetaRoutes(rr router.Router, opts Options) {
	newEnv(nil, opts).mount(rr, "", in(Meta))
}

// RegisterUserRoutes mounts the buyer-facing surface, on a router rooted at
// /v1.
func RegisterUserRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	newEnv(rt, opts).mount(rr, "/v1", in(Checkout))
}

// RegisterMerchantArchiveRoutes mounts the complete portable billing archive
// surface, shared by the server and database-only CLI runtime.
func RegisterMerchantArchiveRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	newEnv(rt, opts).mount(rr, "/v1/merchant", in(MerchantAPI, under("/v1/merchant/billing-archive")))
}

// RegisterServiceRoutes mounts the merchant billing surface. Access is gated by
// merchant permissions, not credential type (#564).
func RegisterServiceRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	newEnv(rt, opts).mount(rr, "/v1/merchant", in(MerchantAPI))
}

// RegisterMerchantActionRoutes mounts merchant staff's support and money
// operations.
func RegisterMerchantActionRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	if opts.AdminLimiter == nil && rt != nil {
		opts.AdminLimiter = middleware.NewAdminOperationLimiter(rt.RedisClient)
	}
	newEnv(rt, opts).mount(rr, "/v1/merchant", in(MerchantAdmin, under("/v1/merchant")))
}

// RegisterCatalogRoutes mounts merchant catalog administration.
func RegisterCatalogRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	newEnv(rt, opts).mount(rr, "/v1/merchant/catalog", in(CatalogAdmin, under("/v1/merchant/catalog")))
}

// RegisterCatalogCollectionRoutes is the separately authorized merchant-admin
// collection. Supplying an owner subject here is permitted only by that grant.
func RegisterCatalogCollectionRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	newEnv(rt, opts).mount(rr, "/v1/merchant/catalogs", in(CatalogAdmin, under("/v1/merchant/catalogs")))
}

// RegisterOwnedCatalogRoutes exposes only creator product/price operations.
// Provider configuration, entitlement grants, meters and batch application retain
// their merchant-administrator surfaces and are never mounted in this group.
func RegisterOwnedCatalogRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	newEnv(rt, opts).mount(rr, "/v1/catalog", in(CatalogOwned))
}

// RegisterImportRoutes mounts the #737 DeclaredBilling import door
// (POST <prefix>/billing).
func RegisterImportRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	newEnv(rt, opts).mount(rr, "/v1/import", in(MerchantAdmin, under("/v1/import")))
}

// RegisterMerchantConfigRoutes is shared by the private Client transport and
// explicitly selected external management surface. Route publication grants no
// authority and does not change the selected credential backend's capabilities.
func RegisterMerchantConfigRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	newEnv(rt, opts).mount(rr, "/v1/merchant", in(MerchantConfig))
}

// RegisterControlPlaneRoutes mounts the standalone control plane's merchant
// accounts, API keys and team, on a router rooted at /v1.
func RegisterControlPlaneRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	newEnv(rt, opts).mount(rr, "/v1", in(ControlPlane))
}

// RegisterWebhookRoutes mounts the canonical callback surface under /webhooks.
// The configured provider identity resolves its merchant in the runtime
// environment; runtime bindings and signatures remain mandatory.
func RegisterWebhookRoutes(rr router.Router, rt *app.Runtime) {
	newEnv(rt, Options{}).mount(rr, "/v1/webhooks", in(Webhooks))
}

// SelfRoutePrefix is the canonical browser self-service billing surface. The
// credential profile may be a delegated JWT in standalone mode or a host/user
// bearer in embedded mode; the URL is intentionally one stable `/me` surface
// and the credential profile lives on the resolved Principal.
const SelfRoutePrefix = "/me"

// CustomerRoutePrefix is the canonical customer-treasury surface (#567): a
// customer (any payer) acting over its OWN co-managed/shared balance, addressed
// by the customer's id — not merchant/seller administration.
const CustomerRoutePrefix = "/customers"

var scopeRank = map[CustomerScope]int{ScopeSelfService: 0, ScopeBillingManagement: 1, ScopeSubscriptionManagement: 2}

// serves reports whether an exposure of scope serves a Customer route.
func serves(scope CustomerScope) func(Route) bool {
	return func(r Route) bool { return scopeRank[scope] <= scopeRank[r.Scope] }
}

func customerEnv(rt *app.Runtime, customer router.Middleware, providers routesurface.ProviderRoutes) *Env {
	env := newEnv(rt, Options{ProviderRoutes: &providers})
	env.Customer = customer
	return env
}

// RegisterSelfServiceRoutes mounts the whole customer surface, authenticated
// by delegatedMW. Every operation is scoped to the authenticated end-user and
// their merchant: no path names a user.
func RegisterSelfServiceRoutes(rr router.Router, rt *app.Runtime, delegatedMW router.Middleware, providerRoutes routesurface.ProviderRoutes) {
	customerEnv(rt, delegatedMW, providerRoutes).mount(rr, "/v1/me", in(Customer, serves(ScopeSelfService)))
}

// RegisterCustomerBillingManagementRoutes exposes the customer's existing
// billing history, access, payment methods and agreement management. Creating a
// checkout or selecting a different product/price remains with the host.
func RegisterCustomerBillingManagementRoutes(rr router.Router, rt *app.Runtime, delegatedMW router.Middleware, providerRoutes routesurface.ProviderRoutes) {
	customerEnv(rt, delegatedMW, providerRoutes).mount(rr, "/v1/me", in(Customer, serves(ScopeBillingManagement)))
}

// RegisterCustomerSubscriptionManagementRoutes exposes only customer-owned
// cancellation, resumption and payment-method selection.
func RegisterCustomerSubscriptionManagementRoutes(rr router.Router, rt *app.Runtime, delegatedMW router.Middleware) {
	customerEnv(rt, delegatedMW, routesurface.ProviderRoutes{}).mount(rr, "/v1/me", in(Customer, serves(ScopeSubscriptionManagement)))
}

// RegisterCustomerTreasuryRoutes mounts the customer-as-PAYER treasury surface
// (#567), deliberately separate from `/me` (the caller's OWN balance) and
// `/merchant` (seller operations). Handlers are shared with `/v1/me/*`:
// CustomerScopeRequired confirms the {customer_id} scope and rebinds the
// acting payer. Every route is gated by a `customer:*` permission because the
// balance may be a shared resource.
func RegisterCustomerTreasuryRoutes(rr router.Router, rt *app.Runtime, delegatedMW router.Middleware, providerRoutes routesurface.ProviderRoutes) {
	customerEnv(rt, delegatedMW, providerRoutes).mount(rr, "/v1/customers", in(Treasury))
}
