package routes

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/customerscope"
	"github.com/open-rails/openrails/internal/db/gen"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/merchant"
)

// RegisterMappedCustomerRoutes reuses the explicitly classified customer
// operations. New operations are not exposed with an implicit sensitivity.
func RegisterMappedCustomerRoutes(rr router.Router, rt *app.Runtime, m CustomerMount, resolve func(*http.Request, auth.Verified) (config.CustomerBinding, error)) {
	env := customerEnv(rt, m)
	env.resolveCustomer = resolve
	for _, route := range Catalog() {
		sensitive, allowed := mappedCustomerOperation(route.Key())
		if !allowed || !env.mounts(route) {
			continue
		}
		route.Sensitive = sensitive
		rr.Handle(route.Method, strings.TrimPrefix(route.Path, "/v1/me"), env.Guarded(route), env.gates(route)...)
	}
}

func (e *Env) mappedCustomerCheck(route Route, a auth.Authenticator) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			v, identity, ok := subject(r, route, a)
			if !ok {
				return
			}
			if identity.SubjectKind != auth.SubjectUser || !identity.SelfInvoked() || !billingauth.Interactive(identity) || identity.Credential.Kind == auth.CredentialPersonalAccessToken {
				refuseCode(r, route, a, billing.CodeInvokerScopedPrincipal, "")
				return
			}
			binding, err := e.resolveMappedCustomer(r.Request, v)
			if err != nil {
				switch {
				case errors.Is(err, billing.ErrNotFound):
					refuseCode(r, route, a, billing.CodeResourceNotFound, "")
				case errors.Is(err, billing.ErrMerchantUnresolved):
					refuseCode(r, route, a, billing.CodeMerchantUnresolved, "")
				default:
					refuse(r, route, a, stepAuthenticate, err)
				}
				return
			}
			if binding.MerchantID.IsZero() || binding.CustomerID.IsZero() {
				refuse(r, route, a, stepCan, fault("customer resolver returned an empty binding"))
				return
			}
			ctx := r.Request.Context()
			live, err := e.live(ctx, binding.MerchantID)
			if err != nil {
				refuse(r, route, a, stepCan, errors.Join(auth.ErrUnavailable, err))
				return
			}
			if !live {
				refuseCode(r, route, a, billing.CodeMerchantUnresolved, "")
				return
			}
			// Existence is checked by the billing owner, without DTO network
			// enrichment, customer creation or issuer/tenant reassignment.
			_, err = e.Runtime.DB.GenDirectory().GetCustomer(ctx, gen.GetCustomerParams{MerchantID: binding.MerchantID.UUID(), ID: binding.CustomerID.UUID()})
			if errors.Is(err, pgx.ErrNoRows) {
				refuseCode(r, route, a, billing.CodeResourceNotFound, "")
				return
			}
			if err != nil {
				refuse(r, route, a, stepCan, errors.Join(auth.ErrUnavailable, err))
				return
			}
			if !expectedMerchant(r, binding.MerchantID) {
				return
			}
			if route.Sensitive {
				if err := recentSignIn(ctx, v); err != nil {
					refuse(r, route, a, stepRecentSignIn, err)
					return
				}
			}
			pin(r, billingauth.Target{MerchantID: binding.MerchantID})
			ctx = billingauth.BindIdentity(r.Request.Context(), identity)
			// The original Verified/Identity remain intact. The trusted mapping
			// separately proves that this person acts as the billing customer.
			r.Request = r.Request.WithContext(customerscope.Bind(ctx, binding.MerchantID, binding.CustomerID, binding.CustomerID.String(), true))
			next(r)
		}
	}
}

func (e *Env) resolveMappedCustomer(r *http.Request, v auth.Verified) (binding config.CustomerBinding, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			binding, err = config.CustomerBinding{}, fault("customer resolver panicked")
		}
	}()
	return e.resolveCustomer(r, v)
}

func expectedMerchant(r *httprequest.Request, id billing.MerchantID) bool {
	if values, present := r.Request.Header[http.CanonicalHeaderKey(merchant.ExpectedHeader)]; present {
		var expected billing.MerchantID
		var err error
		if len(values) == 1 {
			expected, err = billing.ParseMerchantID(strings.TrimSpace(values[0]))
		}
		if len(values) != 1 || err != nil || expected.IsZero() {
			r.AbortCode(billing.CodeInvalidParam, merchant.ExpectedHeader+" must contain one nonzero merchant UUID")
			return false
		}
		if expected != id {
			r.AbortCode(billing.CodeMerchantMismatch, "The credential is bound to a different merchant; the operation was not executed.")
			return false
		}
	}
	return true
}
