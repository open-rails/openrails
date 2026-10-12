package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	auth "github.com/open-rails/authkit/auth"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/customernetwork"
	"github.com/open-rails/openrails/internal/customerscope"
	"github.com/open-rails/openrails/internal/db"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/scim"
)

// The route gate builds every gate from the mount's Authenticator, which only
// says who a request is. It asks it once (Authenticate) and binds the
// Verified; then it pins the merchant the credential is bound to
// (boundMerchant) and, by the route's tier, checks who the subject is, asks
// the Verified's Can for the route's permission (its merchant: name in that
// merchant's scope, else its root: name in the Authenticator's root scope).
// Network routes marked Sensitive also ask for CheckRecentSignIn. The optional
// expected-merchant precondition can refuse a binding, never select one.
// It answers every refusal itself, as auth.Refuse says, and binds the verdict
// in its own context, where handlers re-check it.

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
	case errors.Is(err, auth.ErrExpired):
		return billing.CodeCredentialExpired
	case errors.Is(err, auth.ErrRevoked):
		return billing.CodeCredentialRevoked
	case errors.Is(err, auth.ErrUnauthenticated), errors.Is(err, auth.ErrSenderProofRequired):
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

// pin binds target as the merchant the request acts on, and names it on the
// response.
func pin(r *httprequest.Request, target billingauth.Target) {
	r.Request = r.Request.WithContext(merchant.WithID(r.Request.Context(), target.MerchantID))
	r.SetHeader(merchant.BoundHeader, target.MerchantID.String())
}

// boundMerchant is the one merchant v's credential is bound to, by the
// first of three bindings, and nothing of the request:
//
//   - the scope it is bound to (authkit/auth Bound), which is a merchant's in
//     the Authenticator's root authority, its id the merchant's; a scope that
//     is not never falls through to the next binding;
//   - the merchant that declares its issuer (MerchantDeclaration.Issuer);
//   - the row an OpenRails credential of its own names (a checkout URL's
//     secret, a provisioning token, the in-process host).
//
// A credential bound to no live merchant is merchant_unresolved.
func (e *Env) boundMerchant(ctx context.Context, a any, v billingauth.Verified, c billingauth.Identity) (billingauth.Target, error) {
	unresolved := billingauth.Refusal(billing.CodeMerchantUnresolved)
	var id billing.MerchantID
	if scope := boundScope(v); scope != (billingauth.Scope{}) {
		if _, ok := a.(auth.Root); !ok {
			return billingauth.Target{}, unresolved
		}
		root, err := rootScope(ctx, a)
		if err != nil {
			return billingauth.Target{}, err
		}
		parsed, err := billing.ParseMerchantID(scope.ID)
		if scope.Authority != root.Authority || err != nil || parsed.String() != scope.ID {
			return billingauth.Target{}, unresolved
		}
		id = parsed
	} else if declared, ok := e.Runtime.IssuerMerchant(c.Issuer); ok {
		id = declared
	} else if named, ok := billingauth.NamedMerchant(v); ok {
		id = named.MerchantID
	}
	if id.IsZero() {
		return billingauth.Target{}, unresolved
	}
	switch live, err := e.live(ctx, id); {
	case err != nil:
		return billingauth.Target{}, errors.Join(auth.ErrUnavailable, err)
	case !live:
		return billingauth.Target{}, unresolved
	}
	return billingauth.Target{MerchantID: id}, nil
}

// boundScope is the scope v's credential is bound to; zero for one bound to
// none.
func boundScope(v billingauth.Verified) billingauth.Scope {
	if b, ok := v.(auth.Bound); ok {
		return b.BoundScope()
	}
	return billingauth.Scope{}
}

// liveMerchant reads the directory for an active merchant, outside any
// merchant's connection.
func liveMerchant(rt *app.Runtime) func(context.Context, billing.MerchantID) (bool, error) {
	return func(ctx context.Context, id billing.MerchantID) (bool, error) {
		if rt == nil || rt.DB == nil {
			return false, errors.New("no database lists merchants")
		}
		row, err := rt.DB.GenDirectory().GetMerchantDirectoryByID(ctx, id.UUID())
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return false, nil
		case err != nil:
			return false, err
		}
		return row.Status == string(merchants.StatusActive), nil
	}
}

// bound is the request's subject and the merchant its credential is bound
// to, pinned: every gated tier's first step, before any rule of its own.
func (e *Env) bound(r *httprequest.Request, route Route, a any) (billingauth.Verified, billingauth.Identity, billingauth.Target, bool) {
	v, c, ok := subject(r, route, a)
	if !ok {
		return nil, billingauth.Identity{}, billingauth.Target{}, false
	}
	target, err := e.boundMerchant(r.Request.Context(), a, v, c)
	if err != nil {
		refuse(r, route, a, stepCan, err)
		return nil, billingauth.Identity{}, billingauth.Target{}, false
	}
	pin(r, target)
	if values, present := r.Request.Header[http.CanonicalHeaderKey(merchant.ExpectedHeader)]; present {
		var expected billing.MerchantID
		var parseErr error
		if len(values) == 1 {
			expected, parseErr = billing.ParseMerchantID(strings.TrimSpace(values[0]))
		}
		if len(values) != 1 || parseErr != nil || expected.IsZero() {
			r.AbortCode(billing.CodeInvalidParam, merchant.ExpectedHeader+" must contain one nonzero merchant UUID")
			return nil, billingauth.Identity{}, billingauth.Target{}, false
		}
		if expected != target.MerchantID {
			r.AbortCode(billing.CodeMerchantMismatch, "The credential is bound to a different merchant; the operation was not executed.")
			return nil, billingauth.Identity{}, billingauth.Target{}, false
		}
	}
	return v, c, target, true
}

// rootScope is where a's root: permissions are held (authkit/auth Root). A
// panic, or a scope that is not whole, is the provider's fault.
func rootScope(ctx context.Context, a any) (scope billingauth.Scope, err error) {
	root, ok := a.(auth.Root)
	if !ok {
		return billingauth.Scope{}, errors.Join(auth.ErrUnavailable, fault(authName(a)+" has no RootScope"))
	}
	defer func() {
		if p := recover(); p != nil {
			scope, err = billingauth.Scope{}, errors.Join(auth.ErrUnavailable, fault(fmt.Sprintf("%s.RootScope panicked: %v", authName(a), p)))
		}
	}()
	scope, err = root.RootScope(ctx)
	switch {
	case err != nil:
		return billingauth.Scope{}, err
	case scope.Authority == "" || scope.ID == "":
		return billingauth.Scope{}, errors.Join(auth.ErrUnavailable, fault(authName(a)+".RootScope named no scope"))
	}
	return scope, nil
}

// merchantScope is where mid's own staff and credentials hold their
// merchant: permissions: the scope of root's authority whose id is the
// merchant's.
func merchantScope(root billingauth.Scope, mid billing.MerchantID) billingauth.Scope {
	return billingauth.Scope{Authority: root.Authority, ID: mid.String()}
}

// asked is pair without the names a does not know (auth.PermissionCatalog),
// which are never asked: a host declares only the persona it grants in.
// Without a catalog both are asked.
func asked(a any, pair Pair) Pair {
	catalog, ok := a.(auth.PermissionCatalog)
	if !ok {
		return pair
	}
	if pair.Merchant != "" && !catalog.KnownPermission(string(pair.Merchant)) {
		pair.Merchant = ""
	}
	if pair.Root != "" && !catalog.KnownPermission(string(pair.Root)) {
		pair.Root = ""
	}
	return pair
}

// needs is the pair route's gate asks of a. Mounting fails when a has no
// root scope to ask in, or knows neither name.
func needs(route Route, a billingauth.Authenticator) Pair {
	if _, ok := a.(auth.Root); !ok {
		panic(MountError{Route: route.Key(), Reason: "Routes.Auth (" + authName(a) + ") is not a authkit/auth Root: staff and programmatic routes ask its RootScope"})
	}
	pair := route.Needs()
	if pair == (Pair{}) {
		return pair
	}
	known := asked(a, pair)
	if known == (Pair{}) {
		panic(MountError{Route: route.Key(), Reason: fmt.Sprintf("Routes.Auth knows none of its permissions %q", pair.Names())})
	}
	return known
}

// holds reports whether v holds pair at mid: its merchant: name in mid's
// scope, else its root: name in root.
func holds(ctx context.Context, v billingauth.Verified, root billingauth.Scope, mid billing.MerchantID, pair Pair) (bool, error) {
	if ok, err := can(ctx, v, merchantScope(root, mid), string(pair.Merchant)); ok || err != nil {
		return ok && err == nil, err
	}
	return can(ctx, v, root, string(pair.Root))
}

// permitted asks the Verified's Can for pair at mid.
func (e *Env) permitted(r *httprequest.Request, route Route, a any, v billingauth.Verified, mid billing.MerchantID, pair Pair) bool {
	root, err := rootScope(r.Request.Context(), a)
	if err != nil {
		refuse(r, route, a, stepCan, err)
		return false
	}
	allowed, err := holds(r.Request.Context(), v, root, mid, pair)
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

// customerGates gates a customer route: a person acting for itself as the
// customer, at the merchant its credential is bound to.
func (e *Env) customerGates(route Route) []router.Middleware {
	a := e.Customers
	if IsNilAuth(a) {
		panic(MountError{Route: route.Key(), Reason: "a customer route needs Routes.Auth"})
	}
	return []router.Middleware{verify(route, a), e.customerCheck(route, a)}
}

func (e *Env) customerCheck(route Route, a billingauth.Authenticator) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			v, c, target, ok := e.bound(r, route, a)
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
			if e.checkout {
				// A checkout URL acts as its order's customer, whose row the
				// order holds, whatever its issuer: nothing to admit.
				bindCustomer(r, c, id, target)
				next(r)
				return
			}
			who, err := customerIssuer(r.Request.Context(), a, v, c)
			switch {
			case err != nil:
				refuse(r, route, a, stepCan, err)
				return
			case !who.self:
				refuseCode(r, route, a, billing.CodePermissionRequired, "")
				return
			}
			switch err := e.admit(r.Request.Context(), target.MerchantID, id, who.issuer, who.account); {
			case errors.Is(err, db.ErrCustomerIssuerMismatch):
				refuseCode(r, route, a, billing.CodeCustomerIssuerMismatch, "")
			case err != nil:
				refuse(r, route, a, stepCan, err)
			default:
				bindCustomer(r, c, id, target)
				next(r)
			}
		}
	}
}

// customerOf is whose subject a customer is.
type customerOf struct {
	// issuer is the issuer whose subject it is (OIDC Core §5.7): "" for the
	// merchant's own customer.
	issuer string
	// account: the subject is an account of the Authenticator's own
	// authority (the customer network's accounts are).
	account bool
	// self: the credential was issued to act as the customer.
	self bool
}

// customerIssuer says whose subject a customer is. The merchant's own
// customers (issuer ""): a person whose credential is bound to no scope (the
// user of the issuer the merchant declares), an account of the
// Authenticator's own authority bound to the merchant, and the customer the
// merchant's own backend names in an assertion (auth.Asserted). Any other
// bound credential's user is
// its issuer's, pinned to it. A token of the Authenticator's own authority
// that carries scopes but not ScopeSelf, such as a console's, was not issued
// to act as a customer; one that says nothing of its scopes is the host's
// own mint.
func customerIssuer(ctx context.Context, a any, v billingauth.Verified, c billingauth.Identity) (customerOf, error) {
	_, isRoot := a.(auth.Root)
	bound := boundScope(v) != (billingauth.Scope{})
	if !bound && !isRoot {
		return customerOf{self: true}, nil
	}
	root, err := rootScope(ctx, a)
	if err != nil {
		return customerOf{}, err
	}
	switch {
	case c.Issuer == root.Authority:
		scoped, has := v.(interface{ Scopes() []string })
		return customerOf{account: true, self: !bound || !has || slices.Contains(scoped.Scopes(), billing.ScopeSelf)}, nil
	case !bound:
		return customerOf{self: true}, nil
	}
	if asserted, ok := v.(auth.Asserted); ok && asserted.Asserted() {
		return customerOf{self: true}, nil
	}
	return customerOf{issuer: c.Issuer, self: true}, nil
}

// networkGates gate a network route: the account itself and, under one of
// its active links, the customer that link names, at its merchant.
func (e *Env) networkGates(route Route) []router.Middleware {
	a := e.Customers
	if IsNilAuth(a) {
		panic(MountError{Route: route.Key(), Reason: "a network route needs Routes.Auth"})
	}
	if _, ok := a.(auth.Root); !ok {
		panic(MountError{Route: route.Key(), Reason: "Routes.Auth (" + authName(a) + ") is not a authkit/auth Root: a network account is its root authority's own"})
	}
	return []router.Middleware{verify(route, a), e.networkCheck(route, a)}
}

func (e *Env) networkCheck(route Route, a billingauth.Authenticator) router.Middleware {
	throughLink := strings.HasPrefix(route.Path, linkPrefix+"/")
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			v, c, ok := subject(r, route, a)
			if !ok {
				return
			}
			ctx := r.Request.Context()
			root, err := rootScope(ctx, a)
			if err != nil {
				refuse(r, route, a, stepCan, err)
				return
			}
			// The deployment's own user acting for itself, signed in to its
			// account and bound to no merchant.
			account, err := billing.ParseNetworkAccountID(c.Subject)
			if !billingauth.SelfActing(c) || c.SubjectKind != billingauth.SubjectUser || c.Issuer != root.Authority ||
				boundScope(v) != (billingauth.Scope{}) || err != nil || account.IsZero() || account.String() != c.Subject {
				refuseCode(r, route, a, billing.CodePermissionRequired, "")
				return
			}
			network := e.Runtime.Network
			if err := network.Admit(ctx, account); err != nil {
				refuse(r, route, a, stepCan, err)
				return
			}
			if route.Sensitive {
				if err := recentSignIn(ctx, v); err != nil {
					refuse(r, route, a, stepRecentSignIn, err)
					return
				}
			}
			r.Request = r.Request.WithContext(customernetwork.WithAccount(ctx, account))
			if !throughLink {
				next(r)
				return
			}
			id, err := billing.ParseCustomerLinkID(r.Param("link_id"))
			if err != nil || id.IsZero() {
				refuseCode(r, route, a, billing.CodeResourceNotFound, "")
				return
			}
			link, active, err := network.ActiveLink(ctx, account, id)
			switch {
			case err != nil:
				refuse(r, route, a, stepCan, err)
				return
			case !active:
				refuseCode(r, route, a, billing.CodeResourceNotFound, "")
				return
			}
			// The link proves the account is that customer: it acts as it,
			// itself, in person when it signed in.
			as := c
			as.Subject = link.CustomerID.String()
			as.Invoker = auth.Invoker{Issuer: c.Issuer, ID: as.Subject}
			bindCustomer(r, as, link.CustomerID, billingauth.Target{MerchantID: link.MerchantID})
			next(r)
		}
	}
}

// bindCustomer binds the admitted customer and its scope: the only place a
// customer scope is made.
func bindCustomer(r *httprequest.Request, c billingauth.Identity, id billing.CustomerID, target billingauth.Target) {
	pin(r, target)
	ctx := billingauth.BindIdentity(r.Request.Context(), c)
	r.Request = r.Request.WithContext(customerscope.Bind(ctx, target.MerchantID, id, billingauth.InvokerKey(c), billingauth.Interactive(c)))
}

// staffGates gates a merchant-tier route: a person or an application holding
// the route's permission at the merchant its credential is bound to, or in
// the root scope. Admin operations do not ask for a recent sign-in.
func (e *Env) staffGates(route Route) []router.Middleware {
	a := e.Auth
	if IsNilAuth(a) {
		panic(MountError{Route: route.Key(), Reason: "an admin route needs Routes.Auth"})
	}
	pair := needs(route, a)
	if pair == (Pair{}) {
		panic(MountError{Route: route.Key(), Reason: "an admin route declares no permission"})
	}
	return []router.Middleware{verify(route, a), e.staffCheck(route, a, pair)}
}

// bindStaff binds the admitted staff member or application at target: the
// only place a staff identity is bound.
func bindStaff(r *httprequest.Request, c billingauth.Identity, route Route, target billingauth.Target) {
	r.Request = r.Request.WithContext(billingauth.BindStaff(r.Request.Context(), billingauth.Staff{Identity: c, Route: route.Key(), Merchant: target.MerchantID}))
}

func (e *Env) staffCheck(route Route, a billingauth.Authenticator, pair Pair) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			v, c, target, ok := e.bound(r, route, a)
			if !ok || !e.permitted(r, route, a, v, target.MerchantID, pair) {
				return
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
	var pair Pair
	if route, ok := routeByKey(routeKey); ok && e.on(route) {
		pair = asked(e.Auth, route.Needs())
	}
	if pair == (Pair{}) {
		return billingauth.ErrForbidden
	}
	root, err := rootScope(r.Context(), e.Auth)
	if err != nil {
		return err
	}
	allowed, err := holds(r.Context(), v, root, staff.Merchant, pair)
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

// appGates gates a programmatic route: an application holding the route's
// permission at the merchant its credential is bound to. A person is refused
// whatever it holds. SCIM's routes name no permission.
func (e *Env) appGates(route Route) []router.Middleware {
	a := e.Auth
	if IsNilAuth(a) {
		panic(MountError{Route: route.Key(), Reason: "a programmatic route needs Routes.Auth"})
	}
	pair := needs(route, a)
	if route.Auth == AuthApplication && pair == (Pair{}) {
		panic(MountError{Route: route.Key(), Reason: "a programmatic route declares no permission"})
	}
	return []router.Middleware{verify(route, a), e.appCheck(route, a, pair)}
}

func (e *Env) appCheck(route Route, a billingauth.Authenticator, pair Pair) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			v, c, target, ok := e.bound(r, route, a)
			if !ok {
				return
			}
			if c.SubjectKind != billingauth.SubjectApplication {
				refuseCode(r, route, a, billing.CodeApplicationRequired, "")
				return
			}
			if pair != (Pair{}) && !e.permitted(r, route, a, v, target.MerchantID, pair) {
				return
			}
			bindStaff(r, c, route, target)
			next(r)
		}
	}
}

type provisionerKey struct{}

// provisioningGate admits a SCIM request by the merchant's provisioning
// token, which opens only these routes and is bound to its row's merchant,
// or else as a programmatic route. A refusal before the SCIM server runs is
// answered as a SCIM error.
func (e *Env) provisioningGate(route Route) router.Middleware {
	gates := e.appGates(route)
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			var refusals *scimRefusals
			admitted := func(r *httprequest.Request) {
				refusals.passed = true
				next(r)
			}
			asApp := router.Handler(admitted)
			for i := len(gates) - 1; i >= 0; i-- {
				asApp = gates[i](asApp)
			}
			r.Through(func(h http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					refusals = &scimRefusals{ResponseWriter: w}
					h.ServeHTTP(refusals, req)
					refusals.flush()
				})
			}, func(r *httprequest.Request) {
				v, ok := e.provisioner(r, route)
				if !ok {
					asApp(r)
					return
				}
				target, err := e.boundMerchant(r.Request.Context(), nil, v, v.Identity())
				if err != nil {
					refuse(r, route, e.Auth, stepCan, err)
					return
				}
				pin(r, target)
				r.Request = r.Request.WithContext(context.WithValue(r.Request.Context(), provisionerKey{}, target.MerchantID))
				admitted(r)
			})
		}
	}
}

// provisioningToken is a merchant's provisioning token as a Verified: bound
// to the merchant of its row.
type provisioningToken struct{ billingauth.MerchantBinding }

// Identity names no subject: the token opens the SCIM routes alone.
func (provisioningToken) Identity() billingauth.Identity { return billingauth.Identity{} }

// provisioner is the provisioning token r presents, when it is one.
func (e *Env) provisioner(r *httprequest.Request, route Route) (provisioningToken, bool) {
	token := scim.BearerToken(r.Request)
	if token == "" || e.Runtime == nil || e.Runtime.DB == nil {
		return provisioningToken{}, false
	}
	mid, err := scim.Tokens{DB: e.Runtime.DB}.Resolve(r.Request.Context(), token)
	if err != nil {
		if !errors.Is(err, scim.ErrUnauthorized) {
			log.WithError(err).WithField("route", route.Key()).Error("openrails: provisioning token lookup failed")
		}
		return provisioningToken{}, false
	}
	return provisioningToken{billingauth.BindsMerchant(billingauth.Target{MerchantID: mid})}, true
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

// signedInGates gates the access read: any person or application, at the
// merchant its credential is bound to.
func (e *Env) signedInGates(route Route) []router.Middleware {
	a := e.Auth
	if IsNilAuth(a) {
		panic(MountError{Route: route.Key(), Reason: "a staff route needs Routes.Auth"})
	}
	needs(route, a)
	return []router.Middleware{verify(route, a), func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			if _, c, target, ok := e.bound(r, route, a); ok {
				bindStaff(r, c, route, target)
				next(r)
			}
		}
	}}
}

// adminAccess answers what the caller may use of each mounted staff group,
// asking the Verified's Can for each group's permission, at the merchant or
// in the root scope: what a console shows its user. A group counts only
// where it is mounted and the caller holds it.
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
		root, err := rootScope(ctx, e.Auth)
		if err != nil {
			refuse(r, route, e.Auth, stepCan, err)
			return
		}
		var failed error
		has := func(on bool, pair Pair) bool {
			if !on || failed != nil {
				return false
			}
			ok, err := holds(ctx, v, root, staff.Merchant, asked(e.Auth, pair))
			if err != nil {
				failed = err
			}
			return ok && err == nil
		}
		level := func(read, update bool) billing.AccessLevel {
			switch {
			case read && update:
				return billing.AccessUpdate
			case read:
				return billing.AccessRead
			}
			return billing.AccessNone
		}
		g := e.Groups
		support := has(g.Admin, needAdminRead)
		access := billing.AdminAccess{
			Admin:                  level(support, support && has(g.Admin, needAdminUpdate)),
			Catalog:                level(has(g.Catalog, Pair{billing.MerchantCatalogRead, billing.RootCatalogRead}), has(g.Catalog, needCatalog)),
			MerchantConfig:         level(has(g.MerchantConfig, needMerchantConfigRead), has(g.MerchantConfig, needMerchantConfig)),
			Metrics:                has(g.Metrics, needMetrics),
			ScopeID:                merchantScope(root, staff.Merchant).ID,
			ApplicationPermissions: []billing.Permission{},
		}
		if staff.Identity.SubjectKind == billingauth.SubjectApplication {
			for _, pair := range e.applicationPermissions {
				if has(true, pair) {
					access.ApplicationPermissions = append(access.ApplicationPermissions, pair.Merchant)
				}
			}
			slices.Sort(access.ApplicationPermissions)
		}
		llm := e.Runtime.Config.LLM
		access.Assistants = billing.AdminAssistants{
			CatalogCopilot:  access.Catalog != billing.AccessNone && config.LLMCatalogCopilotConfigured(llm),
			CatalogDrafting: access.Catalog != billing.AccessNone && config.LLMCatalogDraftingConfigured(llm),
		}
		if failed != nil {
			refuse(r, route, e.Auth, stepCan, failed)
			return
		}
		r.SuccessJSON(access)
	}
}
