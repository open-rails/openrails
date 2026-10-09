package routes

import (
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/credential"
	httphandlers "github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routesurface"
)

// Options is what an assembly supplies to mount catalog routes.
type Options struct {
	// Auth is the host's middleware for the merchant tier and, on checkout
	// sessions, who presents one.
	Auth billingauth.Auth
	// AuthBindsMerchant: Auth's own middleware resolves the merchant a
	// request acts on (the standalone server's, the in-process host's).
	// Otherwise a merchant route acts on the configured merchant.
	AuthBindsMerchant bool

	// Authenticator and ResourceUsers authenticate the standalone control
	// plane's own user routes (AuthUser).
	Authenticator billingauth.Authenticator
	ResourceUsers ResourceUserResolver

	// ProviderRoutes controls provider-specific public routes. Nil preserves the
	// broad standalone surface; embedded single-merchant mounts pass an explicit
	// value derived from configured PSPs.
	ProviderRoutes *routesurface.ProviderRoutes

	// AdminLimiter is the #111 per-human-admin operation limiter. It runs after
	// the staff gate, so counters key the authorized actor rather than an
	// untrusted token claim or source IP.
	AdminLimiter *middleware.AdminOperationLimiter

	// CatalogWrites mounts the catalog-write routes. Their guard still admits
	// only the process owner unless the merchant API's mount published catalog
	// edits (app.Runtime.CatalogEdits).
	CatalogWrites bool

	// External are the handlers the assembly owns.
	External External
}

// External are the handlers an assembly owns: built from its configuration, or
// from packages the catalog must not import (the standalone control plane). A
// route bound to one is mounted where the assembly supplies it.
type External struct {
	// Meta: the process surface.
	Live, Ready, Metrics, Capabilities http.Handler
	// Captcha discovery, beside the checkout routes.
	CaptchaStatus, CaptchaScript http.Handler

	// The standalone control plane's merchant accounts, API keys and team.
	ListMerchants, CreateMerchant, RenameMerchant      router.Handler
	CreateAPIKey, ListAPIKeys, RevokeAPIKey            router.Handler
	ListTeam, ListTeamInvites, InviteTeamMember        router.Handler
	RevokeTeamInvite, ChangeTeamRole, RemoveTeamMember router.Handler
	// Federated grants (#1140): the merchant's, and the signed-in user's own.
	ListFederatedGrants, CreateFederatedGrant, RevokeFederatedGrant router.Handler
	ListMyFederatedGrants, AcceptFederatedGrant                     router.Handler
	MerchantCreationEnabled                                         bool
	MerchantCreationLimit                                           router.Middleware
}

// Env is one assembly mounting routes: its options and the gates built from
// them.
type Env struct {
	Options
	Runtime *app.Runtime
	// Customers gates customer routes at CustomerMerchant, else at the
	// merchant each request selects (SelectedMerchant), else at the
	// configured one (unless AuthBindsMerchant).
	Customers        billingauth.Auth
	CustomerMerchant billingauth.Target
	SelectedMerchant bool
	// Viewers says who presents a checkout session.
	Viewers billingauth.Auth
	// permissions caches Auth.RequirePermission by permission for staffCan.
	permissions *sync.Map
	// Root and Unlocker serve the platform tier.
	Root     RootPermissionChecker
	Unlocker AdminRateLimitUnlocker
	// providers is ProviderRoutes resolved.
	providers routesurface.ProviderRoutes
}

func newEnv(rt *app.Runtime, opts Options) *Env {
	env := &Env{Options: opts, Runtime: rt, Viewers: opts.Auth, providers: routesurface.AllProviderRoutes(), permissions: &sync.Map{}}
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

// gated binds a handler that asks the route's staff gate a further
// permission.
func gated(build func(httphandlers.StaffCan) func(*httprequest.Request)) func(*Env) router.Handler {
	return func(e *Env) router.Handler { return router.Handler(build(e.staffCan)) }
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

// mount registers the selected catalog routes on rr, which is rooted at base.
// A route is mounted when its feature is configured, its handler is supplied
// and, for a catalog write, the assembly mounts catalog writes.
func (e *Env) mount(rr router.Router, base string, selected func(Route) bool) {
	for _, route := range Catalog() {
		if !selected(route) || !e.enabled(route.When) {
			continue
		}
		if route.CatalogWrite && !e.CatalogWrites {
			continue
		}
		handler := e.Guarded(route)
		if handler == nil {
			continue
		}
		path, ok := strings.CutPrefix(route.Path, base)
		if !ok {
			panic("routes: " + route.Key() + " is not under " + base)
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
	case AuthCheckoutSession:
		mw = append([]router.Middleware{middleware.CheckoutSessionMerchant(e.Runtime), e.checkoutViewer(route)}, conn...)
	case AuthUser:
		mw = append(conn, e.requiredMW())
	case AuthCustomer:
		mw = append(e.customerGates(route), conn...)
	case AuthMerchant:
		if route.CatalogWrite {
			mw = append(mw, catalogWriteGuardMW(e.Runtime))
		}
		mw = append(mw, e.staffGates(route)...)
		// The staff gate stays outermost; the actor-keyed operation limiter
		// runs before any merchant DB connection is pinned.
		if e.AdminLimiter != nil && route.Limit != "" {
			mw = append(mw, e.AdminLimiter.AdminRateLimitMW(route.Limit))
		}
		mw = append(mw, conn...)
	case AuthOperator:
		mw = []router.Middleware{e.platformPermissionMW(route.Perm)}
	default:
		panic(MountError{Route: route.Key(), Reason: "declares no auth tier"})
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

// requiredMW authenticates a standalone control-plane user for its own
// routes, aborts 401 on failure, and pins the resulting UserContext.
func (opts Options) requiredMW() router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			if opts.ResourceUsers != nil && credential.LooksLikeResourceToken(authorizationToken(r.Request.Header.Get("Authorization"))) {
				user, err := opts.ResourceUsers.ResolveResourceUser(r.Request)
				if err != nil {
					r.AbortGate(credential.ResourceTokenRefusal(err))
					return
				}
				r.Set(middleware.ResourceUserContextKey, user)
				next(r)
				return
			}
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

// RegisterMerchantRoutes mounts the merchant API, each route gated by its
// merchant permission, on a router rooted at /v1.
func RegisterMerchantRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	if opts.AdminLimiter == nil && rt != nil {
		opts.AdminLimiter = middleware.NewAdminOperationLimiter(rt.RedisClient)
	}
	newEnv(rt, opts).mount(rr, "/v1", in(Merchant))
}

// RegisterMerchantRoutesUnder mounts the merchant routes under prefix, on a
// router rooted at /v1: the CLI's database-only runtimes serve one resource.
func RegisterMerchantRoutesUnder(rr router.Router, rt *app.Runtime, opts Options, prefix string) {
	newEnv(rt, opts).mount(rr, "/v1", in(Merchant, under(prefix)))
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

// SelfRoutePrefix is the customer surface's path: one stable /me, whatever
// credential the mount's Auth accepts.
const SelfRoutePrefix = "/me"

var scopeRank = map[CustomerScope]int{ScopeSelfService: 0, ScopeBillingManagement: 1, ScopeSubscriptionManagement: 2}

// serves reports whether an exposure of scope serves a Customer route.
func serves(scope CustomerScope) func(Route) bool {
	return func(r Route) bool { return scopeRank[scope] <= scopeRank[r.Scope] }
}

// CustomerMount is one customer surface: the Auth that admits its
// customers and the merchant they buy from. Without a Merchant, a server's
// surface (SelectedMerchant) serves the merchant each request selects, and
// otherwise the Auth binds the merchant itself or the configured one serves.
type CustomerMount struct {
	Auth              billingauth.Auth
	AuthBindsMerchant bool
	Merchant          billingauth.Target
	SelectedMerchant  bool
	Providers         routesurface.ProviderRoutes
}

func customerEnv(rt *app.Runtime, m CustomerMount) *Env {
	env := newEnv(rt, Options{ProviderRoutes: &m.Providers, AuthBindsMerchant: m.AuthBindsMerchant})
	env.Customers, env.CustomerMerchant, env.SelectedMerchant = m.Auth, m.Merchant, m.SelectedMerchant
	return env
}

// RegisterSelfServiceRoutes mounts the whole customer surface. Every
// operation is scoped to the verified customer and their merchant: no path
// names a customer.
func RegisterSelfServiceRoutes(rr router.Router, rt *app.Runtime, m CustomerMount) {
	customerEnv(rt, m).mount(rr, "/v1/me", in(Customer, serves(ScopeSelfService)))
}

// RegisterCustomerBillingManagementRoutes exposes the customer's existing
// billing history, access, payment methods and agreement management. Creating a
// checkout or selecting a different product/price remains with the host.
func RegisterCustomerBillingManagementRoutes(rr router.Router, rt *app.Runtime, m CustomerMount) {
	customerEnv(rt, m).mount(rr, "/v1/me", in(Customer, serves(ScopeBillingManagement)))
}

// RegisterCustomerSubscriptionManagementRoutes exposes only customer-owned
// cancellation, resumption and payment-method selection.
func RegisterCustomerSubscriptionManagementRoutes(rr router.Router, rt *app.Runtime, m CustomerMount) {
	m.Providers = routesurface.ProviderRoutes{}
	customerEnv(rt, m).mount(rr, "/v1/me", in(Customer, serves(ScopeSubscriptionManagement)))
}
