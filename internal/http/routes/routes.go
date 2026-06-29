package routes

import (
	"context"
	"errors"
	"net/http"
	"strings"

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

	// Gate protects merchant routes. AuthKit/control-plane and embedded host auth
	// are adapters behind this one interface.
	Gate billingauth.Gate
}

type GateOptions struct {
	Authenticator             billingauth.Authenticator
	AdminPermissionChecker    authpolicy.AdminPermissionChecker
	ServiceCredentialResolver ServiceCredentialResolver
	DelegatedResolver         DelegatedResolver
	DelegatedAuthenticator    billingauth.DelegatedAuthenticator
}

func NewGate(opts GateOptions) billingauth.Gate {
	return legacyGate(opts)
}

type legacyGate GateOptions

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

type merchantGroupResolver interface {
	ResolveMerchantForGroup(ctx context.Context, merchantRef string) (merchant.ID, string, error)
}

// merchantUserResolver resolves the merchant a user session is acting on from
// the user's merchant-group membership. User access tokens carry no merchant
// claim under #567, so /v1/merchant routes infer it from membership.
type merchantUserResolver interface {
	MerchantForUser(ctx context.Context, userID string) (string, error)
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
	group.Handle(http.MethodGet, "/credit-limit", h(httphandlers.ServiceGetCreditLimit), readMW...)

	admissions := group.Group("/admissions")
	admissions.Handle(http.MethodPost, "/:id/capture", h(httphandlers.ServiceCaptureHold), admissionMW...)
	admissions.Handle(http.MethodPost, "/:id/release", h(httphandlers.ServiceReleaseHold), admissionMW...)

	usage := group.Group("/usage")
	usage.Handle(http.MethodPost, "/metered", h(httphandlers.ServiceMeteredUsage), usageReadMW...)
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

func RegisterCatalogRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	var dbMW []router.Middleware
	if rt != nil && rt.DB != nil {
		dbMW = append(dbMW, middleware.MerchantDBConnMW(rt.DB))
	}
	registerCatalogActionRoutes(rr, opts, dbMW...)
}

func RegisterPaymentProviderRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	var dbMW []router.Middleware
	if rt != nil && rt.DB != nil {
		dbMW = append(dbMW, middleware.MerchantDBConnMW(rt.DB))
	}
	registerPaymentProviderActionRoutes(rr, opts, dbMW...)
}

func (opts Options) merchantActionPermissionMW(perm string) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			if opts.Gate == nil {
				r.AbortJSON(http.StatusInternalServerError, "authorization unavailable")
				return
			}
			principal, err := opts.Gate.Authorize(r.Request.Context(), r.Request, perm)
			if err != nil {
				var ge billingauth.GateError
				if errors.As(err, &ge) {
					r.AbortJSON(ge.Status, ge.Message)
				} else {
					r.AbortJSON(http.StatusInternalServerError, "authorization unavailable")
				}
				return
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
			r.Set(httphandlers.MerchantRoutePrincipalContextKey, true)
			next(r)
		}
	}
}

func (g legacyGate) Authorize(ctx context.Context, req *http.Request, perm string) (billingauth.Principal, error) {
	if resolved, err, handled := g.resolveServiceCredential(ctx, req, g.Authenticator != nil); handled {
		if err != nil {
			switch {
			case errors.Is(err, controlplane.ErrServiceCredentialMerchantUnresolved):
				return billingauth.Principal{}, billingauth.GateError{Status: http.StatusForbidden, Message: "service_credential_merchant_unresolved"}
			case errors.Is(err, controlplane.ErrServiceCredentialScopeDenied):
				return billingauth.Principal{}, billingauth.GateError{Status: http.StatusForbidden, Message: "service_credential_resource_scope_denied"}
			default:
				return billingauth.Principal{}, billingauth.GateError{Status: http.StatusUnauthorized, Message: "service_credential_invalid"}
			}
		}
		if resolved == nil {
			return billingauth.Principal{}, billingauth.GateError{Status: http.StatusUnauthorized, Message: "service_credential_invalid"}
		}
		if !resolved.HasPermission(perm) {
			return billingauth.Principal{}, billingauth.GateError{Status: http.StatusForbidden, Message: "permission_required"}
		}
		return billingauth.Principal{MerchantID: resolved.MerchantID}, nil
	}
	if g.DelegatedResolver != nil && req != nil {
		if token := bearerToken(req.Header.Get("Authorization")); controlplane.LooksLikeJWT(token) {
			resolved, err := g.DelegatedResolver.ResolveDelegated(ctx, token, req.Header.Get("Origin"))
			if err != nil {
				if g.Authenticator == nil || !errors.Is(err, controlplane.ErrDelegatedInvalid) {
					return billingauth.Principal{}, billingauth.GateError{Status: http.StatusUnauthorized, Message: "delegated_token_invalid"}
				}
			} else {
				if !resolved.HasPermission(perm) {
					return billingauth.Principal{}, billingauth.GateError{Status: http.StatusForbidden, Message: "permission_required"}
				}
				return billingauth.Principal{
					MerchantID: resolved.MerchantID,
					UserContext: billingauth.UserContext{
						UserID:        resolved.DelegatedSubject,
						Email:         resolved.Email,
						EmailVerified: resolved.EmailVerified,
						Username:      resolved.Username,
						Merchant:      resolved.Merchant,
					},
				}, nil
			}
		}
	}
	if g.DelegatedAuthenticator != nil && req != nil {
		principal, err := g.DelegatedAuthenticator.AuthenticateDelegated(ctx, req)
		if err != nil {
			return billingauth.Principal{}, billingauth.GateError{Status: http.StatusUnauthorized, Message: billingauth.UnauthenticatedMessage(err)}
		}
		resolved, verr := controlplane.ResolvedDelegatedFromHostPrincipal(principal)
		if verr != nil {
			return billingauth.Principal{}, billingauth.GateError{Status: http.StatusUnauthorized, Message: "delegated_principal_invalid"}
		}
		if !resolved.HasPermission(perm) {
			return billingauth.Principal{}, billingauth.GateError{Status: http.StatusForbidden, Message: "permission_required"}
		}
		return billingauth.Principal{
			MerchantID: resolved.MerchantID,
			UserContext: billingauth.UserContext{
				UserID:        resolved.DelegatedSubject,
				Email:         resolved.Email,
				EmailVerified: resolved.EmailVerified,
				Username:      resolved.Username,
				Merchant:      resolved.Merchant,
			},
		}, nil
	}
	if g.Authenticator == nil {
		return billingauth.Principal{}, billingauth.GateError{Status: http.StatusUnauthorized, Message: "bearer principal required"}
	}
	uc, err := g.Authenticator.Authenticate(ctx, req)
	if err != nil {
		return billingauth.Principal{}, billingauth.GateError{Status: http.StatusUnauthorized, Message: billingauth.UnauthenticatedMessage(err)}
	}
	if verr := uc.ValidateSubject(); verr != nil {
		return billingauth.Principal{}, billingauth.GateError{Status: http.StatusUnauthorized, Message: verr.Error()}
	}
	if g.AdminPermissionChecker == nil {
		return billingauth.Principal{}, billingauth.GateError{Status: http.StatusInternalServerError, Message: "authorization unavailable"}
	}
	if strings.TrimSpace(uc.Merchant) == "" {
		resolver, ok := g.AdminPermissionChecker.(merchantUserResolver)
		if !ok {
			return billingauth.Principal{}, billingauth.GateError{Status: http.StatusInternalServerError, Message: "merchant resolver unavailable"}
		}
		ref, rerr := resolver.MerchantForUser(ctx, uc.UserID)
		if rerr != nil || strings.TrimSpace(ref) == "" {
			return billingauth.Principal{}, billingauth.GateError{Status: http.StatusForbidden, Message: "merchant_unresolved"}
		}
		uc.Merchant = ref
	}
	allowed, err := g.AdminPermissionChecker.HasAdminPermission(ctx, uc.Merchant, uc.UserID, perm)
	if err != nil {
		return billingauth.Principal{}, billingauth.GateError{Status: http.StatusInternalServerError, Message: "failed to check permission"}
	}
	if !allowed {
		return billingauth.Principal{}, billingauth.GateError{Status: http.StatusForbidden, Message: "permission_required"}
	}
	mid, ok := merchant.FromContext(ctx)
	if !ok {
		resolver, ok := g.AdminPermissionChecker.(merchantGroupResolver)
		if !ok {
			return billingauth.Principal{}, billingauth.GateError{Status: http.StatusInternalServerError, Message: "merchant resolver unavailable"}
		}
		var err error
		mid, _, err = resolver.ResolveMerchantForGroup(ctx, uc.Merchant)
		if err != nil {
			return billingauth.Principal{}, billingauth.GateError{Status: http.StatusForbidden, Message: "merchant_unresolved"}
		}
	}
	return billingauth.Principal{MerchantID: mid, UserContext: uc}, nil
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

func (g legacyGate) resolveServiceCredential(ctx context.Context, r *http.Request, allowJWTFallthrough bool) (*controlplane.ResolvedServiceCredential, error, bool) {
	resolver := g.ServiceCredentialResolver
	if resolver == nil || r == nil {
		return nil, nil, false
	}
	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" {
		return nil, nil, false
	}
	if resolver.LooksLikeAPIKey(token) {
		resolved, err := resolver.ResolveAPIKey(ctx, token)
		if err != nil {
			return nil, err, true
		}
		return resolved, nil, true
	}
	if !controlplane.LooksLikeJWT(token) {
		return nil, nil, false
	}
	if raResolver, ok := resolver.(remoteApplicationResolver); ok {
		resolved, err := raResolver.ResolveRemoteApplication(ctx, token)
		if err == nil {
			return resolved, nil, true
		}
		if allowJWTFallthrough && errors.Is(err, controlplane.ErrDelegatedInvalid) {
			return nil, nil, false
		}
		if !errors.Is(err, controlplane.ErrNotRemoteApplicationToken) {
			return nil, err, true
		}
	}
	if jwtResolver, ok := resolver.(serviceJWTResolver); ok {
		resolved, err := jwtResolver.ResolveServiceJWT(ctx, token)
		if err == nil {
			return resolved, nil, true
		}
		// A VERIFIED service JWT that is definitively rejected (cross-merchant
		// resource scope, or its issuer owns no merchant) must surface as 403 — not
		// fall through to the delegated/user-session paths, which would mislabel it
		// 401 access_token_wrong_typ. A wrong-typ (not-a-service-JWT) error still
		// falls through so delegated/user tokens reach their own resolvers.
		if errors.Is(err, controlplane.ErrServiceCredentialScopeDenied) ||
			errors.Is(err, controlplane.ErrServiceCredentialMerchantUnresolved) {
			return nil, err, true
		}
	}
	return nil, nil, false
}

func bearerToken(header string) string {
	header = strings.TrimSpace(header)
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
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
	products.Handle(http.MethodGet, "/by-slug/:slug", h(httphandlers.AdminGetProductByKey), readMW...)
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
// Stripe + NMI-backed rails + CCBill) is shared by both the standalone gin server and the
// embedded mux, so the two cannot drift.
func RegisterMerchantWebhookRoutes(rr router.Router, rt *app.Runtime) {
	rr.Handle(http.MethodPost, "/merchants/:merchant/webhooks/:provider", h(httphandlers.MerchantWebhook))
}
