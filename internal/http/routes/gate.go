package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	auth "github.com/open-rails/helpers/auth"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/customerscope"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/internal/scim"
)

// The route gate builds every gate from the mount's Authenticator, which only
// says who a request is. It asks it once (Authenticate) and binds the
// Verified; then, by the route's tier, it checks who the subject is, pins the
// merchant, asks the Verified's Can for the route's permission in that
// merchant's scope and, for a person on a Sensitive route, its
// CheckRecentSignIn. It answers every refusal itself, as auth.Refuse says,
// and binds the verdict in its own context, where handlers re-check it.

// MountError refuses a route the mount cannot gate. Mounting stops; a route
// is never mounted open.
type MountError struct{ Route, Reason string }

func (e MountError) Error() string { return "openrails: " + e.Route + ": " + e.Reason }

// IsNilAuth reports a missing Authenticator, typed nil pointers included.
func IsNilAuth(a billingauth.Authenticator) bool { return isNil(a) }

func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Func, reflect.Interface, reflect.Slice, reflect.Chan:
		return rv.IsNil()
	}
	return false
}

func authName(a any) string { return fmt.Sprintf("%T", a) }

// ScopeFunc is where a merchant's staff hold the mount's Permissions.
type ScopeFunc func(ctx context.Context, mid billing.MerchantID) (billingauth.Scope, error)

// FixedScope is scope at every merchant: an embedded mount's Routes.Scope.
func FixedScope(scope billingauth.Scope) ScopeFunc {
	return func(context.Context, billing.MerchantID) (billingauth.Scope, error) { return scope, nil }
}

// MerchantResolver finds the merchant an authenticated request acts on when
// its credential names none.
type MerchantResolver func(r *http.Request, v billingauth.Verified) (billingauth.Target, error)

// CredentialOnly finds none: the credential must name its merchant.
func CredentialOnly(*http.Request, billingauth.Verified) (billingauth.Target, error) {
	return billingauth.Target{}, billingauth.Refusal(billing.CodeMerchantUnresolved)
}

// step is the call an error came from; refuse classifies it by step.
type step int

const (
	stepAuthenticate step = iota
	stepCan
	stepRecentSignIn
)

// codeFor is the code a host's error from st answers.
func codeFor(st step, err error) string {
	switch st {
	case stepCan:
		switch {
		case errors.Is(err, auth.ErrExpired):
			return billing.CodeCredentialExpired
		case errors.Is(err, auth.ErrRevoked):
			return billing.CodeCredentialRevoked
		}
		return billing.CodeAuthorizationUnavailable
	case stepRecentSignIn:
		switch {
		case errors.Is(err, auth.ErrStepUpRequired):
			return billing.CodeStepUpRequired
		case errors.Is(err, auth.ErrExpired):
			return billing.CodeCredentialExpired
		case errors.Is(err, auth.ErrRevoked):
			return billing.CodeCredentialRevoked
		case errors.Is(err, auth.ErrForbidden):
			return billing.CodeStepUpUnavailable
		}
		return billing.CodeAuthenticationUnavailable
	}
	switch {
	case err == nil, errors.Is(err, auth.ErrUnavailable):
		return billing.CodeAuthenticationUnavailable
	case errors.Is(err, auth.ErrStepUpRequired):
		return billing.CodeStepUpRequired
	case errors.Is(err, auth.ErrSenderProofRequired):
		return billing.CodeSenderProofRequired
	case errors.Is(err, auth.ErrExpired):
		return billing.CodeCredentialExpired
	case errors.Is(err, auth.ErrRevoked):
		return billing.CodeCredentialRevoked
	case errors.Is(err, auth.ErrUnauthenticated):
		return billing.CodeAuthenticationRequired
	case errors.Is(err, auth.ErrForbidden):
		return billing.CodePermissionRequired
	}
	return billing.CodeAuthenticationUnavailable
}

// fault is a provider misbehaving (a panic, an answer outside its
// contract): logged at ERROR and answered as its step's outage.
type fault string

func (f fault) Error() string { return string(f) }

// refuse answers err from st in OpenRails' envelope, with the status and
// challenge every consumer gives (auth.Refuse): an OpenRails refusal keeps
// its code, a host's is classified. Logs never carry credentials or emails,
// nor the provider's text at any status but an outage.
func refuse(r *httprequest.Request, route Route, a any, st step, err error) {
	var gate billingauth.GateError
	if !errors.As(err, &gate) || gate.Code == "" {
		gate = billingauth.Refusal(codeFor(st, err))
		gate.Metadata = auth.Refuse(r.Request, err).Metadata
	}
	if gate.Status == http.StatusUnauthorized {
		for name, values := range auth.Refuse(r.Request, err).Header {
			r.SetHeader(name, strings.Join(values, ", "))
		}
	}
	for name, value := range gate.Headers {
		r.SetHeader(name, value)
	}
	entry := log.WithFields(log.Fields{"route": route.Key(), "tier": string(route.Auth), "code": gate.Code, "request_id": r.RequestID(), "auth": authName(a)})
	var f fault
	switch {
	case errors.As(err, &f):
		entry.Error("openrails: " + f.Error())
	case gate.Status >= http.StatusInternalServerError:
		entry.WithError(err).Warn("openrails: route gate could not decide")
	default:
		entry.Info("openrails: route gate refused")
	}
	r.AbortAPIError(billingauth.RefusalError(gate))
}

// refuseCode answers a refusal of OpenRails' own rules; a fault names the
// provider's misbehavior for the log.
func refuseCode(r *httprequest.Request, route Route, a any, code, why string) {
	err := error(billingauth.Refusal(code))
	if why != "" {
		err = errors.Join(err, fault(why))
	}
	refuse(r, route, a, stepAuthenticate, err)
}

// authenticate asks a who req is. A panic, or neither a Verified nor an
// error, is the provider's fault and an outage.
func authenticate(a billingauth.Authenticator, req *http.Request) (v billingauth.Verified, err error) {
	defer func() {
		if p := recover(); p != nil {
			v, err = nil, errors.Join(auth.ErrUnavailable, fault(fmt.Sprintf("%s.Authenticate panicked: %v", authName(a), p)))
		}
	}()
	v, err = a.Authenticate(req)
	switch {
	case err != nil:
		return nil, err
	case isNil(v):
		return nil, errors.Join(auth.ErrUnavailable, fault(authName(a)+".Authenticate returned neither a Verified nor an error"))
	}
	return v, nil
}

// identityOf is v's identity; a panic is the provider's fault.
func identityOf(v billingauth.Verified) (id billingauth.Identity, err error) {
	defer func() {
		if p := recover(); p != nil {
			id, err = billingauth.Identity{}, errors.Join(auth.ErrUnavailable, fault(fmt.Sprintf("%s.Identity panicked: %v", authName(v), p)))
		}
	}()
	return v.Identity(), nil
}

// can asks v's Can; without one v holds nothing, and a panic is the
// provider's fault.
func can(ctx context.Context, v billingauth.Verified, scope billingauth.Scope, perm string) (ok bool, err error) {
	pc, has := v.(auth.PermissionChecker)
	if !has || perm == "" {
		return false, nil
	}
	defer func() {
		if p := recover(); p != nil {
			ok, err = false, errors.Join(auth.ErrUnavailable, fault(fmt.Sprintf("%s.Can panicked: %v", authName(v), p)))
		}
	}()
	return pc.Can(ctx, scope, perm)
}

// recentSignIn asks v's CheckRecentSignIn; without one v cannot prove a
// recent sign-in, and a panic is the provider's fault.
func recentSignIn(ctx context.Context, v billingauth.Verified) (err error) {
	rs, has := v.(auth.RecentSignInChecker)
	if !has {
		return auth.ErrForbidden
	}
	defer func() {
		if p := recover(); p != nil {
			err = errors.Join(auth.ErrUnavailable, fault(fmt.Sprintf("%s.CheckRecentSignIn panicked: %v", authName(v), p)))
		}
	}()
	return rs.CheckRecentSignIn(ctx)
}

// bindVerified records the request's Verified for the gate's later steps
// and the handlers' own checks.
func bindVerified(r *httprequest.Request, v billingauth.Verified) {
	r.Request = r.Request.WithContext(billingauth.BindVerified(r.Request.Context(), v))
}

// verify asks a, once, who the request is, and binds the Verified.
func verify(route Route, a billingauth.Authenticator) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			v, err := authenticate(a, r.Request)
			if err != nil {
				refuse(r, route, a, stepAuthenticate, err)
				return
			}
			bindVerified(r, v)
			next(r)
		}
	}
}

// subject is the request's identity when it names a subject a gated tier
// can serve: a user or an application, with its invoker.
func subject(r *httprequest.Request, route Route, a any) (billingauth.Verified, billingauth.Identity, bool) {
	v, ok := billingauth.VerifiedFrom(r.Request.Context())
	if !ok {
		refuseCode(r, route, a, billing.CodeAuthenticationRequired, "a gated route ran without a Verified")
		return nil, billingauth.Identity{}, false
	}
	c, err := identityOf(v)
	switch {
	case err != nil:
		refuse(r, route, a, stepAuthenticate, err)
	case strings.TrimSpace(c.Subject) == "":
		refuseCode(r, route, a, billing.CodeAuthenticationRequired, "the Verified's Identity names no subject")
	case strings.TrimSpace(c.Invoker.ID) == "" || strings.TrimSpace(c.Invoker.Issuer) == "":
		refuseCode(r, route, a, billing.CodeAuthenticationRequired, "the Verified's Identity names no invoker; a subject acting itself is its own invoker")
	case c.SubjectKind != billingauth.SubjectUser && c.SubjectKind != billingauth.SubjectApplication:
		refuseCode(r, route, a, billing.CodePermissionRequired, "the Verified's Identity names a subject that is neither a user nor an application")
	default:
		return v, c, true
	}
	return nil, billingauth.Identity{}, false
}

// pin binds target as the merchant the request acts on.
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

// gateTarget is the merchant pinned before the request was authenticated.
func gateTarget(r *http.Request) billingauth.Target {
	if target, ok := merchanttarget.FromContext(r.Context()); ok && !target.MerchantID.IsZero() {
		return target
	}
	if id, ok := merchant.FromContext(r.Context()); ok && !id.IsZero() {
		return billingauth.Target{MerchantID: id}
	}
	return billingauth.Target{}
}

// requestMerchant pins the merchant an authenticated request acts on. A
// mount without ResolveMerchant pinned its merchant before authenticating,
// which a credential naming its own must agree with. Otherwise it is the
// one the credential names, else the one ResolveMerchant finds; a request
// selector and the API host may only agree.
func (e *Env) requestMerchant(r *httprequest.Request, route Route, a any, v billingauth.Verified) (billingauth.Target, bool) {
	named, ok := billingauth.NamedMerchant(v)
	if e.ResolveMerchant == nil {
		pinned := gateTarget(r.Request)
		switch {
		case pinned.MerchantID.IsZero():
			refuseCode(r, route, a, billing.CodeMerchantUnresolved, "a gated route admitted an identity at no merchant")
			return billingauth.Target{}, false
		case ok && named.MerchantID != pinned.MerchantID:
			refuseCode(r, route, a, billing.CodeMerchantBindingMismatch, "")
			return billingauth.Target{}, false
		}
		return pinned, true
	}
	target := named
	if !ok {
		var err error
		if target, err = e.ResolveMerchant(r.Request, v); err != nil {
			refuse(r, route, a, stepAuthenticate, err)
			return billingauth.Target{}, false
		}
	}
	ctx := r.Request.Context()
	if host, hosted := merchant.HostMerchant(ctx); hosted && host != target.MerchantID {
		refuseCode(r, route, a, billing.CodeHostMerchantMismatch, "")
		return billingauth.Target{}, false
	}
	// A selector already resolved to this merchant (a former name included)
	// stays as the request named it; one naming another merchant is refused.
	if resolved, ok := merchanttarget.FromContext(ctx); ok && resolved.MerchantID == target.MerchantID {
		target = resolved
	} else {
		ctx = merchanttarget.WithResolved(ctx, target)
	}
	r.Request = r.Request.WithContext(merchant.WithID(ctx, target.MerchantID))
	return target, pin(r, target)
}

// scope is where mid's staff hold the mount's Permissions.
func (e *Env) scope(ctx context.Context, mid billing.MerchantID) (billingauth.Scope, error) {
	if e.Scope == nil {
		return billingauth.Scope{}, errors.Join(auth.ErrUnavailable, fault("a staff route ran without Options.Scope"))
	}
	s, err := e.Scope(ctx, mid)
	switch {
	case err != nil:
		return billingauth.Scope{}, err
	case s.Authority == "" || s.ID == "":
		return billingauth.Scope{}, errors.Join(auth.ErrUnavailable, fault("merchant "+mid.String()+" has no scope"))
	}
	return s, nil
}

// permitted asks the Verified's Can for perm at mid.
func (e *Env) permitted(r *httprequest.Request, route Route, a any, v billingauth.Verified, mid billing.MerchantID, perm string) bool {
	scope, err := e.scope(r.Request.Context(), mid)
	if err != nil {
		refuse(r, route, a, stepCan, err)
		return false
	}
	allowed, err := can(r.Request.Context(), v, scope, perm)
	switch {
	case err != nil:
		refuse(r, route, a, stepCan, err)
		return false
	case !allowed:
		refuseCode(r, route, a, billing.CodePermissionRequired, "")
		return false
	}
	return true
}

// customerGates gates a customer route: the mount's merchant (the profile's,
// the one the request selects on a server, else the configured one), then a
// person acting for itself as the customer.
func (e *Env) customerGates(route Route) []router.Middleware {
	a := e.Customers
	if IsNilAuth(a) {
		panic(MountError{Route: route.Key(), Reason: "a customer route needs Routes.Auth"})
	}
	var out []router.Middleware
	if e.ResolveMerchant == nil {
		switch fixed := e.CustomerMerchant; {
		case !fixed.MerchantID.IsZero():
			out = append(out, fixedMerchant(fixed))
		case e.SelectedMerchant:
			out = append(out, selectedMerchant())
		default:
			out = append(out, e.mountedMerchant())
		}
	}
	return append(out, verify(route, a), e.customerCheck(route, a))
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

func (e *Env) customerCheck(route Route, a billingauth.Authenticator) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			v, c, ok := subject(r, route, a)
			if !ok {
				return
			}
			// The customer is the subject: a user acting itself, the same
			// whatever credential they signed in with. An invoker acting on
			// its behalf, or an application subject, is refused.
			if !billingauth.SelfActing(c) || c.SubjectKind != billingauth.SubjectUser {
				refuseCode(r, route, a, billing.CodeInvokerScopedPrincipal, "")
				return
			}
			id, err := billing.ParseCustomerID(c.Subject)
			if err != nil || id.IsZero() || id.String() != c.Subject {
				refuseCode(r, route, a, billing.CodeAuthenticationRequired, "the Verified's Identity names a customer that is not a canonical UUID")
				return
			}
			target, ok := e.requestMerchant(r, route, a, v)
			if ok && bindCustomer(r, c, id, target) {
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

// checkoutViewer asks who presents a checkout session, when a credential is
// presented, without letting it refuse: the capability alone pays with a new
// card, and only the session's own customer, a person in person, sees and
// pays with its saved cards.
func (e *Env) checkoutViewer(route Route) router.Middleware {
	a := e.Viewers
	if IsNilAuth(a) {
		return func(next router.Handler) router.Handler { return next }
	}
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			if !presentsCredential(r.Request) {
				next(r)
				return
			}
			v, err := authenticate(a, r.Request)
			if err != nil {
				next(r)
				return
			}
			c, err := identityOf(v)
			id, perr := billing.ParseCustomerID(c.Subject)
			mid, _ := merchant.FromContext(r.Request.Context())
			named, bound := billingauth.NamedMerchant(v)
			if err != nil || perr != nil || mid.IsZero() || bound && named.MerchantID != mid ||
				!billingauth.Interactive(c) || id.IsZero() || id.String() != c.Subject || c.Invoker.Issuer == "" {
				next(r)
				return
			}
			bindVerified(r, v)
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

// staffGates gates a merchant-tier route: the request's merchant, a person
// or an application holding the route's permission in its scope and, for a
// person on an operation that moves money or removes access, a recent
// sign-in.
func (e *Env) staffGates(route Route) []router.Middleware {
	a := e.Auth
	if IsNilAuth(a) {
		panic(MountError{Route: route.Key(), Reason: "an admin route needs Routes.Auth"})
	}
	perm := e.Permissions.For(route)
	if strings.TrimSpace(perm) == "" {
		panic(MountError{Route: route.Key(), Reason: "no permission (Routes.Permissions)"})
	}
	if e.Scope == nil {
		panic(MountError{Route: route.Key(), Reason: "an admin route needs Routes.Scope"})
	}
	return append(e.preAuth(), verify(route, a), e.staffCheck(route, a, perm))
}

// preAuth pins the mount's configured merchant before the request is
// authenticated, unless the merchant comes from it.
func (e *Env) preAuth() []router.Middleware {
	if e.ResolveMerchant != nil {
		return nil
	}
	return []router.Middleware{e.mountedMerchant()}
}

// Sensitive reports a merchant route whose operation moves money or removes
// access: a person needs a recent sign-in for it.
func Sensitive(route Route) bool {
	return route.Auth == AuthMerchant && route.Sensitive
}

// mountedMerchant pins the configured merchant, the one the mount's staff
// administer and customers buy from; a request selector may only agree.
// Without one the request is refused.
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

// bindStaff binds the admitted staff member or application at target: the
// only place a staff identity is bound.
func bindStaff(r *httprequest.Request, c billingauth.Identity, route Route, target billingauth.Target) {
	r.Request = r.Request.WithContext(billingauth.BindStaff(r.Request.Context(), billingauth.Staff{Identity: c, Route: route.Key(), Merchant: target.MerchantID}))
}

func (e *Env) staffCheck(route Route, a billingauth.Authenticator, perm string) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			v, c, ok := subject(r, route, a)
			if !ok {
				return
			}
			target, ok := e.requestMerchant(r, route, a, v)
			if !ok || !e.permitted(r, route, a, v, target.MerchantID, perm) {
				return
			}
			// An application has no sign-in to renew: its permission is the
			// whole check. A person proves a recent one, whatever credential
			// they present.
			if Sensitive(route) && c.SubjectKind == billingauth.SubjectUser {
				if err := recentSignIn(r.Request.Context(), v); err != nil {
					refuse(r, route, a, stepRecentSignIn, err)
					return
				}
			}
			bindStaff(r, c, route, target)
			next(r)
		}
	}
}

// staffCan answers a handler that asks whether its caller would pass the
// permission of another route, by key, on the merchant the route already
// resolved.
func (e *Env) staffCan(r *http.Request, routeKey string) error {
	staff, ok := billingauth.StaffFromContext(r.Context())
	v, verified := billingauth.VerifiedFrom(r.Context())
	if !ok || !verified {
		return billingauth.ErrUnauthenticated
	}
	route, ok := routeByKey(routeKey)
	perm := ""
	if ok {
		perm = e.Permissions.For(route)
	}
	if perm == "" {
		return billingauth.ErrForbidden
	}
	scope, err := e.scope(r.Context(), staff.Merchant)
	if err != nil {
		return err
	}
	allowed, err := can(r.Context(), v, scope, perm)
	switch {
	case err != nil:
		return err
	case !allowed:
		return billingauth.ErrForbidden
	}
	return nil
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
				refuseCode(r, route, nil, billing.CodeAuthenticationRequired, "a customer handler ran without the route gate's customer")
				return
			}
			h(r)
		}
	case AuthMerchant:
		return func(r *httprequest.Request) {
			staff, ok := billingauth.StaffFromContext(r.Request.Context())
			mid, pinned := merchant.FromContext(r.Request.Context())
			if !ok || staff.Route != route.Key() || !pinned || mid != staff.Merchant {
				refuseCode(r, route, nil, billing.CodeAuthenticationRequired, "a merchant handler ran without the route gate's staff member")
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

// appGates gates a programmatic route: an application, at the request's
// merchant. A person is refused whatever it holds.
func (e *Env) appGates(route Route) []router.Middleware {
	a := e.Auth
	if IsNilAuth(a) {
		panic(MountError{Route: route.Key(), Reason: "a programmatic route needs Routes.Auth"})
	}
	return append(e.preAuth(), verify(route, a), e.appCheck(route, a))
}

func (e *Env) appCheck(route Route, a billingauth.Authenticator) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			v, c, ok := subject(r, route, a)
			if !ok {
				return
			}
			if c.SubjectKind != billingauth.SubjectApplication {
				refuseCode(r, route, a, billing.CodeApplicationRequired, "")
				return
			}
			target, ok := e.requestMerchant(r, route, a, v)
			if !ok {
				return
			}
			// The application check above is the whole gate: Permissions
			// holds no permission for the programmatic routes (open with the
			// owner, #1179). Given one (Route.Needs naming it), it is asked
			// here like a staff route's.
			if perm := e.Permissions.For(route); perm != "" && !e.permitted(r, route, a, v, target.MerchantID, perm) {
				return
			}
			bindStaff(r, c, route, target)
			next(r)
		}
	}
}

type provisionerKey struct{}

// provisioningGate admits a SCIM request by the merchant's provisioning
// token, which opens only these routes, or else as a programmatic route.
// A refusal before the SCIM server runs is answered as a SCIM error.
func (e *Env) provisioningGate(route Route) router.Middleware {
	gates := e.appGates(route)
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			if mid, ok := e.provisioner(r, route); ok {
				if pin(r, billingauth.Target{MerchantID: mid}) {
					r.Request = r.Request.WithContext(context.WithValue(r.Request.Context(), provisionerKey{}, mid))
					next(r)
				}
				return
			}
			var refusals *scimRefusals
			asApp := func(r *httprequest.Request) {
				refusals.passed = true
				next(r)
			}
			for i := len(gates) - 1; i >= 0; i-- {
				asApp = gates[i](asApp)
			}
			r.Through(func(h http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					refusals = &scimRefusals{ResponseWriter: w}
					h.ServeHTTP(refusals, req)
					refusals.flush()
				})
			}, asApp)
		}
	}
}

// provisioner is the merchant whose provisioning token r presents, when
// this mount serves it; another merchant's token is no credential here.
func (e *Env) provisioner(r *httprequest.Request, route Route) (billing.MerchantID, bool) {
	token := scim.BearerToken(r.Request)
	if token == "" || e.Runtime == nil || e.Runtime.DB == nil {
		return billing.MerchantID{}, false
	}
	mid, err := scim.Tokens{DB: e.Runtime.DB}.Resolve(r.Request.Context(), token)
	if err != nil {
		if !errors.Is(err, scim.ErrUnauthorized) {
			log.WithError(err).WithField("route", route.Key()).Error("openrails: provisioning token lookup failed")
		}
		return billing.MerchantID{}, false
	}
	host, hosted := merchant.HostMerchant(r.Request.Context())
	if e.ResolveMerchant == nil && e.Runtime.ConfiguredMerchant() != mid || hosted && host != mid {
		return billing.MerchantID{}, false
	}
	return mid, true
}

// scimRefusals answers a refusal written before the SCIM server ran as a
// SCIM error, keeping its status and headers (a challenge among them).
type scimRefusals struct {
	http.ResponseWriter
	passed bool
	status int
	body   bytes.Buffer
}

func (w *scimRefusals) WriteHeader(status int) {
	if w.passed {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.status = status
}

func (w *scimRefusals) Write(b []byte) (int, error) {
	if w.passed {
		return w.ResponseWriter.Write(b)
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(b)
}

func (w *scimRefusals) flush() {
	if w.passed || w.status == 0 {
		return
	}
	var refused struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.body.Bytes(), &refused)
	detail := refused.Error.Message
	if detail == "" {
		detail = http.StatusText(w.status)
	}
	w.Header().Del("Content-Length")
	scim.WriteRefusal(w.ResponseWriter, w.status, detail)
}

// scimMerchant is the merchant the SCIM gate admitted: a provisioning
// token's, an application's, or the in-process directory's.
func (e *Env) scimMerchant(r *http.Request) (billing.MerchantID, error) {
	if !e.SCIMMerchant.IsZero() {
		return e.SCIMMerchant, nil
	}
	if mid, ok := r.Context().Value(provisionerKey{}).(billing.MerchantID); ok && !mid.IsZero() {
		return mid, nil
	}
	if staff, ok := billingauth.StaffFromContext(r.Context()); ok && staff.Identity.SubjectKind == billingauth.SubjectApplication && !staff.Merchant.IsZero() {
		return staff.Merchant, nil
	}
	return billing.MerchantID{}, scim.ErrUnauthorized
}

// signedInGates gates the access read: any person or application at the
// request's merchant.
func (e *Env) signedInGates(route Route) []router.Middleware {
	a := e.Auth
	if IsNilAuth(a) {
		panic(MountError{Route: route.Key(), Reason: "a staff route needs Routes.Auth"})
	}
	if e.Scope == nil {
		panic(MountError{Route: route.Key(), Reason: "a staff route needs Routes.Scope"})
	}
	return append(e.preAuth(), verify(route, a), func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			v, c, ok := subject(r, route, a)
			if !ok {
				return
			}
			if target, ok := e.requestMerchant(r, route, a, v); ok {
				bindStaff(r, c, route, target)
				next(r)
			}
		}
	})
}

// adminAccess answers what the caller may use of each mounted staff group,
// asking the Verified's Can for each group's permission: what a console
// shows its user. A group counts only where it is mounted and the caller
// holds it.
func adminAccess(e *Env) router.Handler {
	return func(r *httprequest.Request) {
		ctx := r.Request.Context()
		route, _ := routeByKey("GET /v1/admin/access")
		staff, _ := billingauth.StaffFromContext(ctx)
		v, ok := billingauth.VerifiedFrom(ctx)
		if !ok {
			refuseCode(r, route, e.Auth, billing.CodeAuthenticationRequired, "the access read ran without a Verified")
			return
		}
		scope, err := e.scope(ctx, staff.Merchant)
		if err != nil {
			refuse(r, route, e.Auth, stepCan, err)
			return
		}
		var failed error
		holds := func(perm string) bool {
			if perm == "" || failed != nil {
				return false
			}
			ok, err := can(ctx, v, scope, perm)
			if err != nil {
				failed = err
			}
			return ok && err == nil
		}
		p := e.Permissions
		level := func(read, update bool) billing.AccessLevel {
			switch {
			case read && update:
				return billing.AccessUpdate
			case read:
				return billing.AccessRead
			}
			return billing.AccessNone
		}
		support := holds(p.AdminRead)
		access := billing.AdminAccess{
			Admin:          level(support, support && holds(p.AdminUpdate)),
			Catalog:        holds(p.Catalog),
			MerchantConfig: holds(p.MerchantConfig),
			Metrics:        holds(p.Metrics),
		}
		if failed != nil {
			refuse(r, route, e.Auth, stepCan, failed)
			return
		}
		r.SuccessJSON(access)
	}
}
