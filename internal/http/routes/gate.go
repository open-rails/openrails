package routes

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/customerscope"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchanttarget"
)

// The route gate stacks the host's Auth middleware on each route by the
// route's catalog tier, then reads the identity it admitted. The middleware
// answers its own refusals; OpenRails refuses a request admitted without an
// identity or an invoker, a subject a route does not serve, and one at no
// merchant, and binds the identity in its own context, where handlers
// re-check it.

// MountError refuses a route the mount cannot gate. Mounting stops; a route
// is never mounted open.
type MountError struct{ Route, Reason string }

func (e MountError) Error() string { return "openrails: " + e.Route + ": " + e.Reason }

// IsNilAuth reports a missing Auth, typed nil pointers included.
func IsNilAuth(a billingauth.Auth) bool {
	if a == nil {
		return true
	}
	v := reflect.ValueOf(a)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Func, reflect.Interface, reflect.Slice, reflect.Chan:
		return v.IsNil()
	}
	return false
}

func authName(a billingauth.Auth) string { return fmt.Sprintf("%T", a) }

// through runs one of the host's middleware inside the route chain; a nil
// one refuses the mount.
func through(route Route, name string, mw func(http.Handler) http.Handler) router.Middleware {
	if mw == nil {
		panic(MountError{Route: route.Key(), Reason: "Auth." + name + " returned no middleware"})
	}
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) { r.Through(mw, next) }
	}
}

// admittedIdentity reads who the middleware admitted; a panicking Identity admits nobody.
func admittedIdentity(a billingauth.Auth, r *http.Request) (c billingauth.Identity, ok bool) {
	defer func() {
		if v := recover(); v != nil {
			log.WithField("auth", authName(a)).Errorf("openrails: Auth.Identity panicked: %v", v)
			c, ok = billingauth.Identity{}, false
		}
	}()
	c, ok = a.Identity(r.Context())
	return c, ok && strings.TrimSpace(c.Subject) != ""
}

// refuse answers a refusal of OpenRails' own. A fault is a misbehaving Auth
// and logs at ERROR; logs never carry credentials or emails.
func refuse(r *httprequest.Request, route Route, a billingauth.Auth, code, fault string) {
	entry := log.WithFields(log.Fields{"route": route.Key(), "tier": string(route.Auth), "code": code, "request_id": r.RequestID(), "auth": authName(a)})
	if fault != "" {
		entry.Error("openrails: " + fault)
	} else {
		entry.Info("openrails: route gate refused")
	}
	r.AbortCode(code, "")
}

// pin binds target as the merchant the request acts on: the one the host's
// Auth reads (billingauth.BoundMerchant).
func pin(r *httprequest.Request, target billingauth.Target) bool {
	if !middleware.EnforceMerchantBinding(r, target.MerchantID) {
		return false
	}
	ctx := merchanttarget.WithResolved(r.Request.Context(), target)
	ctx = billingauth.BindMerchant(merchant.WithID(ctx, target.MerchantID), target.MerchantID)
	r.Request = r.Request.WithContext(ctx)
	r.Set("openrails.merchant_id", target.MerchantID)
	return true
}

// gateTarget is the merchant pinned before the host's middleware ran, or
// that an internal Auth's middleware resolved.
func gateTarget(r *http.Request) billingauth.Target {
	if target, ok := merchanttarget.FromContext(r.Context()); ok && !target.MerchantID.IsZero() {
		return target
	}
	if id, ok := merchant.FromContext(r.Context()); ok && !id.IsZero() {
		return billingauth.Target{MerchantID: id}
	}
	return billingauth.Target{}
}

// customerGates gates a customer route: the mount's merchant (the profile's,
// the one the request selects on a server, else the configured one), the
// host's Required, then a person as the customer.
func (e *Env) customerGates(route Route) []router.Middleware {
	auth := e.Customers
	if IsNilAuth(auth) {
		panic(MountError{Route: route.Key(), Reason: "a customer route needs Routes.Auth"})
	}
	var out []router.Middleware
	switch fixed := e.CustomerMerchant; {
	case !fixed.MerchantID.IsZero():
		out = append(out, fixedMerchant(fixed))
	case e.SelectedMerchant:
		out = append(out, selectedMerchant())
	case !e.AuthBindsMerchant:
		out = append(out, e.mountedMerchant())
	}
	return append(out, through(route, "Required", auth.Required()), e.customerCheck(route, auth))
}

// fixedMerchant pins the mount's merchant; a request selector may only agree.
func fixedMerchant(fixed billingauth.Target) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			target := fixed
			if resolved, ok := merchanttarget.FromContext(r.Request.Context()); ok {
				// A renamed slug may still name this book; a reused slug may not.
				if resolved.MerchantID != fixed.MerchantID {
					r.AbortCode(billing.CodeMerchantBindingMismatch, "")
					return
				}
				target = resolved
			}
			if host, ok := merchant.HostMerchant(r.Request.Context()); ok && host != target.MerchantID {
				r.AbortCode(billing.CodeHostMerchantMismatch, "")
				return
			}
			if err := merchanttarget.Assert(r.Request, target); err != nil {
				r.AbortGate(err)
				return
			}
			if pin(r, target) {
				next(r)
			}
		}
	}
}

// selectedMerchant pins the merchant the request selected, by its
// OpenRails-Merchant selector (resolved before the gate) or the admin API
// host it called. Without a selection the request is refused.
func selectedMerchant() router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			ctx := r.Request.Context()
			target, selected := merchanttarget.FromContext(ctx)
			selected = selected && !target.MerchantID.IsZero()
			host, hosted := merchant.HostMerchant(ctx)
			switch {
			case selected && hosted && host != target.MerchantID:
				r.AbortCode(billing.CodeHostMerchantMismatch, "")
				return
			case !selected && hosted:
				target = billingauth.Target{MerchantID: host}
			case !selected:
				r.AbortCode(billing.CodeMerchantUnresolved, "")
				return
			}
			if err := merchanttarget.Assert(r.Request, target); err != nil {
				r.AbortGate(err)
				return
			}
			if pin(r, target) {
				next(r)
			}
		}
	}
}

func (e *Env) customerCheck(route Route, a billingauth.Auth) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			c, ok := admittedIdentity(a, r.Request)
			if !ok {
				refuse(r, route, a, billing.CodeAuthenticationRequired, "Auth.Required admitted a request Auth.Identity finds no identity on")
				return
			}
			// The customer is the subject: a user, the same whatever
			// credential they signed in with. An invoker acting on the
			// subject's behalf, or an application subject, may only read its
			// own spend limits.
			if c.SubjectKind != billingauth.SubjectUser && c.SubjectKind != billingauth.SubjectApplication {
				refuse(r, route, a, billing.CodePermissionRequired, "Auth.Identity names a subject that is neither a user nor an application")
				return
			}
			if strings.TrimSpace(c.Invoker.ID) == "" || strings.TrimSpace(c.Invoker.Issuer) == "" {
				refuse(r, route, a, billing.CodeAuthenticationRequired, "Auth.Identity names no invoker; a subject acting itself is its own invoker")
				return
			}
			if (!billingauth.SelfActing(c) || c.SubjectKind != billingauth.SubjectUser) && !route.InvokerScoped {
				refuse(r, route, a, billing.CodeInvokerScopedPrincipal, "")
				return
			}
			id, err := billing.ParseCustomerID(c.Subject)
			if err != nil || id.IsZero() || id.String() != c.Subject {
				refuse(r, route, a, billing.CodeAuthenticationRequired, "Auth.Identity names a subject that is not a canonical UUID")
				return
			}
			target := gateTarget(r.Request)
			if target.MerchantID.IsZero() {
				refuse(r, route, a, billing.CodeMerchantUnresolved, "a customer route admitted an identity at no merchant")
				return
			}
			if bindCustomer(r, c, id, target) {
				next(r)
			}
		}
	}
}

// bindCustomer binds the admitted customer and its scope: the only place a
// customer scope is made.
func bindCustomer(r *httprequest.Request, c billingauth.Identity, id billing.CustomerID, target billingauth.Target) bool {
	if !pin(r, target) {
		return false
	}
	ctx := billingauth.BindIdentity(r.Request.Context(), c)
	r.Request = r.Request.WithContext(customerscope.Bind(ctx, target.MerchantID, id, billingauth.InvokerKey(c), billingauth.Interactive(c)))
	return true
}

// checkoutViewer asks the host's Required who presents a checkout session,
// when a credential is presented, without letting it refuse: the capability
// alone pays with a new card, and only the session's own customer sees and
// pays with its saved cards.
func (e *Env) checkoutViewer(route Route) router.Middleware {
	a := e.Viewers
	if IsNilAuth(a) {
		return func(next router.Handler) router.Handler { return next }
	}
	required := a.Required()
	if required == nil {
		panic(MountError{Route: route.Key(), Reason: "Auth.Required returned no middleware"})
	}
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			if !presentsCredential(r.Request) {
				next(r)
				return
			}
			admitted, ok := probe(required, r.Request)
			if !ok {
				next(r)
				return
			}
			c, ok := admittedIdentity(a, admitted)
			id, err := billing.ParseCustomerID(c.Subject)
			mid, _ := merchant.FromContext(r.Request.Context())
			if at := gateTarget(admitted); !at.MerchantID.IsZero() && at.MerchantID != mid {
				ok = false
			}
			if !ok || !billingauth.Interactive(c) || err != nil || id.IsZero() || id.String() != c.Subject || mid.IsZero() {
				next(r)
				return
			}
			r.Request = admitted
			if bindCustomer(r, c, id, billingauth.Target{MerchantID: mid}) {
				next(r)
			}
		}
	}
}

// presentsCredential reports a request carrying an explicit credential or a
// cookie the mount admitted (ExplicitCredentials strips the rest).
func presentsCredential(r *http.Request) bool {
	return r != nil && (strings.TrimSpace(r.Header.Get("Authorization")) != "" || r.Header.Get("Cookie") != "")
}

// probe runs middleware for its decision alone: the request it admitted, or
// false. Its own refusal is discarded.
func probe(mw func(http.Handler) http.Handler, r *http.Request) (*http.Request, bool) {
	var admitted *http.Request
	mw(http.HandlerFunc(func(_ http.ResponseWriter, passed *http.Request) { admitted = passed })).ServeHTTP(discard{header: http.Header{}}, r)
	return admitted, admitted != nil
}

type discard struct{ header http.Header }

func (d discard) Header() http.Header       { return d.header }
func (discard) Write(b []byte) (int, error) { return len(b), nil }
func (discard) WriteHeader(int)             {}

// staffGates gates a merchant-tier route: the mount's merchant, then the
// host's RequirePermission for the route's permission and, for an operation
// that moves money or removes access by a user in person, Sensitive.
func (e *Env) staffGates(route Route) []router.Middleware {
	a := e.Auth
	if IsNilAuth(a) {
		panic(MountError{Route: route.Key(), Reason: "an admin route needs Routes.Auth"})
	}
	perm := e.permissionFor(route)
	if strings.TrimSpace(perm) == "" {
		panic(MountError{Route: route.Key(), Reason: "no permission (Routes.Permissions)"})
	}
	var out []router.Middleware
	if !e.AuthBindsMerchant {
		out = append(out, e.mountedMerchant())
	}
	out = append(out, through(route, "RequirePermission", a.RequirePermission(perm)))
	if Sensitive(route) {
		out = append(out, inPerson(route, a))
	}
	return append(out, e.staffCheck(route, a))
}

// permissionFor is the permission a merchant-tier route checks: a
// control-plane route's own, a staff route's bundle's.
func (e *Env) permissionFor(route Route) string {
	if !route.Staff() {
		return route.Perm
	}
	return e.Permissions.For(route)
}

// inPerson stacks the host's Sensitive for a user acting in person. A key or
// an application has no sign-in to renew; the provider's permission check
// already admitted it.
func inPerson(route Route, a billingauth.Auth) router.Middleware {
	sensitive := a.Sensitive()
	if sensitive == nil {
		panic(MountError{Route: route.Key(), Reason: "Auth.Sensitive returned no middleware"})
	}
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			c, ok := admittedIdentity(a, r.Request)
			if ok && (c.SubjectKind == billingauth.SubjectUser || c.SubjectKind == billingauth.SubjectApplication) && !billingauth.Interactive(c) {
				next(r)
				return
			}
			r.Through(sensitive, next)
		}
	}
}

// Sensitive reports a merchant route whose operation moves money or removes
// access: the host's Sensitive stacks on it.
func Sensitive(route Route) bool {
	return route.Auth == AuthMerchant && route.Sensitive
}

// mountedMerchant pins the configured merchant, the one the host's
// RequirePermission checks and customers buy from; a request selector may
// only agree. Without one the request is refused.
func (e *Env) mountedMerchant() router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			if e.Runtime == nil || e.Runtime.ConfiguredMerchant().IsZero() {
				r.AbortCode(billing.CodeMerchantUnresolved, "")
				return
			}
			bound := e.Runtime.ConfiguredMerchant()
			target := billingauth.Target{MerchantID: bound}
			// Without a selector, nothing is read before the request is
			// authenticated; a selector is resolved and must agree.
			if _, present, _ := merchant.ParseSelector(r.Request.Header); present {
				var err error
				if target, err = merchanttarget.Resolve(r.Request.Context(), r.Request, e.Runtime.Merchants, bound, ""); err != nil {
					r.AbortGate(err)
					return
				}
			} else if resolved, ok := merchanttarget.FromContext(r.Request.Context()); ok && resolved.MerchantID == bound {
				target = resolved
			}
			if pin(r, target) {
				next(r)
			}
		}
	}
}

func (e *Env) staffCheck(route Route, a billingauth.Auth) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			c, ok := admittedIdentity(a, r.Request)
			if !ok {
				refuse(r, route, a, billing.CodeAuthenticationRequired, "Auth admitted a merchant request Auth.Identity finds no identity on")
				return
			}
			if strings.TrimSpace(c.Invoker.ID) == "" || strings.TrimSpace(c.Invoker.Issuer) == "" {
				refuse(r, route, a, billing.CodeAuthenticationRequired, "Auth.Identity names no invoker; a subject acting itself is its own invoker")
				return
			}
			if c.SubjectKind != billingauth.SubjectUser && c.SubjectKind != billingauth.SubjectApplication {
				refuse(r, route, a, billing.CodePermissionRequired, "Auth.Identity names a subject that is neither a user nor an application")
				return
			}
			target := gateTarget(r.Request)
			if target.MerchantID.IsZero() {
				refuse(r, route, a, billing.CodeMerchantUnresolved, "a merchant route admitted an identity at no merchant")
				return
			}
			if !pin(r, target) {
				return
			}
			r.Request = r.Request.WithContext(billingauth.BindStaff(r.Request.Context(), billingauth.Staff{Identity: c, Route: route.Key(), Merchant: target.MerchantID}))
			next(r)
		}
	}
}

// staffCan answers a handler that asks whether its caller would pass the
// permission of another route, by key, on the merchant the route already
// resolved.
func (e *Env) staffCan(r *http.Request, routeKey string) error {
	if IsNilAuth(e.Auth) {
		return billingauth.ErrUnauthenticated
	}
	if _, ok := billingauth.StaffFromContext(r.Context()); !ok {
		return billingauth.ErrUnauthenticated
	}
	route, ok := routeByKey(routeKey)
	perm := ""
	if ok {
		perm = e.permissionFor(route)
	}
	if perm == "" {
		return billingauth.ErrForbidden
	}
	mw, err := e.permission(perm)
	if err != nil {
		return err
	}
	if _, ok := probe(mw, r); !ok {
		return billingauth.ErrForbidden
	}
	return nil
}

func (e *Env) permission(perm string) (mw func(http.Handler) http.Handler, err error) {
	if cached, ok := e.permissions.Load(perm); ok {
		return cached.(func(http.Handler) http.Handler), nil
	}
	defer func() {
		if v := recover(); v != nil {
			mw, err = nil, fmt.Errorf("Auth.RequirePermission(%q) panicked: %v", perm, v)
		}
	}()
	if mw = e.Auth.RequirePermission(perm); mw == nil {
		return nil, fmt.Errorf("Auth.RequirePermission(%q) returned no middleware", perm)
	}
	e.permissions.Store(perm, mw)
	return mw, nil
}

// routeByKey reads the catalog at request time: the catalog's declarations
// refer to staffCan, so it cannot read the index statically.
var routeByKey func(key string) (Route, bool)

func init() {
	routeByKey = func(key string) (Route, bool) {
		r, ok := index[key]
		return r, ok
	}
}

// recheck is each gated handler's own check, wrapped directly around it: a
// customer route runs only with the customer scope the gate bound, a
// merchant route only with the staff member it admitted for this route's
// permission, at the merchant the request is pinned to. Whatever runs
// between the gate and the handler, a missing verdict is a 401.
func recheck(route Route, h router.Handler) router.Handler {
	switch route.Auth {
	case AuthCustomer:
		return func(r *httprequest.Request) {
			scope, ok := customerscope.From(r.Request.Context())
			_, admitted := billingauth.IdentityFromContext(r.Request.Context())
			mid, pinned := merchant.FromContext(r.Request.Context())
			if !ok || !admitted || !pinned || mid != scope.Merchant() {
				refuse(r, route, nil, billing.CodeAuthenticationRequired, "a customer handler ran without the route gate's customer")
				return
			}
			h(r)
		}
	case AuthMerchant:
		return func(r *httprequest.Request) {
			staff, ok := billingauth.StaffFromContext(r.Request.Context())
			mid, pinned := merchant.FromContext(r.Request.Context())
			if !ok || staff.Route != route.Key() || !pinned || mid != staff.Merchant {
				refuse(r, route, nil, billing.CodeAuthenticationRequired, "a merchant handler ran without the route gate's staff member")
				return
			}
			h(r)
		}
	}
	return h
}

// Guarded is route's handler as the mount registers it, with its re-check.
func (e *Env) Guarded(route Route) router.Handler {
	h := route.Handler
	if route.Bind != nil {
		h = route.Bind(e)
	}
	if h == nil {
		return nil
	}
	return recheck(route, h)
}
