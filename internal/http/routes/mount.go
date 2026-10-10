package routes

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	httphandlers "github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/scim"
)

// Options is what an assembly supplies to mount catalog routes.
type Options struct {
	// Auth is the mount's Authenticator: who a request on a staff or
	// programmatic route is and, on checkout sessions, who presents one.
	Auth billingauth.Authenticator
	// Scope is where a merchant's staff hold Permissions; every staff route
	// needs it.
	Scope ScopeFunc
	// ResolveMerchant finds the merchant an authenticated request acts on
	// when its credential names none (the standalone server's sessions);
	// CredentialOnly finds none. Nil serves the configured merchant, pinned
	// before the request is authenticated.
	ResolveMerchant MerchantResolver

	// ProviderRoutes controls provider-specific public routes. Nil preserves the
	// broad standalone surface; embedded single-merchant mounts pass an explicit
	// value derived from configured PSPs.
	ProviderRoutes *routesurface.ProviderRoutes

	// AdminLimiter is the #111 per-human-admin operation limiter. It runs after
	// the staff gate, so counters key the authorized actor rather than an
	// untrusted token claim or source IP.
	AdminLimiter *middleware.AdminOperationLimiter

	// Permissions are what a caller holds in Scope for each staff route; a
	// staff route without one is not mounted.
	Permissions Permissions

	// Capabilities is what the assembly mounts, as GET /v1/config reports
	// it; without it the public read is not mounted.
	Capabilities *billing.Capabilities

	// SCIMMerchant serves the SCIM routes for this merchant without a
	// credential: a directory in the same process holding the engine.
	SCIMMerchant billing.MerchantID

	// External are the handlers the assembly owns.
	External External
}

// External are the handlers an assembly owns, built from its configuration. A
// route bound to one is mounted where the assembly supplies it.
type External struct {
	// Meta: the process surface.
	Live, Ready http.Handler
	// Captcha discovery, beside the checkout routes.
	CaptchaStatus, CaptchaScript http.Handler
}

// Env is one assembly mounting routes: its options and the gates built from
// them.
type Env struct {
	Options
	Runtime *app.Runtime
	// Customers gates customer routes at CustomerMerchant, else at the
	// merchant each request selects (SelectedMerchant), else at the
	// configured one (unless ResolveMerchant).
	Customers        billingauth.Authenticator
	CustomerMerchant billingauth.Target
	SelectedMerchant bool
	// Viewers says who presents a checkout session.
	Viewers billingauth.Authenticator
	// providers is ProviderRoutes resolved.
	providers routesurface.ProviderRoutes
	// scim is the assembly's SCIM server, built once.
	scim *scim.Server
}

// scimServer is the assembly's SCIM server, acting for the merchant its
// gate admitted.
func (e *Env) scimServer() *scim.Server {
	if e.Runtime == nil {
		return nil
	}
	if e.scim == nil {
		rt := e.Runtime
		e.scim = &scim.Server{DB: rt.DB, Authenticate: e.scimMerchant, Now: func() time.Time {
			if rt.Clock != nil {
				return rt.Clock.Now()
			}
			return time.Now()
		}}
	}
	return e.scim
}

func newEnv(rt *app.Runtime, opts Options) *Env {
	env := &Env{Options: opts, Runtime: rt, Viewers: opts.Auth, providers: routesurface.AllProviderRoutes()}
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

// gated binds a handler that asks whether its caller would pass another
// route's permission.
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
	}
	panic("routes: unknown feature " + string(f))
}

// mounts reports whether the assembly mounts a selected route: its feature
// is configured and its handler supplied.
func (e *Env) mounts(route Route) bool {
	return e.enabled(route.When) && e.Guarded(route) != nil
}

// mount registers the selected catalog routes on rr, which is rooted at base.
func (e *Env) mount(rr router.Router, base string, selected func(Route) bool) {
	for _, route := range Catalog() {
		if !selected(route) || !e.mounts(route) {
			continue
		}
		handler := e.Guarded(route)
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
	case AuthProvisioning:
		if e.SCIMMerchant.IsZero() {
			mw = []router.Middleware{e.provisioningGate(route)}
		}
	case AuthApplication:
		mw = append(e.appGates(route), conn...)
		if route.AppWrite() {
			mw = append(mw, e.appRequestReplay(route))
		}
	case AuthCheckoutSession:
		mw = append([]router.Middleware{middleware.CheckoutSessionMerchant(e.Runtime), e.checkoutViewer(route)}, conn...)
	case AuthCustomer:
		mw = append(e.customerGates(route), conn...)
	case AuthSignedIn:
		mw = append(e.signedInGates(route), conn...)
	case AuthMerchant:
		mw = append(mw, e.staffGates(route)...)
		// The staff gate stays outermost; the actor-keyed operation limiter
		// runs before any merchant DB connection is pinned.
		if e.AdminLimiter != nil && route.Limit != "" {
			mw = append(mw, e.AdminLimiter.AdminRateLimitMW(route.Limit))
		}
		mw = append(mw, conn...)
	default:
		panic(MountError{Route: route.Key(), Reason: "declares no auth tier"})
	}
	switch route.Throttle {
	case ThrottleSessionRead:
		mw = append(mw, middleware.CheckoutSessionRateLimit(e.Runtime, "checkout-session-read", middleware.CheckoutSessionReadsPerMinute))
	case ThrottleSessionPay:
		mw = append(mw, middleware.CheckoutSessionRateLimit(e.Runtime, "checkout-session-pay", middleware.CheckoutSessionPaysPerMinute))
	}
	if checked := checkedParams(route.Query); len(checked) > 0 {
		mw = append(mw, strictQueryMW(checked))
	}
	if slices.Contains(route.Query, idsParam) {
		mw = append(mw, idsMW)
	}
	return mw
}

// idsMW bounds a list's ids filter and refuses any other parameter beside it,
// so every list reads named records the same way.
func idsMW(next router.Handler) router.Handler {
	return func(r *httprequest.Request) {
		query := r.Request.URL.Query()
		raw, named := query["ids"]
		if !named {
			next(r)
			return
		}
		refuse := func(message string) {
			r.AbortAPIError(api.Coded(billing.CodeInvalidQuery, message).WithParam("ids"))
		}
		if len(raw) != 1 {
			refuse("ids is one comma-separated list")
			return
		}
		ids := strings.Split(raw[0], ",")
		if len(ids) > billing.MaxBatchItems || strings.TrimSpace(raw[0]) == "" {
			refuse(fmt.Sprintf("ids must hold 1 to %d ids", billing.MaxBatchItems))
			return
		}
		for name := range query {
			if name != "ids" {
				refuse("ids takes no other parameter: " + name)
				return
			}
		}
		next(r)
	}
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

// RegisterStaffRoutes mounts the staff groups opts.Permissions gives a
// permission, each route behind it, and with any of them the access read, on
// a router rooted at /v1.
func RegisterStaffRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	if opts.AdminLimiter == nil && rt != nil {
		opts.AdminLimiter = middleware.NewAdminOperationLimiter(rt.RedisClient, rt.RateWindows)
	}
	any := opts.Permissions != (Permissions{})
	newEnv(rt, opts).mount(rr, "/v1", func(r Route) bool { return opts.Permissions.mounts(r) || any && r.Group == Access })
}

// RegisterStaffRoutesUnder mounts the staff routes under prefix, on a router
// rooted at /v1: the CLI's database-only runtimes serve one resource.
func RegisterStaffRoutesUnder(rr router.Router, rt *app.Runtime, opts Options, prefix string) {
	newEnv(rt, opts).mount(rr, "/v1", func(r Route) bool { return opts.Permissions.mounts(r) && under(prefix)(r) })
}

// mounts reports a staff route its group's permission mounts.
func (p Permissions) mounts(r Route) bool { return r.Staff() && p.For(r) != "" }

// RegisterPublicClientRoutes mounts the public routes the Go Client calls
// (ClientMethod), on a router rooted at /v1: the in-process transport serves
// them beside the staff routes.
func RegisterPublicClientRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	newEnv(rt, opts).mount(rr, "/v1", func(r Route) bool { return r.Auth == AuthPublic && r.ClientMethod() })
}

// RegisterWebhookRoutes mounts the canonical callback surface under /webhooks.
// The configured provider identity resolves its merchant in the runtime
// environment; runtime bindings and signatures remain mandatory.
func RegisterWebhookRoutes(rr router.Router, rt *app.Runtime) {
	newEnv(rt, Options{}).mount(rr, "/v1/webhooks", in(Webhooks))
}

// RegisterAppRoutes mounts the programmatic routes (Routes.Programmatic) on
// a router rooted at /v1: SCIM provisioning too, unless the runtime reads the
// host's user directory instead of keeping a pushed copy.
func RegisterAppRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	scim := rt != nil && !rt.HostUserInfo
	newEnv(rt, opts).mount(rr, "/v1", in(App, func(r Route) bool { return r.Auth == AuthApplication || scim }))
}

// RegisterSCIMRoutes mounts the SCIM routes for merchant, without a
// credential, on a router rooted at the SCIM root: a directory in the same
// process pushes to them.
func RegisterSCIMRoutes(rr router.Router, rt *app.Runtime, merchant billing.MerchantID) {
	newEnv(rt, Options{SCIMMerchant: merchant}).mount(rr, SCIMRoot, in(App, func(r Route) bool { return r.Auth == AuthProvisioning }))
}

// SCIMRoot is where the SCIM routes are, beneath the API root.
const SCIMRoot = "/v1/app/scim/v2"

// SelfRoutePrefix is the customer surface's path: one stable /me, whatever
// credential the mount's Auth accepts.
const SelfRoutePrefix = "/me"

// CustomerMount is one customer surface: the Authenticator that says who
// its customers are and the merchant they buy from. Without a Merchant, a
// server's surface (SelectedMerchant) serves the merchant each request
// selects; with ResolveMerchant, the merchant the credential names;
// otherwise the configured one.
type CustomerMount struct {
	Auth             billingauth.Authenticator
	ResolveMerchant  MerchantResolver
	Merchant         billingauth.Target
	SelectedMerchant bool
	Providers        routesurface.ProviderRoutes
}

func customerEnv(rt *app.Runtime, m CustomerMount) *Env {
	env := newEnv(rt, Options{ProviderRoutes: &m.Providers, ResolveMerchant: m.ResolveMerchant})
	env.Customers, env.CustomerMerchant, env.SelectedMerchant = m.Auth, m.Merchant, m.SelectedMerchant
	return env
}

// RegisterCustomerRoutes mounts the customer surface. Every operation is
// scoped to the verified customer and their merchant: no path names a
// customer.
func RegisterCustomerRoutes(rr router.Router, rt *app.Runtime, m CustomerMount) {
	customerEnv(rt, m).mount(rr, "/v1/me", in(Customer))
}
