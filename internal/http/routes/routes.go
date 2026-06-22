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

	// DelegatedResolver validates browser-direct delegated access tokens — a
	// merchant's federated merchant-signed JWT for one of its admin users
	// (#259). The control plane satisfies it in standalone mode; nil disables the
	// delegated path on merchant routes (#555/#564). Permissions are bounded by
	// AuthKit to the signing remote application's stored authority.
	DelegatedResolver DelegatedResolver

	// DelegatedAuthenticator is the IN-PROCESS host identity seam (#565): when
	// OpenRails runs as a subsystem of a host application (embedded, no control
	// plane), the host verifies its OWN credential and returns the explicitly
	// mapped principal. In-process => the host is TRUSTED and its supplied
	// permissions are authoritative — the same trust the gin self surface already
	// grants via DelegatedPrincipalRequired. nil disables this path (standalone
	// hosts use the control-plane resolvers above instead).
	DelegatedAuthenticator billingauth.DelegatedAuthenticator
}

type ServiceCredentialResolver interface {
	LooksLikeAPIKey(token string) bool
	ResolveAPIKey(ctx context.Context, token string) (*controlplane.ResolvedServiceCredential, error)
}

// DelegatedResolver validates a browser-direct delegated access token and
// resolves its merchant + acting user (#259/#555).
type DelegatedResolver interface {
	ResolveDelegated(ctx context.Context, token string, origin string) (*controlplane.ResolvedDelegated, error)
}

type serviceJWTResolver interface {
	ResolveServiceJWT(ctx context.Context, token string) (*controlplane.ResolvedServiceCredential, error)
}

type remoteApplicationResolver interface {
	ResolveRemoteApplication(ctx context.Context, token string) (*controlplane.ResolvedServiceCredential, error)
}

type merchantOrgResolver interface {
	ResolveMerchantForOrg(ctx context.Context, orgSlug string) (merchant.ID, string, error)
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

// RegisterServiceRoutes mounts the merchant billing surface. Access is gated by
// merchant permissions, not credential type (#564).
func RegisterServiceRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	group := rr
	var dbMW []router.Middleware
	if rt != nil && rt.DB != nil {
		dbMW = append(dbMW, middleware.MerchantDBConnMW(rt.DB))
	}
	readMW := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantCustomerSettingsRead)}, dbMW...)
	writeMW := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantCustomerSettingsUpdate)}, dbMW...)
	admissionMW := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantAdmissionsCreate)}, dbMW...)
	settingsReadMW := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantSettingsRead)}, dbMW...)
	settingsWriteMW := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantSettingsUpdate)}, dbMW...)
	usageReadMW := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantUsageRead)}, dbMW...)

	group.Handle(http.MethodPost, "/customers/entitlements:batch",
		h(httphandlers.ServiceGetExternalSubjectEntitlements),
		readMW...,
	)

	customers := group.Group("/customers/:customer_id")
	customers.Handle(http.MethodGet, "/entitlements",
		h(httphandlers.ServiceGetCustomerEntitlements),
		readMW...,
	)

	entitlements := group.Group("/entitlements")
	entitlements.Handle(http.MethodGet, "/:entitlement/customers",
		h(httphandlers.ServiceGetCustomersWithEntitlement),
		readMW...,
	)

	users := group.Group("/users/:user_id")
	users.Handle(http.MethodGet, "/product-access",
		h(httphandlers.ServiceGetUserProductAccess),
		readMW...,
	)

	invokers := group.Group("/invokers/:invoker")
	invokers.Handle(http.MethodGet, "/credits",
		h(httphandlers.ServiceGetInvokerCredits),
		readMW...,
	)

	group.Handle(http.MethodPost, "/admissions", h(httphandlers.ServiceAdmitBatch), admissionMW...)
	group.Handle(http.MethodGet, "/settings", h(httphandlers.ServiceGetMerchantSettings), settingsReadMW...)
	group.Handle(http.MethodPut, "/settings", h(httphandlers.ServiceSetMerchantSettings), settingsWriteMW...)
	group.Handle(http.MethodGet, "/trust-tier", h(httphandlers.ServiceGetTier), readMW...)
	group.Handle(http.MethodPost, "/wasted-spend", h(httphandlers.ServiceReportWastedSpend), admissionMW...)
	group.Handle(http.MethodPut, "/credit-limit", h(httphandlers.ServiceSetCreditLimit), writeMW...)

	admissions := group.Group("/admissions")
	admissions.Handle(http.MethodPost, "/:id/capture", h(httphandlers.ServiceCaptureHold), admissionMW...)
	admissions.Handle(http.MethodPost, "/:id/release", h(httphandlers.ServiceReleaseHold), admissionMW...)

	usage := group.Group("/usage")
	usage.Handle(http.MethodPost, "/rollup", h(httphandlers.ServiceUsageRollup), usageReadMW...)
	usage.Handle(http.MethodPost, "/resource-revenue", h(httphandlers.ServiceResourceRevenue), usageReadMW...)

	credits := group.Group("/credits")
	credits.Handle(http.MethodGet, "/balance", h(httphandlers.ServiceGetCreditsBalance), readMW...)
	credits.Handle(http.MethodPost, "/deposit", h(httphandlers.ServiceDepositCredits), writeMW...)
}

func RegisterMerchantActionRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	var dbMW []router.Middleware
	if rt != nil && rt.DB != nil {
		dbMW = append(dbMW, middleware.MerchantDBConnMW(rt.DB))
	}
	registerMerchantSupportRoutes(rr, opts, dbMW...)
}

func RegisterMerchantSettingsRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	var dbMW []router.Middleware
	if rt != nil && rt.DB != nil {
		dbMW = append(dbMW, middleware.MerchantDBConnMW(rt.DB))
	}
	registerCatalogActionRoutes(rr.Group("/catalog"), opts, dbMW...)
	registerPaymentProviderActionRoutes(rr.Group("/payment-providers"), opts, dbMW...)
}

func (opts Options) merchantActionPermissionMW(perm string) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			if resolved, handled := opts.resolveServiceCredential(r, opts.Authenticator != nil); handled {
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
				r.Set(httphandlers.MerchantRoutePrincipalContextKey, true)
				next(r)
				return
			}

			// Browser-direct delegated access token (#564): a merchant admin's
			// remote-app-signed JWT. AuthKit has already bounded permissions to
			// the signer's stored authority, so gate purely on the permission; the
			// merchant is pinned from the verified token, never the URL.
			if opts.DelegatedResolver != nil && r.Request != nil {
				if token := bearerToken(r.Request.Header.Get("Authorization")); controlplane.LooksLikeJWT(token) {
					resolved, err := opts.DelegatedResolver.ResolveDelegated(r.Request.Context(), token, r.Request.Header.Get("Origin"))
					if err != nil {
						if opts.Authenticator == nil || !errors.Is(err, controlplane.ErrDelegatedInvalid) {
							r.AbortJSON(http.StatusUnauthorized, "delegated_token_invalid")
							return
						}
					} else {
						if !resolved.HasPermission(perm) {
							r.AbortJSON(http.StatusForbidden, "permission_required")
							return
						}
						r.Request = r.Request.WithContext(merchant.WithID(r.Request.Context(), resolved.MerchantID))
						r.Set("openrails.merchant_id", resolved.MerchantID)
						r.Set(httphandlers.MerchantRoutePrincipalContextKey, true)
						r.SetUserContext(billingauth.UserContext{
							UserID:        resolved.DelegatedSubject,
							Email:         resolved.Email,
							EmailVerified: resolved.EmailVerified,
							Username:      resolved.Username,
							Org:           resolved.Merchant,
						})
						next(r)
						return
					}
				}
			}

			// In-process host-delegated principal (#565): an embedded host's
			// DelegatedAuthenticator verifies its OWN credential and returns the
			// explicitly mapped principal. In-process => the host is trusted and its
			// permissions are authoritative (same trust as the gin self surface's
			// DelegatedPrincipalRequired); gate purely on permission, no control plane.
			if opts.DelegatedAuthenticator != nil && r.Request != nil {
				principal, err := opts.DelegatedAuthenticator.AuthenticateDelegated(r.Request.Context(), r.Request)
				if err != nil {
					r.AbortJSON(http.StatusUnauthorized, billingauth.UnauthenticatedMessage(err))
					return
				}
				resolved, verr := controlplane.ResolvedDelegatedFromHostPrincipal(principal)
				if verr != nil {
					r.AbortJSON(http.StatusUnauthorized, "delegated_principal_invalid")
					return
				}
				if !resolved.HasPermission(perm) {
					r.AbortJSON(http.StatusForbidden, "permission_required")
					return
				}
				r.Request = r.Request.WithContext(merchant.WithID(r.Request.Context(), resolved.MerchantID))
				r.Set("openrails.merchant_id", resolved.MerchantID)
				r.Set(httphandlers.MerchantRoutePrincipalContextKey, true)
				r.SetUserContext(billingauth.UserContext{
					UserID:        resolved.DelegatedSubject,
					Email:         resolved.Email,
					EmailVerified: resolved.EmailVerified,
					Username:      resolved.Username,
					Org:           resolved.Merchant,
				})
				next(r)
				return
			}

			if opts.Authenticator == nil {
				r.AbortJSON(http.StatusUnauthorized, "bearer principal required")
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
			if r.Request != nil {
				if _, ok := merchant.FromContext(r.Request.Context()); !ok {
					resolver, ok := opts.AdminPermissionChecker.(merchantOrgResolver)
					if !ok {
						r.AbortJSON(http.StatusInternalServerError, "merchant resolver unavailable")
						return
					}
					mid, _, err := resolver.ResolveMerchantForOrg(r.Request.Context(), uc.Org)
					if err != nil {
						r.AbortJSON(http.StatusForbidden, "merchant_unresolved")
						return
					}
					r.Request = r.Request.WithContext(merchant.WithID(r.Request.Context(), mid))
				}
			}
			r.Set(httphandlers.MerchantRoutePrincipalContextKey, true)
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

func (opts Options) resolveServiceCredential(r *httprequest.Request, allowJWTFallthrough bool) (*controlplane.ResolvedServiceCredential, bool) {
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
		if allowJWTFallthrough && errors.Is(err, controlplane.ErrDelegatedInvalid) {
			return nil, false
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
		// A VERIFIED service JWT that is definitively rejected (cross-merchant
		// resource scope, or its issuer owns no merchant) must surface as 403 — not
		// fall through to the delegated/user-session paths, which would mislabel it
		// 401 access_token_wrong_typ. A wrong-typ (not-a-service-JWT) error still
		// falls through so delegated/user tokens reach their own resolvers.
		if errors.Is(err, controlplane.ErrServiceCredentialScopeDenied) ||
			errors.Is(err, controlplane.ErrServiceCredentialMerchantUnresolved) {
			writeServiceCredentialError(r, err)
			return nil, true
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

func registerCatalogActionRoutes(catalog router.Router, opts Options, dbMW ...router.Middleware) {
	read := opts.merchantActionPermissionMW(controlplane.PermMerchantCatalogRead)
	write := opts.merchantActionPermissionMW(authpolicy.PermMerchantCatalogUpdate)
	readMW := append([]router.Middleware{read}, dbMW...)
	writeMW := append([]router.Middleware{write}, dbMW...)

	products := catalog.Group("/products")
	products.Handle(http.MethodPost, "", h(httphandlers.AdminCreateProduct), writeMW...)
	products.Handle(http.MethodGet, "", h(httphandlers.AdminListProducts), readMW...)
	products.Handle(http.MethodGet, "/:id", h(httphandlers.AdminGetProduct), readMW...)
	products.Handle(http.MethodGet, "/by-slug/:slug", h(httphandlers.AdminGetProductBySlug), readMW...)
	products.Handle(http.MethodPatch, "/:id", h(httphandlers.AdminUpdateProduct), writeMW...)
	products.Handle(http.MethodPost, "/:id/activate", h(httphandlers.AdminActivateProduct), writeMW...)
	products.Handle(http.MethodPost, "/:id/deactivate", h(httphandlers.AdminDeactivateProduct), writeMW...)

	prices := catalog.Group("/prices")
	prices.Handle(http.MethodPost, "", h(httphandlers.AdminCreatePrice), writeMW...)
	prices.Handle(http.MethodGet, "", h(httphandlers.AdminListPrices), readMW...)
	prices.Handle(http.MethodGet, "/:id", h(httphandlers.AdminGetPrice), readMW...)
	prices.Handle(http.MethodPatch, "/:id", h(httphandlers.AdminUpdatePrice), writeMW...)
	prices.Handle(http.MethodPost, "/:id/activate", h(httphandlers.AdminActivatePrice), writeMW...)
	prices.Handle(http.MethodPost, "/:id/deactivate", h(httphandlers.AdminDeactivatePrice), writeMW...)

	catalog.Handle(http.MethodGet, "/drift", h(httphandlers.AdminListCatalogDrift), readMW...)
	catalog.Handle(http.MethodPost, "/drift/refresh", h(httphandlers.AdminRefreshCatalogDrift), writeMW...)
	catalog.Handle(http.MethodPost, "/publish", h(httphandlers.MerchantPublishCatalog), writeMW...)
}

func registerPaymentProviderActionRoutes(providers router.Router, opts Options, dbMW ...router.Middleware) {
	read := opts.merchantActionPermissionMW(controlplane.PermMerchantPaymentProvidersRead)
	write := opts.merchantActionPermissionMW(controlplane.PermMerchantPaymentProvidersUpdate)
	readMW := append([]router.Middleware{read}, dbMW...)
	writeMW := append([]router.Middleware{write}, dbMW...)

	providers.Handle(http.MethodGet, "", h(httphandlers.MerchantListPaymentProviders), readMW...)
	providers.Handle(http.MethodGet, "/:provider", h(httphandlers.MerchantGetPaymentProvider), readMW...)
	providers.Handle(http.MethodPut, "/:provider", h(httphandlers.MerchantPutPaymentProvider), writeMW...)
	providers.Handle(http.MethodDelete, "/:provider", h(httphandlers.MerchantDeletePaymentProvider), writeMW...)
}

func registerMerchantSupportRoutes(rr router.Router, opts Options, dbMW ...router.Middleware) {
	customerRead := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantCustomerSettingsRead)}, dbMW...)
	customerWrite := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantCustomerSettingsUpdate)}, dbMW...)
	payRead := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantPaymentsRead)}, dbMW...)
	payRefund := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantPaymentsRefund)}, dbMW...)
	subRead := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantSubscriptionsRead)}, dbMW...)
	subWrite := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantSubscriptionsUpdate)}, dbMW...)
	usageRead := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantUsageRead)}, dbMW...)
	repairRead := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantRepairAlertsRead)}, dbMW...)

	customers := rr.Group("/customers/:customer_id")
	customers.Handle(http.MethodGet, "", h(httphandlers.GetAdminUserBillingProfile), customerRead...)
	customers.Handle(http.MethodGet, "/payment-methods", h(httphandlers.GetAdminUserPaymentMethods), customerRead...)
	customers.Handle(http.MethodGet, "/payments", h(httphandlers.GetAdminUserPayments), payRead...)
	customers.Handle(http.MethodPost, "/payments/off-channel", h(httphandlers.AdminCreateOffChannelPayment), customerWrite...)
	customers.Handle(http.MethodPost, "/entitlements", h(httphandlers.GrantAdminEntitlement), customerWrite...)
	customers.Handle(http.MethodDelete, "/entitlements/:id", h(httphandlers.RevokeAdminEntitlement), customerWrite...)
	customers.Handle(http.MethodPost, "/product-access", h(httphandlers.GrantAdminProductAccess), customerWrite...)
	customers.Handle(http.MethodDelete, "/product-access/:id", h(httphandlers.RevokeAdminProductAccess), customerWrite...)

	payments := rr.Group("/payments")
	payments.Handle(http.MethodGet, "", h(httphandlers.GetAdminPayments), payRead...)
	payments.Handle(http.MethodGet, "/:id", h(httphandlers.GetAdminPayment), payRead...)
	payments.Handle(http.MethodPost, "/:id/refunds", h(httphandlers.AdminRefundPayment), payRefund...)

	subs := rr.Group("/subscriptions")
	subs.Handle(http.MethodGet, "", h(httphandlers.GetAdminSubscriptions), subRead...)
	subs.Handle(http.MethodGet, "/:id", h(httphandlers.GetAdminSubscription), subRead...)
	subs.Handle(http.MethodPost, "/:id/cancel", h(httphandlers.AdminCancelSubscription), subWrite...)
	subs.Handle(http.MethodPost, "/:id/resume", h(httphandlers.AdminResumeSubscription), subWrite...)
	subs.Handle(http.MethodPut, "/:id/payment-method", h(httphandlers.AdminUpdateSubscriptionPaymentMethod), subWrite...)

	rr.Handle(http.MethodGet, "/metrics", h(httphandlers.GetAdminMetrics), usageRead...)
	rr.Handle(http.MethodGet, "/repair-alerts", h(httphandlers.GetAdminRepairAlerts), repairRead...)
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
