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

	// AdminPermissionChecker is the LIVE authority for admin routes (#312/#537):
	// admin routes require merchant-local `org:` authority evaluated live against
	// the CALLER'S OWN org. nil for embedded hosts without a control plane, in
	// which case admin routes fail closed (there is no operator-org or
	// role-claim fallback).
	AdminPermissionChecker authpolicy.AdminPermissionChecker

	// ServiceCredentialResolver validates merchant-scoped programmatic
	// credentials: generated API keys, remote-application self tokens, and service
	// JWTs. The control plane satisfies this in standalone mode.
	ServiceCredentialResolver ServiceCredentialResolver
}

type ServiceCredentialResolver interface {
	LooksLikeAPIKey(token string) bool
	ResolveAPIKey(ctx context.Context, token string) (*controlplane.ResolvedServiceCredential, error)
}

type serviceJWTResolver interface {
	ResolveServiceJWT(ctx context.Context, token string) (*controlplane.ResolvedServiceCredential, error)
}

type remoteApplicationResolver interface {
	ResolveRemoteApplication(ctx context.Context, token string) (*controlplane.ResolvedServiceCredential, error)
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

	// Pin a merchant-scoped DB connection for the request (merchant resolved by the
	// global ResolveMerchant middleware) so RLS constrains merchant-owned queries
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
}

// RegisterServiceRoutes mounts the server-to-server billing surface on a
// merchant-scoped route group authenticated by OpenRails-issued merchant API
// keys or service JWTs.
func RegisterServiceRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	group := rr.Group("", opts.serviceCredentialRequiredMW())
	if rt != nil && rt.DB != nil {
		group = group.Group("", middleware.MerchantDBConnMW(rt.DB))
	}

	group.Handle(http.MethodPost, "/customers/by-external-subject/entitlements",
		h(httphandlers.ServiceGetExternalSubjectEntitlements),
		opts.servicePermissionMW(controlplane.PermEntitlementsRead),
	)

	customers := group.Group("/customers/:customer_id")
	customers.Handle(http.MethodGet, "/entitlements",
		h(httphandlers.ServiceGetCustomerEntitlements),
		opts.servicePermissionMW(controlplane.PermEntitlementsRead),
	)

	entitlements := group.Group("/entitlements")
	entitlements.Handle(http.MethodGet, "/:entitlement/customers",
		h(httphandlers.ServiceGetCustomersWithEntitlement),
		opts.servicePermissionMW(controlplane.PermEntitlementsRead),
	)

	users := group.Group("/users/:user_id")
	users.Handle(http.MethodGet, "/product-access",
		h(httphandlers.ServiceGetUserProductAccess),
		opts.servicePermissionMW(controlplane.PermEntitlementsRead),
	)

	invokers := group.Group("/invokers/:invoker")
	invokers.Handle(http.MethodGet, "/credits",
		h(httphandlers.ServiceGetInvokerCredits),
		opts.servicePermissionMW(controlplane.PermCreditsRead),
	)

	credits := group.Group("/credits")
	creditsSpend := opts.servicePermissionMW(controlplane.PermCreditsSpend)
	creditsWrite := opts.servicePermissionMW(controlplane.PermCreditsWrite)
	creditsRead := opts.servicePermissionMW(controlplane.PermCreditsRead)

	group.Handle(http.MethodPost, "/admit", h(httphandlers.ServiceAdmit), creditsWrite, creditsSpend)
	group.Handle(http.MethodPost, "/admit/batch", h(httphandlers.ServiceAdmitBatch), creditsWrite, creditsSpend)
	group.Handle(http.MethodGet, "/budget", h(httphandlers.ServiceGetBudget), creditsRead)
	group.Handle(http.MethodPut, "/payer-spend-limits", h(httphandlers.ServiceSetPayerSpendLimits), creditsWrite)
	group.Handle(http.MethodPut, "/tier-schedules", h(httphandlers.ServiceSetTierSchedule), creditsWrite)
	group.Handle(http.MethodGet, "/merchant-configuration",
		h(httphandlers.ServiceGetMerchantConfiguration),
		opts.servicePermissionMW(controlplane.PermMerchantConfigurationRead),
	)
	group.Handle(http.MethodPut, "/merchant-configuration",
		h(httphandlers.ServiceSetMerchantConfiguration),
		opts.servicePermissionMW(controlplane.PermMerchantConfigurationWrite),
	)
	group.Handle(http.MethodGet, "/trust-tier", h(httphandlers.ServiceGetTier), creditsRead)
	group.Handle(http.MethodGet, "/tier", h(httphandlers.ServiceGetTier), creditsRead)
	group.Handle(http.MethodPost, "/wasted-spend", h(httphandlers.ServiceReportWastedSpend), creditsWrite)
	group.Handle(http.MethodGet, "/abuse-usage", h(httphandlers.ServiceAbuseUsage), creditsRead)
	group.Handle(http.MethodPut, "/credit-limit", h(httphandlers.ServiceSetCreditLimit), creditsWrite)
	group.Handle(http.MethodGet, "/credit-limit", h(httphandlers.ServiceGetCreditLimit), creditsRead)
	group.Handle(http.MethodPut, "/invoker-spend-limits", h(httphandlers.ServiceSetInvokerSpendLimits), creditsWrite)
	group.Handle(http.MethodGet, "/invoker-spend-limits", h(httphandlers.ServiceGetInvokerSpendLimits), creditsRead)

	credits.Handle(http.MethodGet, "/balance", h(httphandlers.ServiceGetCreditsBalance), creditsRead)
	credits.Handle(http.MethodPost, "/deposit", h(httphandlers.ServiceDepositCredits), creditsWrite)
	credits.Handle(http.MethodPost, "/withdraw", h(httphandlers.ServiceWithdrawCredits), creditsWrite)
	credits.Handle(http.MethodPost, "/holds/:id/capture", h(httphandlers.ServiceCaptureHold), creditsWrite, creditsSpend)
	credits.Handle(http.MethodPost, "/holds/:id/release", h(httphandlers.ServiceReleaseHold), creditsWrite)
	credits.Handle(http.MethodPost, "/usage/rollup", h(httphandlers.ServiceUsageRollup), creditsRead)
	credits.Handle(http.MethodPost, "/usage/resource-revenue", h(httphandlers.ServiceResourceRevenue), creditsRead)
	credits.Handle(http.MethodGet, "/transactions/lookup", h(httphandlers.ServiceLookupCreditTransaction), creditsRead)
	credits.Handle(http.MethodPut, "/account-settings", h(httphandlers.ServiceSetCreditAccountSettings), creditsWrite)
	credits.Handle(http.MethodGet, "/account-settings", h(httphandlers.ServiceGetCreditAccountSettings), creditsRead)
	credits.Handle(http.MethodGet, "/transactions", h(httphandlers.ServiceListCustomerCreditTransactions), creditsRead)
}

func (opts Options) serviceCredentialRequiredMW() router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			if opts.ServiceCredentialResolver == nil {
				r.AbortJSON(http.StatusInternalServerError, "API key authentication not configured")
				return
			}
			token := ""
			if r != nil && r.Request != nil {
				token = bearerToken(r.Request.Header.Get("Authorization"))
			}
			if token == "" {
				r.AbortJSON(http.StatusUnauthorized, "API key bearer token required")
				return
			}
			resolved, handled := opts.resolveServiceCredential(r)
			if handled && resolved == nil {
				return
			}
			if resolved == nil {
				r.AbortJSON(http.StatusUnauthorized, "service_credential_invalid")
				return
			}
			if r.Request != nil {
				r.Request = r.Request.WithContext(merchant.WithID(r.Request.Context(), resolved.MerchantID))
			}
			r.Set("openrails.merchant_id", resolved.MerchantID)
			r.Set("openrails.service_credential", resolved)
			r.Set("openrails.service_credential_authkit_org_slug", resolved.OwnerOrgSlug)
			next(r)
		}
	}
}

func (opts Options) servicePermissionMW(perm string) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			resolved, ok := serviceCredentialFromRequest(r)
			if !ok {
				r.AbortJSON(http.StatusUnauthorized, "bearer principal required")
				return
			}
			if !resolved.HasPermission(perm) {
				r.AbortJSON(http.StatusForbidden, "permission_required")
				return
			}
			next(r)
		}
	}
}

func serviceCredentialFromRequest(r *httprequest.Request) (*controlplane.ResolvedServiceCredential, bool) {
	if r == nil {
		return nil, false
	}
	v, ok := r.Get("openrails.service_credential")
	if !ok {
		return nil, false
	}
	resolved, ok := v.(*controlplane.ResolvedServiceCredential)
	return resolved, ok && resolved != nil
}

// #528: the per-user `/v1/admin` surface (RegisterAdminRoutes) was retired. The
// admin API is the delegated issuer-owner surface (ginroutes.RegisterAdminRoutes,
// browser-safe `org:` permissions). The dropped features (provider-intents, reconcile,
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
				r.Set("openrails.service_credential", resolved)
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

func (opts Options) resolveServiceCredential(r *httprequest.Request) (*controlplane.ResolvedServiceCredential, bool) {
	resolver := opts.ServiceCredentialResolver
	if resolver == nil || r == nil || r.Request == nil {
		return nil, false
	}
	token := bearerToken(r.Request.Header.Get("Authorization"))
	if token == "" {
		return nil, false
	}
	if resolver.LooksLikeAPIKey(token) {
		resolved, err := resolver.ResolveAPIKey(r.Request.Context(), token)
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
		r.AbortJSON(http.StatusUnauthorized, "service_credential_expired")
	case errors.Is(err, authcore.ErrAccessTokenRevoked):
		r.AbortJSON(http.StatusUnauthorized, "service_credential_revoked")
	case errors.Is(err, controlplane.ErrServiceCredentialMerchantUnresolved):
		r.AbortJSON(http.StatusForbidden, "service_credential_merchant_unresolved")
	case errors.Is(err, controlplane.ErrServiceCredentialScopeDenied):
		r.AbortJSON(http.StatusForbidden, "service_credential_resource_scope_denied")
	default:
		r.AbortJSON(http.StatusUnauthorized, "service_credential_invalid")
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
	// INTENTIONAL ALIAS (#534): /drift/reconcile-all is the spec-named on-demand
	// synchronous pull and deliberately maps to the SAME AdminRefreshCatalogDrift
	// handler as /drift/refresh — it is NOT a duplicate to reconcile away. If you
	// ever split them, keep them permission-symmetric.
	catalog.Handle(http.MethodPost, "/drift/reconcile-all", h(httphandlers.AdminRefreshCatalogDrift))
	// /orphans is provider-filterable (?provider=stripe|nmi); /stripe/orphans is
	// the operator-friendly convenience alias scoped to Stripe.
	catalog.Handle(http.MethodGet, "/orphans", h(httphandlers.AdminListCatalogOrphans))
	catalog.Handle(http.MethodGet, "/stripe/orphans", h(httphandlers.AdminListStripeOrphans))
}

// RegisterWebhookRoutes mounts the legacy configured-merchant webhook surface.
// Standalone and embedded defaults do not call this; hosts should prefer
// RegisterMerchantWebhookRoutes or RegisterHostWebhookRoutes.
func RegisterWebhookRoutes(rr router.Router, rt *app.Runtime) {
	rr.Handle(http.MethodPost, "/:provider", h(httphandlers.Webhook))
}

// RegisterHostWebhookRoutes mounts POST /:provider for embedding hosts that
// resolve Host -> merchant in middleware before OpenRails verifies the webhook.
func RegisterHostWebhookRoutes(rr router.Router, rt *app.Runtime) {
	rr.Handle(http.MethodPost, "/:provider", h(httphandlers.HostWebhook))
}

// RegisterMerchantWebhookRoutes mounts the CANONICAL merchant-scoped webhook surface
// (POST /merchants/:merchant/webhooks/:provider, issue #529) — the active inbound
// webhook surface for every deployment. One handler (httphandlers.MerchantWebhook,
// Stripe + NMI-backed processors + CCBill) is shared by both the standalone gin server and the
// embedded mux, so the two cannot drift.
func RegisterMerchantWebhookRoutes(rr router.Router, rt *app.Runtime) {
	rr.Handle(http.MethodPost, "/merchants/:merchant/webhooks/:provider", h(httphandlers.MerchantWebhook))
}
