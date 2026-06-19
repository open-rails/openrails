package routes

import (
	"context"
	"errors"
	"net/http"
	"strings"

	authcore "github.com/open-rails/authkit/core"
	"github.com/open-rails/openrails/internal/app"
	authpolicy "github.com/open-rails/openrails/internal/auth/policy"
	"github.com/open-rails/openrails/internal/controlplane"
	httphandlers "github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

type Options struct {
	// Authenticator is the framework-neutral auth boundary used to build the
	// neutral Required/Optional middleware for these routes (issue #282/#285).
	Authenticator billingauth.Authenticator

	// AdminPermissionChecker is the LIVE authority for admin routes (#312): admin
	// routes require the openrails:admin permission evaluated live against the
	// CALLER'S OWN tenant. nil for embedded hosts without a control plane, in
	// which case admin routes fail closed (there is no operator-tenant or
	// role-claim fallback).
	AdminPermissionChecker authpolicy.AdminPermissionChecker

	// ServiceCredentialResolver validates merchant-scoped programmatic
	// credentials: generated service tokens, remote-application self tokens, and
	// service JWTs. The control plane satisfies this in standalone mode.
	ServiceCredentialResolver ServiceCredentialResolver
}

type ServiceCredentialResolver interface {
	LooksLikeServiceToken(token string) bool
	ResolveServiceToken(ctx context.Context, token string) (*controlplane.ResolvedServiceToken, error)
}

type serviceJWTResolver interface {
	ResolveServiceJWT(ctx context.Context, token string) (*controlplane.ResolvedServiceToken, error)
}

type remoteApplicationResolver interface {
	ResolveRemoteApplication(ctx context.Context, token string) (*controlplane.ResolvedServiceToken, error)
}

// requiredMW builds the neutral "authentication required" middleware for the
// embedded surface. It authenticates via opts.Authenticator, aborts 401 on
// failure, and pins the resulting UserContext on the request context (the single
// contract handlers + the operator gates read via FromContext).
func (opts Options) requiredMW() router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			a := opts.Authenticator
			if a == nil {
				r.AbortJSON(http.StatusInternalServerError, "authentication disabled")
				return
			}
			uc, err := a.Authenticate(r.Request.Context(), r.Request)
			if err != nil {
				r.AbortJSON(http.StatusUnauthorized, billingauth.UnauthenticatedMessage(err))
				return
			}
			if verr := uc.ValidateSubject(); verr != nil {
				r.AbortJSON(http.StatusUnauthorized, verr.Error())
				return
			}
			r.SetUserContext(uc)
			next(r)
		}
	}
}

// optionalMW builds the neutral best-effort auth middleware: it attempts
// authentication and pins the UserContext when it succeeds, but never aborts.
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

// h adapts a handler func to the neutral router.Handler type.
func h(fn func(r *httprequest.Request)) router.Handler {
	return router.Handler(fn)
}

func RegisterUserRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	required := opts.requiredMW()
	optional := opts.optionalMW()

	// Pin a tenant-scoped DB connection for the request (tenant resolved by the
	// global ResolveMerchant middleware) so RLS constrains tenant-owned queries
	// (issue #227). It applies to every route in this group.
	var group router.Router = rr
	if rt != nil && rt.DB != nil {
		group = rr.Group("", middleware.MerchantDBConnMW(rt.DB))
	}

	group.Handle(http.MethodGet, "/products", h(httphandlers.GetProducts), optional)
	group.Handle(http.MethodGet, "/prices", h(httphandlers.GetPrices), optional)
	group.Handle(http.MethodGet, "/solana/config", h(httphandlers.GetSolanaConfig))
	group.Handle(http.MethodGet, "/solana/tokens", h(httphandlers.GetSupportedTokens))

	checkout := group.Group("/checkout", required)
	checkout.Handle(http.MethodPost, "", h(httphandlers.CreateCheckoutSession))
	checkout.Handle(http.MethodGet, "/:id", h(httphandlers.GetCheckoutSession))
	checkout.Handle(http.MethodPost, "/:id/confirm", h(httphandlers.ConfirmCheckoutSession))

	group.Handle(http.MethodGet, "/checkout/:id/solana-pay", h(httphandlers.GetSolanaPay))
	group.Handle(http.MethodPost, "/checkout/:id/solana-pay", h(httphandlers.PostSolanaPay))

	// Solana recurring enrollment (#255): the wallet signs subscribe client-side,
	// then confirms here to charge the first cycle + create the membership.
	group.Handle(http.MethodPost, "/solana/recurring/enroll", h(httphandlers.ConfirmSolanaEnrollment))

	me := group.Group("/me", required)
	me.Handle(http.MethodGet, "/status", h(httphandlers.GetMyBillingStatus))
	me.Handle(http.MethodGet, "/subscriptions", h(httphandlers.GetMySubscriptions))
	me.Handle(http.MethodGet, "/subscriptions/:id", h(httphandlers.GetSubscription))
	me.Handle(http.MethodPut, "/subscriptions/:id/payment-method", h(httphandlers.UpdateSubscriptionPaymentMethod))
	me.Handle(http.MethodPost, "/subscriptions/:id/cancel", h(httphandlers.CancelSubscription))
	me.Handle(http.MethodPost, "/subscriptions/:id/resume", h(httphandlers.ResumeSubscription))
	me.Handle(http.MethodPost, "/subscriptions/:id/change-tier", h(httphandlers.ChangeTier))
	me.Handle(http.MethodPost, "/subscriptions/:id/change-tier/preview", h(httphandlers.ChangeTierPreview))
	me.Handle(http.MethodGet, "/payments", h(httphandlers.GetUserPayments))
	me.Handle(http.MethodGet, "/payment-methods", h(httphandlers.ListPaymentMethods))
	me.Handle(http.MethodPost, "/payment-methods", h(httphandlers.CreatePaymentMethod))
	me.Handle(http.MethodPut, "/payment-methods/:id", h(httphandlers.UpdatePaymentMethod))
	me.Handle(http.MethodDelete, "/payment-methods/:id", h(httphandlers.DeletePaymentMethod))
	me.Handle(http.MethodGet, "/notifications", h(httphandlers.GetNotifications))
	me.Handle(http.MethodGet, "/notifications/unread-count", h(httphandlers.GetUnreadNotificationCount))
	me.Handle(http.MethodPost, "/notifications/:id/read", h(httphandlers.MarkNotificationRead))
	me.Handle(http.MethodGet, "/credits", h(httphandlers.GetMyCredits))
	me.Handle(http.MethodGet, "/credits/:currency", h(httphandlers.GetMyCreditsType))
	me.Handle(http.MethodGet, "/credits/:currency/transactions", h(httphandlers.GetMyCreditTransactions))
	me.Handle(http.MethodGet, "/products", h(httphandlers.GetMyProducts))
	me.Handle(http.MethodGet, "/products/:product_id/access", h(httphandlers.GetMyProductAccess))

	stripe := group.Group("/stripe", required)
	stripe.Handle(http.MethodPost, "/portal", h(httphandlers.CreatePortalSession))
}

// #528: the per-user `/v1/admin` surface (RegisterAdminRoutes) was retired. The
// admin API is the delegated issuer→owner surface (ginroutes.RegisterAdminRoutes,
// `openrails:merchant:*`). The dropped features (provider-intents, reconcile,
// solana recurring plans, entitlement-features) live only on the dead surface and
// are removed; product-access moves to the delegated surface (#528 increment 4).

func RegisterMerchantActionRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	mw := []router.Middleware{
		opts.merchantActionPermissionMW(authpolicy.PermCatalogWrite),
	}
	if rt != nil && rt.DB != nil {
		mw = append(mw, middleware.MerchantDBConnMW(rt.DB))
	}

	group := rr.Group("", mw...)
	registerCatalogActionRoutes(group.Group("/catalog"))
}

func (opts Options) merchantActionPermissionMW(perm string) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			if resolved, handled := opts.resolveServiceCredential(r); handled {
				if resolved == nil {
					return
				}
				if !resolved.HasPermission(perm) {
					r.AbortJSON(http.StatusForbidden, "permission_required")
					return
				}
				if r.Request != nil {
					r.Request = r.Request.WithContext(merchant.WithID(r.Request.Context(), resolved.MerchantID))
				}
				r.Set("openrails.service_token", resolved)
				next(r)
				return
			}

			uc, ok := opts.authenticateUser(r)
			if !ok {
				return
			}
			if opts.AdminPermissionChecker == nil {
				r.AbortJSON(http.StatusInternalServerError, "authorization unavailable")
				return
			}
			allowed, err := opts.AdminPermissionChecker.HasAdminPermission(r.Request.Context(), uc.Org, uc.UserID, perm)
			if err != nil {
				r.AbortJSON(http.StatusInternalServerError, "failed to check permission")
				return
			}
			if !allowed {
				r.AbortJSON(http.StatusForbidden, "permission_required")
				return
			}
			next(r)
		}
	}
}

func (opts Options) authenticateUser(r *httprequest.Request) (billingauth.UserContext, bool) {
	a := opts.Authenticator
	if a == nil {
		r.AbortJSON(http.StatusInternalServerError, "authentication disabled")
		return billingauth.UserContext{}, false
	}
	uc, err := a.Authenticate(r.Request.Context(), r.Request)
	if err != nil {
		r.AbortJSON(http.StatusUnauthorized, billingauth.UnauthenticatedMessage(err))
		return billingauth.UserContext{}, false
	}
	if verr := uc.ValidateSubject(); verr != nil {
		r.AbortJSON(http.StatusUnauthorized, verr.Error())
		return billingauth.UserContext{}, false
	}
	r.SetUserContext(uc)
	return uc, true
}

func (opts Options) resolveServiceCredential(r *httprequest.Request) (*controlplane.ResolvedServiceToken, bool) {
	resolver := opts.ServiceCredentialResolver
	if resolver == nil || r == nil || r.Request == nil {
		return nil, false
	}
	token := bearerToken(r.Request.Header.Get("Authorization"))
	if token == "" {
		return nil, false
	}
	if resolver.LooksLikeServiceToken(token) {
		resolved, err := resolver.ResolveServiceToken(r.Request.Context(), token)
		if err != nil {
			writeServiceCredentialError(r, err)
			return nil, true
		}
		return resolved, true
	}
	if !controlplane.LooksLikeJWT(token) {
		return nil, false
	}
	if raResolver, ok := resolver.(remoteApplicationResolver); ok {
		resolved, err := raResolver.ResolveRemoteApplication(r.Request.Context(), token)
		if err == nil {
			return resolved, true
		}
		if !errors.Is(err, controlplane.ErrNotRemoteApplicationToken) {
			writeServiceCredentialError(r, err)
			return nil, true
		}
	}
	if jwtResolver, ok := resolver.(serviceJWTResolver); ok {
		resolved, err := jwtResolver.ResolveServiceJWT(r.Request.Context(), token)
		if err == nil {
			return resolved, true
		}
	}
	return nil, false
}

func bearerToken(header string) string {
	header = strings.TrimSpace(header)
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

func writeServiceCredentialError(r *httprequest.Request, err error) {
	switch {
	case errors.Is(err, authcore.ErrAccessTokenExpired):
		r.AbortJSON(http.StatusUnauthorized, "service_token_expired")
	case errors.Is(err, authcore.ErrAccessTokenRevoked):
		r.AbortJSON(http.StatusUnauthorized, "service_token_revoked")
	case errors.Is(err, controlplane.ErrServiceTokenMerchantUnresolved):
		r.AbortJSON(http.StatusForbidden, "service_token_merchant_unresolved")
	case errors.Is(err, controlplane.ErrServiceTokenScopeDenied):
		r.AbortJSON(http.StatusForbidden, "service_token_resource_scope_denied")
	default:
		r.AbortJSON(http.StatusUnauthorized, "service_token_invalid")
	}
}

func registerCatalogActionRoutes(catalog router.Router) {
	products := catalog.Group("/products")
	products.Handle(http.MethodPost, "", h(httphandlers.AdminCreateProduct))
	products.Handle(http.MethodGet, "", h(httphandlers.AdminListProducts))
	products.Handle(http.MethodGet, "/:id", h(httphandlers.AdminGetProduct))
	products.Handle(http.MethodGet, "/by-slug/:slug", h(httphandlers.AdminGetProductBySlug))
	products.Handle(http.MethodPatch, "/:id", h(httphandlers.AdminUpdateProduct))
	products.Handle(http.MethodPost, "/:id/activate", h(httphandlers.AdminActivateProduct))
	products.Handle(http.MethodPost, "/:id/deactivate", h(httphandlers.AdminDeactivateProduct))
	products.Handle(http.MethodPost, "/:id/reconcile", h(httphandlers.AdminReconcileProduct))

	prices := catalog.Group("/prices")
	prices.Handle(http.MethodPost, "", h(httphandlers.AdminCreatePrice))
	prices.Handle(http.MethodGet, "", h(httphandlers.AdminListPrices))
	prices.Handle(http.MethodGet, "/:id", h(httphandlers.AdminGetPrice))
	prices.Handle(http.MethodPatch, "/:id", h(httphandlers.AdminUpdatePrice))
	prices.Handle(http.MethodPost, "/:id/activate", h(httphandlers.AdminActivatePrice))
	prices.Handle(http.MethodPost, "/:id/deactivate", h(httphandlers.AdminDeactivatePrice))
	prices.Handle(http.MethodPost, "/:id/reconcile", h(httphandlers.AdminReconcilePrice))

	// Catalog reconciliation loop drift surface (issue #209). Alert-only:
	// these endpoints never mutate Stripe, NMI, or the catalog rows. Operators
	// resolve drift via the per-price reconcile action above. CCBill is never
	// reconciled (no catalog-list API), so it never appears in these surfaces.
	catalog.Handle(http.MethodGet, "/drift", h(httphandlers.AdminListCatalogDrift))
	catalog.Handle(http.MethodPost, "/drift/refresh", h(httphandlers.AdminRefreshCatalogDrift))
	// reconcile-all is the spec-named alias for an on-demand synchronous pull.
	catalog.Handle(http.MethodPost, "/drift/reconcile-all", h(httphandlers.AdminRefreshCatalogDrift))
	// /orphans is provider-filterable (?provider=stripe|nmi); /stripe/orphans is
	// the operator-friendly convenience alias scoped to Stripe.
	catalog.Handle(http.MethodGet, "/orphans", h(httphandlers.AdminListCatalogOrphans))
	catalog.Handle(http.MethodGet, "/stripe/orphans", h(httphandlers.AdminListStripeOrphans))
}

func RegisterWebhookRoutes(rr router.Router, rt *app.Runtime) {
	rr.Handle(http.MethodPost, "/:provider", h(httphandlers.Webhook))
}
