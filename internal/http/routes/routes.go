package routes

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/requestauth"

	"github.com/open-rails/authkit/verify"

	"github.com/open-rails/openrails/internal/app"
	authpolicy "github.com/open-rails/openrails/internal/auth/policy"
	"github.com/open-rails/openrails/internal/controlplane"
	httphandlers "github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/api"
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

	// ProviderRoutes controls provider-specific public routes. Nil preserves the
	// broad standalone surface; embedded single-merchant mounts pass an explicit
	// value derived from configured PSPs.
	ProviderRoutes *routesurface.ProviderRoutes

	// APIKeys is the #757 merchant self-serve API-key manager (mint/list/revoke
	// through AuthKit core). Implemented by *controlplane.ControlPlane. Nil
	// (an embedded host without a control plane) omits the /api-keys routes.
	APIKeys httphandlers.MerchantAPIKeyManager

	// Team is the #760 merchant team manager (roster, invites, role changes,
	// removal via AuthKit group membership). Implemented by
	// *controlplane.ControlPlane. Nil (an embedded host without a control plane)
	// omits the /team routes.
	Team httphandlers.MerchantTeamManager

	// AdminLimiter is the #111 per-human-admin operation limiter. It runs after
	// Gate has resolved the effective principal, so counters key the authorized
	// user rather than an untrusted token claim or source IP.
	AdminLimiter *middleware.AdminOperationLimiter
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
	ResolveDelegated(r *http.Request) (*controlplane.ResolvedDelegated, error)
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
	providerRoutes := routesurface.AllProviderRoutes()
	if opts.ProviderRoutes != nil {
		providerRoutes = *opts.ProviderRoutes
	}

	// Pin a merchant-scoped DB connection for the request (merchant resolved by the
	// global ResolveMerchant middleware) so RLS constrains merchant-owned queries
	// (issue #227). It applies to every route in this group.
	var group router.Router = rr
	if rt != nil && rt.DB != nil {
		group = rr.Group("", middleware.MerchantDBConnMW(rt.DB))
	}

	group.Handle(http.MethodGet, "/products", h(httphandlers.GetProducts), optional)
	group.Handle(http.MethodGet, "/prices", h(httphandlers.GetPrices), optional)
	// #829: public per-merchant checkout discovery — the merchant's ARMED PSPs
	// and the public-by-nature values a browser needs to tokenize on each.
	// Deliberately unauthenticated (the values are public; secrets are
	// structurally unreachable, see handlers.GetCheckoutConfig) and not gated on
	// providerRoutes: it is exactly the endpoint that TELLS a frontend which
	// rails this merchant has.
	group.Handle(http.MethodGet, "/checkout-config", h(httphandlers.GetCheckoutConfig))
	// The currency scale registry behind every monetary string on the wire:
	// system-fixed, so it needs neither a merchant nor a database connection.
	rr.Handle(http.MethodGet, "/currencies", h(httphandlers.GetCurrencies))
	if providerRoutes.Solana {
		group.Handle(http.MethodGet, "/solana/config", h(httphandlers.GetSolanaConfig))
		group.Handle(http.MethodGet, "/solana/tokens", h(httphandlers.GetSupportedTokens))
	}

	checkout := group.Group("/checkout", required)
	checkout.Handle(http.MethodPost, "", h(httphandlers.CreateCheckoutSession))
	checkout.Handle(http.MethodGet, "/:id", h(httphandlers.GetCheckoutSession))
	checkout.Handle(http.MethodPost, "/:id/confirm", h(httphandlers.ConfirmCheckoutSession))

	if providerRoutes.Solana {
		// One-off Solana Pay: the BUYER signs and pushes funds, so this needs only a
		// configured recipient — not an OpenRails signer.
		group.Handle(http.MethodGet, "/checkout/:id/solana-pay", h(httphandlers.GetSolanaPay))
		group.Handle(http.MethodPost, "/checkout/:id/solana-pay", h(httphandlers.PostSolanaPay))
	}
	if providerRoutes.SolanaSigning {
		// Solana recurring enrollment (#255): confirms after the wallet signs subscribe,
		// then OpenRails charges the first cycle — so it needs a signer (#661).
		group.Handle(http.MethodPost, "/solana/recurring/enroll", h(httphandlers.ConfirmSolanaEnrollment), required)
	}
}

// RegisterMerchantArchiveRoutes mounts the complete portable billing archive
// surface, shared by the server and database-only CLI runtime.
func RegisterMerchantArchiveRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	// Archives own their snapshot/restore transaction and its merchant RLS pin;
	// no outer merchant connection is held while transferring the artifact.
	rr.Handle(http.MethodGet, "/billing-archive", h(httphandlers.ExportMerchantBilling), opts.merchantActionPermissionMW(controlplane.PermMerchantBillingExport))
	rr.Handle(http.MethodPost, "/billing-archive", h(httphandlers.ImportMerchantBilling), opts.merchantActionPermissionMW(controlplane.PermMerchantBillingImport))
}

// RegisterServiceRoutes mounts the merchant billing surface. Access is gated by
// merchant permissions, not credential type (#564).
func RegisterServiceRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	group := rr
	RegisterMerchantArchiveRoutes(rr, rt, opts)
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

	hostEventReadMW := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantHostEventsRead)}, dbMW...)
	hostEventAckMW := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantHostEventsAcknowledge)}, dbMW...)
	group.Handle(http.MethodGet, "/host-events", h(httphandlers.ServiceListHostEvents), hostEventReadMW...)
	group.Handle(http.MethodPost, "/host-events/:id/acknowledge", h(httphandlers.ServiceAcknowledgeHostEvent), hostEventAckMW...)

	group.Handle(http.MethodPost, "/customers/entitlements:batch",
		h(httphandlers.ServiceGetExternalSubjectEntitlements),
		readMW...,
	)

	customers := group.Group("/customers/:customer_id")
	// Materialize a customer before its first purchase (idempotent touch).
	customers.Handle(http.MethodPut, "", h(httphandlers.ServiceEnsureCustomer), writeMW...)
	customers.Handle(http.MethodGet, "/billing-policy", h(httphandlers.ServiceGetCustomerBillingPolicy), readMW...)
	customers.Handle(http.MethodPut, "/billing-policy", h(httphandlers.ServiceSetCustomerBillingPolicy), writeMW...)
	paymentReadMW := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantPaymentsRead)}, dbMW...)
	customers.Handle(http.MethodGet, "/payment-settlement-status", h(httphandlers.ServicePaymentSettlementStatus), paymentReadMW...)
	customers.Handle(http.MethodGet, "/entitlements",
		h(httphandlers.ServiceGetCustomerEntitlements),
		readMW...,
	)
	customers.Handle(http.MethodPut, "/spend-delegations",
		h(httphandlers.ServicePutCustomerSpendDelegations),
		writeMW...,
	)
	customers.Handle(http.MethodPut, "/spend-delegations:upsert",
		h(httphandlers.ServicePutCustomerSpendDelegation),
		writeMW...,
	)
	// or#911: single-grant revocation for machine callers.
	customers.Handle(http.MethodDelete, "/spend-delegations/:scope/:scope_key",
		h(httphandlers.ServiceDeleteCustomerSpendDelegation),
		writeMW...,
	)
	// or#878: the delinquency state OpenRails derived, and the roster of who is
	// overdue. Read-only on purpose — the state is a reading of invoice truth,
	// so it is settled by paying the invoice, never by an API call.
	customers.Handle(http.MethodGet, "/delinquency",
		h(httphandlers.ServiceGetCustomerDelinquency),
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

	checkoutWriteMW := append([]router.Middleware{opts.merchantActionPermissionMW(permissions.MerchantCheckoutCreate)}, dbMW...)
	group.Handle(http.MethodPost, "/checkout-sessions", h(httphandlers.ServiceCreateCheckoutSession), checkoutWriteMW...)
	group.Handle(http.MethodGet, "/checkout-sessions/:id", h(httphandlers.ServiceGetCheckoutSession), readMW...)
	group.Handle(http.MethodPost, "/checkout-sessions/:id/confirm", h(httphandlers.ServiceConfirmCheckoutSession), checkoutWriteMW...)
	group.Handle(http.MethodGet, "/checkout-options", h(httphandlers.ServiceListCheckoutRailOptions), readMW...)
	group.Handle(http.MethodGet, "/checkout-config", h(httphandlers.ServiceGetCheckoutConfig), readMW...)
	customers.Handle(http.MethodGet, "/effective-tier", h(httphandlers.ServiceResolveEffectiveTier), readMW...)

	group.Handle(http.MethodPost, "/admissions", h(httphandlers.ServiceAdmitBatch), admissionMW...)
	group.Handle(http.MethodGet, "/settings", h(httphandlers.ServiceGetMerchantSettings), settingsReadMW...)
	group.Handle(http.MethodPut, "/settings", h(httphandlers.ServiceSetMerchantSettings), settingsWriteMW...)
	group.Handle(http.MethodGet, "/trust-level", h(httphandlers.ServiceGetTrustLevel), readMW...)
	group.Handle(http.MethodPost, "/wasted-spend", h(httphandlers.ServiceReportWastedSpend), admissionMW...)
	group.Handle(http.MethodPut, "/credit-limit", h(httphandlers.ServiceSetCreditLimit), writeMW...)
	group.Handle(http.MethodGet, "/credit-limit", h(httphandlers.ServiceGetCreditLimit), readMW...)
	group.Handle(http.MethodGet, "/delinquency", h(httphandlers.ServiceListDelinquency), readMW...)

	admissions := group.Group("/admissions")
	admissions.Handle(http.MethodPost, "/:id/capture", h(httphandlers.ServiceCaptureHold), admissionMW...)
	admissions.Handle(http.MethodPost, "/:id/release", h(httphandlers.ServiceReleaseHold), admissionMW...)
	admissions.Handle(http.MethodPost, "/:id/extend", h(httphandlers.ServiceExtendHold), admissionMW...)

	// #1004 provider obligations: spend authority for writes (settlement is
	// OpenRails-rated), usage authority for reads.
	operations := group.Group("/provider-operations")
	operations.Handle(http.MethodPost, "", h(httphandlers.ServiceOpenOperationAuthorization), admissionMW...)
	operations.Handle(http.MethodGet, "/:operation_id", h(httphandlers.ServiceGetOperationAuthorization), usageReadMW...)
	operations.Handle(http.MethodPost, "/:operation_id/release", h(httphandlers.ServiceReleaseOperationAuthorization), admissionMW...)
	operations.Handle(http.MethodPost, "/:operation_id/observations", h(httphandlers.ServiceRecordProviderBillingObservation), admissionMW...)
	operations.Handle(http.MethodGet, "/:operation_id/qualification", h(httphandlers.ServiceGetProviderBillingQualification), usageReadMW...)

	usage := group.Group("/usage")
	usage.Handle(http.MethodPost, "/report", h(httphandlers.ServiceRecordUsage), admissionMW...)
	usage.Handle(http.MethodPost, "/rollup", h(httphandlers.ServiceUsageRollup), usageReadMW...)
	usage.Handle(http.MethodPost, "/resource-revenue", h(httphandlers.ServiceResourceRevenue), usageReadMW...)

	credits := group.Group("/credits")
	credits.Handle(http.MethodGet, "/balance", h(httphandlers.ServiceGetCreditsBalance), readMW...)
	// The machine deposit stays permission-gated only (no AdminOperationGrant
	// limiter, or#906): the limiter is a HUMAN-velocity guard and machine
	// credentials pass it unmetered anyway; rail settlement bursts are
	// legitimate, and once-only is a database fact now (migration 0004).
	credits.Handle(http.MethodPost, "/deposit", h(httphandlers.ServiceDepositCredits), writeMW...)
	// or#906 key-qualified lookup: what did this deposit key do. GET on the
	// same path the POST writes — read gate.
	credits.Handle(http.MethodGet, "/deposit", h(httphandlers.ServiceGetDeposit), readMW...)
}

func RegisterMerchantActionRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	if opts.AdminLimiter == nil && rt != nil {
		opts.AdminLimiter = middleware.NewAdminOperationLimiter(rt.RedisClient)
	}
	var dbMW []router.Middleware
	if rt != nil && rt.DB != nil {
		dbMW = append(dbMW, middleware.MerchantDBConnMW(rt.DB))
	}
	registerMerchantSupportRoutes(rr, rt, opts, dbMW...)
}

func RegisterCatalogRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	var dbMW []router.Middleware
	if rt != nil && rt.DB != nil {
		dbMW = append(dbMW, middleware.MerchantDBConnMW(rt.DB))
	}
	registerCatalogActionRoutes(rr, rt, opts, dbMW...)
}

// RegisterImportRoutes mounts the #737 DeclaredBilling import door
// (POST <prefix>/billing). Gated on the distinct owner-level
// merchant:billing:import grant (a bulk book import rewrites
// subscriptions/payments/payment methods wholesale). No MerchantDBConnMW:
// the import seam pins its own merchant-scoped RLS connection.
func RegisterImportRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	write := opts.merchantActionPermissionMW(controlplane.PermMerchantBillingImport)
	rr.Handle(http.MethodPost, "/billing", h(httphandlers.ImportDeclaredBilling), write)
}

func RegisterPaymentProviderRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	var dbMW []router.Middleware
	if rt != nil && rt.DB != nil {
		dbMW = append(dbMW, middleware.MerchantDBConnMW(rt.DB))
	}
	registerPaymentProviderActionRoutes(rr, rt, opts, dbMW...)
}

// manifestModeWriteGuardMW keeps host-declared provider configuration immutable.
// Catalog mutation has its own authority guard below.
func manifestModeWriteGuardMW(rt *app.Runtime) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			if rt != nil && rt.Config.IsManifestMerchantSource() {
				r.APIError(&api.APIError{
					HTTPStatus: http.StatusMethodNotAllowed,
					Type:       api.ErrorTypeInvalidRequest,
					Code:       "manifest_driven",
					Message:    "merchant_source=manifest: payment-provider configuration is host-declared; update the host configuration and restart",
				})
				return
			}
			next(r)
		}
	}
}

// catalogModeWriteGuardMW permits API catalogs independently of provider secrets.
func catalogModeWriteGuardMW(rt *app.Runtime) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			if rt != nil && rt.Config.IsManifestCatalogSource() {
				r.APIError(&api.APIError{
					HTTPStatus: http.StatusMethodNotAllowed,
					Type:       api.ErrorTypeInvalidRequest,
					Code:       "manifest_driven",
					Message:    "catalog_source=manifest: catalog definitions are host-declared; update the catalog manifest and apply it",
				})
				return
			}
			next(r)
		}
	}
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
					if ge.Message == "sender_proof_required" {
						r.SetHeader("WWW-Authenticate", `DPoP error="invalid_dpop_proof", algs="ES256"`)
					}
					r.AbortJSON(ge.Status, ge.Message)
				} else {
					r.AbortJSON(http.StatusInternalServerError, "authorization unavailable")
				}
				return
			}
			if !middleware.EnforceMerchantBinding(r, principal.MerchantID) {
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
			// The full gate-resolved principal (existing consumers only check
			// presence; #757 api-key handlers read Permissions for no-escalation).
			r.Set(httphandlers.MerchantRoutePrincipalContextKey, principal)
			next(r)
		}
	}
}

func (g legacyGate) Authorize(ctx context.Context, req *http.Request, perm string) (billingauth.Principal, error) {
	// #685: in-process host principal, attached to the request CONTEXT by the
	// embed SDK's in-process transport. Trusted precisely because context values
	// cannot arrive on a network request (no header is consulted); gated on
	// permissions like every other credential.
	if hp, ok := requestauth.HostPrincipalFromContext(ctx); ok {
		if hp.MerchantID.IsZero() {
			return billingauth.Principal{}, billingauth.GateError{Status: http.StatusUnauthorized, Message: "host_principal_invalid"}
		}
		resolved := &controlplane.ResolvedServiceCredential{
			OwnerGroupRef: "in-process-host",
			MerchantID:    hp.MerchantID,
			MerchantSlug:  hp.MerchantSlug,
			Permissions:   hp.Permissions,
		}
		if !resolved.HasPermission(perm) {
			return billingauth.Principal{}, billingauth.GateError{Status: http.StatusForbidden, Message: "permission_required"}
		}
		return billingauth.Principal{MerchantID: hp.MerchantID, Subject: hp.Subject, Permissions: resolved.Permissions}, nil
	}
	if resolved, err, handled := g.resolveServiceCredential(ctx, req, g.Authenticator != nil); handled {
		if err != nil {
			switch {
			case errors.Is(err, controlplane.ErrServiceCredentialMerchantUnresolved):
				return billingauth.Principal{}, billingauth.GateError{Status: http.StatusForbidden, Message: "service_credential_merchant_unresolved"}
			case errors.Is(err, controlplane.ErrServiceCredentialScopeDenied):
				return billingauth.Principal{}, billingauth.GateError{Status: http.StatusForbidden, Message: "service_credential_resource_scope_denied"}
			case errors.Is(err, controlplane.ErrDelegatedIssuerUnknown), errors.Is(err, controlplane.ErrServiceCredentialHostMismatch):
				// The API key or issuer resolves to a different Host merchant.
				// Issuer resolution also uses its sentinel for unregistered/disabled issuers.
				return billingauth.Principal{}, billingauth.GateError{Status: http.StatusForbidden, Message: "host_merchant_mismatch"}
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
		return billingauth.Principal{MerchantID: resolved.MerchantID, Permissions: resolved.Permissions}, nil
	}
	if g.DelegatedResolver != nil && req != nil {
		if token := authorizationToken(req.Header.Get("Authorization")); controlplane.LooksLikeJWT(token) {
			resolved, err := g.DelegatedResolver.ResolveDelegated(req)
			if err != nil {
				if errors.Is(err, controlplane.ErrDelegatedUnavailable) {
					return billingauth.Principal{}, billingauth.GateError{Status: http.StatusServiceUnavailable, Message: "delegated_verification_unavailable"}
				}
				if errors.Is(err, verify.ErrSenderProofRequired) {
					return billingauth.Principal{}, billingauth.GateError{Status: http.StatusUnauthorized, Message: "sender_proof_required"}
				}
				if g.Authenticator == nil || !errors.Is(err, controlplane.ErrDelegatedInvalid) {
					return billingauth.Principal{}, billingauth.GateError{Status: http.StatusUnauthorized, Message: "delegated_token_invalid"}
				}
			} else {
				if !resolved.HasPermission(perm) {
					return billingauth.Principal{}, billingauth.GateError{Status: http.StatusForbidden, Message: "permission_required"}
				}
				return billingauth.Principal{
					MerchantID: resolved.MerchantID,
					Subject:    resolved.DelegatedSubject,
					UserContext: billingauth.UserContext{
						UserID:        resolved.DelegatedSubject,
						Email:         resolved.Email,
						EmailVerified: resolved.EmailVerified,
						Username:      resolved.Username,
						Merchant:      resolved.Merchant,
					},
					Permissions: resolved.Permissions,
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
			Subject:    resolved.DelegatedSubject,
			UserContext: billingauth.UserContext{
				UserID:        resolved.DelegatedSubject,
				Email:         resolved.Email,
				EmailVerified: resolved.EmailVerified,
				Username:      resolved.Username,
				Merchant:      resolved.Merchant,
			},
			Permissions: resolved.Permissions,
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
		if req != nil {
			uc.Merchant = strings.TrimSpace(req.Header.Get(billingauth.MerchantSelectorHeader))
		}

	}
	membershipMID, canonical, err := g.AdminPermissionChecker.ResolveAuthorizedMerchant(ctx, uc.Merchant, uc.UserID, perm)
	if err != nil {
		switch {
		case errors.Is(err, authpolicy.ErrPermissionRequired):
			return billingauth.Principal{}, billingauth.GateError{Status: http.StatusForbidden, Message: "permission_required"}
		case errors.Is(err, authpolicy.ErrMerchantUnresolved), errors.Is(err, controlplane.ErrMerchantAmbiguous):
			return billingauth.Principal{}, billingauth.GateError{Status: http.StatusForbidden, Message: "merchant_unresolved"}
		default:
			return billingauth.Principal{}, billingauth.GateError{Status: http.StatusInternalServerError, Message: "failed to check permission"}
		}
	}
	if membershipMID.IsZero() {
		return billingauth.Principal{}, billingauth.GateError{Status: http.StatusForbidden, Message: "merchant_unresolved"}
	}
	uc.Merchant = canonical
	mid, ok := merchant.FromContext(ctx)
	if !ok {
		mid = membershipMID
	}
	// #766: uc.Merchant was resolved from the USER'S group membership, but mid
	// may instead be the Host-pinned merchant (merchant.WithHostMerchant, set by
	// ResolveMerchantFromHostHTTP alongside merchant.WithID — #734). Without this
	// assertion a user with permission on merchant A, whose request lands on
	// merchant B's Host, would get a Principal scoped to B on authority checked
	// against A. Mirrors merchantForIssuer's identical Host-pin check
	// (internal/controlplane/issuer_registry.go) for the service-JWT/API-key/
	// delegated paths; HostMerchant (not the plain FromContext merchant) is the
	// right signal because it is a no-op unless a Host resolver actually ran, so
	// single-merchant self-hosters are unaffected.
	if hostMID, ok := merchant.HostMerchant(ctx); ok {
		if hostMID != membershipMID {
			return billingauth.Principal{}, billingauth.GateError{Status: http.StatusForbidden, Message: "host_merchant_mismatch"}
		}
	}
	if mid != membershipMID {
		return billingauth.Principal{}, billingauth.GateError{Status: http.StatusForbidden, Message: "merchant_context_mismatch"}
	}
	return billingauth.Principal{MerchantID: mid, Subject: uc.UserID, UserContext: uc}, nil
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
		// resource scope, its issuer owns no merchant, or — #734 — its issuer's
		// merchant disagrees with the request's Host-pinned merchant) must surface
		// as 403 — not fall through to the delegated/user-session paths, which
		// would mislabel it 401 access_token_wrong_typ. A wrong-typ (not-a-service-JWT)
		// error still falls through so delegated/user tokens reach their own resolvers.
		if errors.Is(err, controlplane.ErrServiceCredentialScopeDenied) ||
			errors.Is(err, controlplane.ErrServiceCredentialMerchantUnresolved) ||
			errors.Is(err, controlplane.ErrDelegatedIssuerUnknown) {
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

func registerCatalogActionRoutes(catalog router.Router, rt *app.Runtime, opts Options, dbMW ...router.Middleware) {
	read := opts.merchantActionPermissionMW(controlplane.PermMerchantCatalogRead)
	write := opts.merchantActionPermissionMW(authpolicy.PermMerchantCatalogUpdate)
	readMW := append([]router.Middleware{read}, dbMW...)
	// Reject mutations of a manifest-owned catalog before authorization work.
	writeMW := append([]router.Middleware{catalogModeWriteGuardMW(rt), write}, dbMW...)

	products := catalog.Group("/products")
	products.Handle(http.MethodPost, "", h(httphandlers.AdminCreateProduct), writeMW...)
	products.Handle(http.MethodGet, "", h(httphandlers.AdminListProducts), readMW...)
	products.Handle(http.MethodGet, "/:id", h(httphandlers.AdminGetProduct), readMW...)
	products.Handle(http.MethodGet, "/by-key/:key", h(httphandlers.AdminGetProductByKey), readMW...)
	products.Handle(http.MethodPatch, "/:id", h(httphandlers.AdminUpdateProduct), writeMW...)
	products.Handle(http.MethodPost, "/:id/activate", h(httphandlers.AdminActivateProduct), writeMW...)
	products.Handle(http.MethodPost, "/:id/deactivate", h(httphandlers.AdminDeactivateProduct), writeMW...)

	prices := catalog.Group("/prices")
	prices.Handle(http.MethodPost, "", h(httphandlers.AdminCreatePrice), writeMW...)
	prices.Handle(http.MethodGet, "", h(httphandlers.AdminListPrices), readMW...)
	prices.Handle(http.MethodGet, "/by-key/:key", h(httphandlers.AdminGetPriceByKey), readMW...)
	// #777: the key's version chain resolved from the #774 pointer-movement
	// log, most-recent-first — the console price page's "history with dates".
	prices.Handle(http.MethodGet, "/by-key/:key/history", h(httphandlers.AdminGetPriceKeyHistory), readMW...)
	prices.Handle(http.MethodGet, "/:id", h(httphandlers.AdminGetPrice), readMW...)
	prices.Handle(http.MethodPatch, "/:id", h(httphandlers.AdminUpdatePrice), writeMW...)
	prices.Handle(http.MethodPost, "/:id/activate", h(httphandlers.AdminActivatePrice), writeMW...)
	prices.Handle(http.MethodPost, "/:id/deactivate", h(httphandlers.AdminDeactivatePrice), writeMW...)
	// #774: relabel a price's key (a plain rename; version-bump repoint on
	// collision) — mode-1-guarded like every other catalog write.
	prices.Handle(http.MethodPost, "/:id/key", h(httphandlers.AdminSetPriceKey), writeMW...)

	meters := catalog.Group("/meters")
	meters.Handle(http.MethodGet, "", h(httphandlers.AdminListUsageMeters), readMW...)
	meters.Handle(http.MethodGet, "/:key", h(httphandlers.AdminGetUsageMeter), readMW...)
	meters.Handle(http.MethodGet, "/:key/overrides", h(httphandlers.AdminListUsageMeterOverrides), readMW...)
	meters.Handle(http.MethodPut, "/:key", h(httphandlers.AdminPutUsageMeter), writeMW...)
	meters.Handle(http.MethodPut, "/:key/rate-card", h(httphandlers.AdminPutDefaultUsageRateCard), writeMW...)
	meters.Handle(http.MethodDelete, "/:key/rate-card", h(httphandlers.AdminDeleteDefaultUsageRateCard), writeMW...)

	catalog.Handle(http.MethodGet, "/drift", h(httphandlers.AdminListCatalogDrift), readMW...)
	catalog.Handle(http.MethodPost, "/drift/refresh", h(httphandlers.AdminRefreshCatalogDrift), writeMW...)
	catalog.Handle(http.MethodPost, "/publish", h(httphandlers.MerchantPublishCatalog), writeMW...)

	// #779 catalog copilot: read-only Q&A (+ flag-gated Phase 2 drafting,
	// never a mutation) shares the catalog-read permission, like #756's
	// metrics ask shares metrics-read — the LLM-cost axis is guarded by the
	// service's own per-merchant rate limit + fail-closed consent flag. NOT
	// manifest-guarded: it never mutates catalog rows, even when drafting.
	// Registered only when the copilot is configured (llm.api_key +
	// llm.catalog_copilot_enabled): an absent route is the only honest
	// advertisement of an absent capability (#1001), and the console keys its
	// panels on /admin/config.json, never on probing here.
	if rt != nil && rt.CopilotService.Configured() {
		catalog.Handle(http.MethodPost, "/ask", h(httphandlers.CatalogCopilotAsk), readMW...)
		// The confirm-provenance log rides the catalog-WRITE permission (only a
		// caller who could actually apply a price change should be able to log a
		// draft as confirmed) but skips the mode-1 write guard: it never touches
		// a catalog row, only an audit log entry, for a mutation that already
		// happened via the normal catalog/reprice endpoints.
		copilotConfirmMW := append([]router.Middleware{write}, dbMW...)
		catalog.Handle(http.MethodPost, "/copilot/confirm", h(httphandlers.CatalogCopilotConfirmDraft), copilotConfirmMW...)
	}
}

func registerPaymentProviderActionRoutes(providers router.Router, rt *app.Runtime, opts Options, dbMW ...router.Middleware) {
	read := opts.merchantActionPermissionMW(controlplane.PermMerchantPaymentProvidersRead)
	write := opts.merchantActionPermissionMW(controlplane.PermMerchantPaymentProvidersUpdate)
	readMW := append([]router.Middleware{read}, dbMW...)
	writeMW := append([]router.Middleware{manifestModeWriteGuardMW(rt), write}, dbMW...)

	providers.Handle(http.MethodGet, "", h(httphandlers.MerchantListPaymentProviders), readMW...)
	// or#288 routing dry run: read-only "which PSP would this checkout get, and
	// why" — same permission as reading the PSP catalog, since the answer is a
	// projection of it. Registered before "/:provider" so "routing" is never
	// captured as a provider name.
	providers.Handle(http.MethodPost, "/routing/dry-run", h(httphandlers.MerchantDryRunCheckoutRouting), readMW...)
	providers.Handle(http.MethodGet, "/:provider", h(httphandlers.MerchantGetPaymentProvider), readMW...)
	// Host-owned provider configuration has no mutation HTTP surface. Keep
	// reads and routing dry runs available, independently of catalog ownership.
	if rt != nil && rt.Config.IsManifestMerchantSource() {
		return
	}
	// Provider-config WRITE surface persists secrets; mount it only when OpenRails
	// can actually write them (#661). Explicit route selection cannot override
	// the runtime backend, and callers may omit ProviderRoutes entirely.
	secretWrite := opts.ProviderRoutes == nil || opts.ProviderRoutes.SecretWrite
	if rt != nil && rt.RouteCapabilities != nil {
		secretWrite = secretWrite && rt.RouteCapabilities.SecretWrite
	}
	if secretWrite {
		providers.Handle(http.MethodPut, "/:provider", h(httphandlers.MerchantPutPaymentProvider), writeMW...)
	}
	// Lifecycle archives (#655/#656) write only the PSP row — never a secret,
	// never the provider — so they stay mounted when the secret backend is
	// read-only: a terminated account must be archivable from any deployment.
	providers.Handle(http.MethodDelete, "/:provider", h(httphandlers.MerchantDeletePaymentProvider), writeMW...)
	providers.Handle(http.MethodPost, "/:provider/accounts/:psp_id/archive", h(httphandlers.MerchantArchivePaymentProviderAccount), writeMW...)
}

func registerMerchantSupportRoutes(rr router.Router, rt *app.Runtime, opts Options, dbMW ...router.Middleware) {
	registerMerchantInvoiceRoutes(rr, opts, dbMW...)
	customerRead := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantCustomerSettingsRead)}, dbMW...)
	offChannelWrite := opts.merchantAdminOperationMW(controlplane.PermMerchantCustomerSettingsUpdate, middleware.AdminOperationOffChannel, dbMW...)
	grantWrite := opts.merchantAdminOperationMW(controlplane.PermMerchantCustomerSettingsUpdate, middleware.AdminOperationGrant, dbMW...)
	revokeWrite := opts.merchantAdminOperationMW(controlplane.PermMerchantCustomerSettingsUpdate, middleware.AdminOperationDestructive, dbMW...)
	payRead := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantPaymentsRead)}, dbMW...)
	payRefund := opts.merchantAdminOperationMW(controlplane.PermMerchantPaymentsRefund, middleware.AdminOperationDestructive, dbMW...)
	subRead := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantSubscriptionsRead)}, dbMW...)
	subWrite := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantSubscriptionsUpdate)}, dbMW...)
	tierChangeWrite := opts.merchantAdminOperationMW(controlplane.PermMerchantSubscriptionsUpdate, middleware.AdminOperationOffChannel, dbMW...)
	subCancel := opts.merchantAdminOperationMW(controlplane.PermMerchantSubscriptionsUpdate, middleware.AdminOperationDestructive, dbMW...)
	repairRead := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantRepairAlertsRead)}, dbMW...)

	// #740: merchant customer list/search for the admin console.
	rr.Handle(http.MethodGet, "/customers", h(httphandlers.ListAdminCustomers), customerRead...)

	customers := rr.Group("/customers/:customer_id")
	customers.Handle(http.MethodGet, "", h(httphandlers.GetAdminUserBillingProfile), customerRead...)
	customers.Handle(http.MethodGet, "/payment-methods", h(httphandlers.GetAdminUserPaymentMethods), customerRead...)
	customers.Handle(http.MethodDelete, "/payment-methods/:id", h(httphandlers.AdminDeletePaymentMethod), revokeWrite...)
	customers.Handle(http.MethodGet, "/payments", h(httphandlers.GetAdminUserPayments), payRead...)
	customers.Handle(http.MethodPost, "/payments/off-channel", h(httphandlers.AdminCreateOffChannelPayment), offChannelWrite...)
	customers.Handle(http.MethodPost, "/entitlements", h(httphandlers.GrantAdminEntitlement), grantWrite...)
	customers.Handle(http.MethodDelete, "/entitlements/:id", h(httphandlers.RevokeAdminEntitlement), revokeWrite...)
	customers.Handle(http.MethodPost, "/product-access", h(httphandlers.GrantAdminProductAccess), grantWrite...)
	customers.Handle(http.MethodDelete, "/product-access/:id", h(httphandlers.RevokeAdminProductAccess), revokeWrite...)
	// or#906: human-admin credit grant. Money-in gets its OWN permission
	// (merchant:credits:grant — owner-level, NOT held by the fixed support
	// role) rather than riding customer-settings:update like its siblings:
	// minting balance is the one grant whose blast radius is monetary.
	creditsGrantWrite := opts.merchantAdminOperationMW(controlplane.PermMerchantCreditsGrant, middleware.AdminOperationGrant, dbMW...)
	customers.Handle(http.MethodPost, "/credits", h(httphandlers.AdminGrantCredits), creditsGrantWrite...)
	creditsRevokeWrite := opts.merchantAdminOperationMW(controlplane.PermMerchantCreditsRevoke, middleware.AdminOperationDestructive, dbMW...)
	customers.Handle(http.MethodGet, "/credits", h(httphandlers.ListAdminCreditGrants(opts.Gate)), customerRead...)
	customers.Handle(http.MethodDelete, "/credits/:grant_id", h(httphandlers.RevokeAdminCreditGrant), creditsRevokeWrite...)
	customers.Handle(http.MethodGet, "/credit-transactions", h(httphandlers.ListAdminCreditTransactions), customerRead...)

	// or#909 negotiated price overrides: per-customer rate cards replacing the
	// merchant-default card for a meter (included allowance netted before
	// overage). PUT rides the grant class; DELETE the destructive class —
	// dropping a negotiated card silently reprices the customer at default.
	customers.Handle(http.MethodGet, "/rate-overrides", h(httphandlers.ListAdminRateOverrides), customerRead...)
	customers.Handle(http.MethodPut, "/rate-overrides/:meter_key", h(httphandlers.PutAdminRateOverride), grantWrite...)
	customers.Handle(http.MethodDelete, "/rate-overrides/:meter_key", h(httphandlers.DeleteAdminRateOverride), revokeWrite...)

	payments := rr.Group("/payments")
	payments.Handle(http.MethodGet, "", h(httphandlers.GetAdminPayments), payRead...)
	payments.Handle(http.MethodGet, "/:id", h(httphandlers.GetAdminPayment), payRead...)
	payments.Handle(http.MethodPost, "/:id/refunds", h(httphandlers.AdminRefundPayment), payRefund...)

	subs := rr.Group("/subscriptions")
	subs.Handle(http.MethodGet, "", h(httphandlers.GetAdminSubscriptions), subRead...)
	subs.Handle(http.MethodGet, "/:id", h(httphandlers.GetAdminSubscription), subRead...)
	subs.Handle(http.MethodPost, "/:id/cancel", h(httphandlers.AdminCancelSubscription), subCancel...)
	subs.Handle(http.MethodPost, "/:id/resume", h(httphandlers.AdminResumeSubscription), subWrite...)
	subs.Handle(http.MethodPost, "/:id/change-tier", h(httphandlers.AdminChangeTier), tierChangeWrite...)
	subs.Handle(http.MethodPost, "/:id/change-tier/preview", h(httphandlers.AdminChangeTierPreview), subWrite...)
	subs.Handle(http.MethodPut, "/:id/payment-method", h(httphandlers.AdminUpdateSubscriptionPaymentMethod), subWrite...)
	subs.Handle(http.MethodPost, "/:id/provider-cutover", h(httphandlers.ProviderCutover), subWrite...)
	subs.Handle(http.MethodGet, "/:id/provider-cutover", h(httphandlers.ProviderCutover), subRead...)
	subs.Handle(http.MethodPost, "/:id/provider-cutover/preview", h(httphandlers.PreviewProviderCutover), subRead...)
	// #773 reprice: schedule a single subscription's price move at its next
	// renewal on/after effective_at.
	subs.Handle(http.MethodPost, "/:id/reprice", h(httphandlers.CreateSubscriptionReprice), subWrite...)

	// #773 reprice: bulk reprice_all_prior_versions(key, effective_date), plus
	// the inspect (list/get) and cancel-before-effective surface the #777
	// console wizard needs.
	rr.Handle(http.MethodPost, "/catalog/reprice-all-prior-versions", h(httphandlers.RepriceAllPriorVersions), subWrite...)
	// #777: read-only dry-run affected-count preview — the wizard's Step 2,
	// called BEFORE the price edit that creates the new version (so never
	// mutates, unlike the bulk call above).
	rr.Handle(http.MethodGet, "/catalog/reprice-all-prior-versions/preview", h(httphandlers.PreviewRepriceAllPriorVersions), subRead...)
	// #813 plan migrations: operator-driven cross-product bulk retirement
	// (plan-A -> plan-B) over the reprice engine. preview registered before
	// "/:id" so it is never captured as an id.
	pm := rr.Group("/plan-migrations")
	pm.Handle(http.MethodPost, "", h(httphandlers.CreatePlanMigration), subWrite...)
	pm.Handle(http.MethodPost, "/preview", h(httphandlers.PreviewPlanMigration), subRead...)
	pm.Handle(http.MethodGet, "/:id", h(httphandlers.GetPlanMigration), subRead...)
	pm.Handle(http.MethodPost, "/:id/cancel", h(httphandlers.CancelPlanMigration), subWrite...)

	reprices := rr.Group("/reprices")
	reprices.Handle(http.MethodGet, "", h(httphandlers.ListSubscriptionReprices), subRead...)
	// #777: list a price key's bulk reprice batches (pending-migration display
	// on the price page) — must be registered before "/:id" so "batches" is
	// never captured as an id.
	reprices.Handle(http.MethodGet, "/batches", h(httphandlers.ListRepriceBatchesByKey), subRead...)
	reprices.Handle(http.MethodGet, "/:id", h(httphandlers.GetSubscriptionReprice), subRead...)
	reprices.Handle(http.MethodPost, "/:id/cancel", h(httphandlers.CancelSubscriptionReprice), subWrite...)

	// #733 PG-first metrics API (replaces the #735-deleted ClickHouse surface):
	// one composable query endpoint + the registry/schema doc.
	metricsRead := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantMetricsRead)}, dbMW...)
	metricsGrp := rr.Group("/metrics")
	metricsGrp.Handle(http.MethodPost, "/query", h(httphandlers.MerchantMetricsQuery), metricsRead...)
	metricsGrp.Handle(http.MethodGet, "/schema", h(httphandlers.MerchantMetricsSchema), metricsRead...)
	// #756 metrics Q&A: read-only over the same data as /query (evidence IS
	// /query output), so it shares the metrics-read permission; the LLM-cost
	// axis is guarded by the per-merchant ask rate limit + fail-closed consent.
	// Registered only with llm.api_key AND the llm.ask_enabled consent: /ask
	// sends aggregate query results to the LLM provider.
	if rt != nil && rt.DashboardService.AskConfigured() {
		metricsGrp.Handle(http.MethodPost, "/ask", h(httphandlers.MerchantMetricsAsk), metricsRead...)
	}

	// #741 configurable dashboard: reads share the metrics permission (a
	// dashboard is a saved view over metrics); writes + NL generation (the
	// LLM call costs money) need the dashboard write grant.
	dashboardWrite := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantDashboardUpdate)}, dbMW...)
	rr.Handle(http.MethodGet, "/dashboard", h(httphandlers.GetMerchantDashboard), metricsRead...)
	rr.Handle(http.MethodPut, "/dashboard", h(httphandlers.PutMerchantDashboard), dashboardWrite...)
	// NL widget generation exists only when an LLM key is configured.
	if rt != nil && rt.DashboardService.NLConfigured() {
		rr.Handle(http.MethodPost, "/dashboard/widgets/generate", h(httphandlers.GenerateDashboardWidget), dashboardWrite...)
	}

	// #757 merchant self-serve API keys: mint/list/revoke scoped credentials
	// through AuthKit core. Gated on merchant:credentials:manage — the SAME
	// string AuthKit's own mint authorization checks; only the merchant owner
	// (merchant:*) holds it in the fixed #567 catalog. No MerchantDBConnMW:
	// these handlers touch only the control plane, never the runtime DB.
	if opts.APIKeys != nil {
		credentialsManage := opts.merchantActionPermissionMW(controlplane.PermMerchantCredentialsManage)
		apiKeys := rr.Group("/api-keys")
		apiKeys.Handle(http.MethodPost, "", h(httphandlers.MerchantCreateAPIKey(opts.APIKeys)), credentialsManage)
		apiKeys.Handle(http.MethodGet, "", h(httphandlers.MerchantListAPIKeys(opts.APIKeys)), credentialsManage)
		apiKeys.Handle(http.MethodDelete, "/:id", h(httphandlers.MerchantRevokeAPIKey(opts.APIKeys)), credentialsManage)
	}

	// #850 merchant api_host (#734 Host routing): read + assign the merchant's
	// canonical API host. Reads gate on merchant:settings:read; the write on
	// merchant:settings:update — owner-only in the fixed #567 catalog. No
	// MerchantDBConnMW: the merchants directory service writes the directory
	// row (openrails.merchants, not an RLS-scoped merchant table) with its own
	// pool.
	// Registered only when the merchant directory is armed; a deployment
	// without one has no host mapping to read or assign.
	if rt != nil && rt.Merchants != nil {
		apiHostRead := opts.merchantActionPermissionMW(controlplane.PermMerchantSettingsRead)
		apiHostWrite := opts.merchantActionPermissionMW(controlplane.PermMerchantSettingsUpdate)
		rr.Handle(http.MethodGet, "/api-host", h(httphandlers.GetMerchantAPIHost), apiHostRead)
		rr.Handle(http.MethodPut, "/api-host", h(httphandlers.PutMerchantAPIHost), apiHostWrite)
	}

	// #760 merchant team management: roster, invites (register+join links),
	// role changes, and removal — all through AuthKit group membership. Reads
	// gate on merchant:members:read; mutations on merchant:members:manage. Only
	// the owner (merchant:*) holds either in the fixed #567 catalog, so the whole
	// surface is owner-only (mirrors #757). No MerchantDBConnMW: control plane
	// only. `/team/invites` (literal) and `/team/:user_id` never collide — they
	// differ by segment shape/method.
	if opts.Team != nil {
		membersRead := opts.merchantActionPermissionMW(controlplane.PermMerchantMembersRead)
		membersManage := opts.merchantActionPermissionMW(controlplane.PermMerchantMembersManage)
		team := rr.Group("/team")
		team.Handle(http.MethodGet, "", h(httphandlers.MerchantListTeam(opts.Team)), membersRead)
		team.Handle(http.MethodGet, "/invites", h(httphandlers.MerchantListTeamInvites(opts.Team)), membersRead)
		team.Handle(http.MethodPost, "/invites", h(httphandlers.MerchantInviteTeamMember(opts.Team)), membersManage)
		team.Handle(http.MethodDelete, "/invites/:id", h(httphandlers.MerchantRevokeTeamInvite(opts.Team)), membersManage)
		team.Handle(http.MethodPatch, "/:user_id", h(httphandlers.MerchantChangeTeamRole(opts.Team)), membersManage)
		team.Handle(http.MethodDelete, "/:user_id", h(httphandlers.MerchantRemoveTeamMember(opts.Team)), membersManage)
	}

	// Outbound notification destinations and the merchant notification bell.
	settingsWrite := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantSettingsUpdate)}, dbMW...)
	webhooks := rr.Group("/webhooks")
	webhooks.Handle(http.MethodGet, "", h(httphandlers.ListMerchantWebhooks), metricsRead...)
	webhooks.Handle(http.MethodPost, "", h(httphandlers.CreateMerchantWebhook), settingsWrite...)
	webhooks.Handle(http.MethodDelete, "/:id", h(httphandlers.DeleteMerchantWebhook), settingsWrite...)
	webhooks.Handle(http.MethodPut, "/:id/url", h(httphandlers.RotateMerchantWebhookURL), settingsWrite...)

	notifications := rr.Group("/notifications")
	notifications.Handle(http.MethodGet, "", h(httphandlers.ListMerchantNotifications), metricsRead...)
	notifications.Handle(http.MethodGet, "/unread-count", h(httphandlers.MerchantNotificationsUnreadCount), metricsRead...)
	notifications.Handle(http.MethodPost, "/:id/read", h(httphandlers.MarkMerchantNotificationRead), settingsWrite...)

	rr.Handle(http.MethodGet, "/repair-alerts", h(httphandlers.GetAdminRepairAlerts), repairRead...)
	// #689: worker-health dashboard — same operator repair surface/permission.
	rr.Handle(http.MethodGet, "/worker-health", h(httphandlers.GetAdminWorkerHealth), repairRead...)

	// #692 operator findings queue: reads share the repair surface permission;
	// resolve executes recommendations (cancel/refund/revoke/grant) and is a
	// distinct write grant. One item at a time — no bulk endpoint (#679).
	findingsResolve := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantFindingsResolve)}, dbMW...)
	findings := rr.Group("/findings")
	findings.Handle(http.MethodGet, "", h(httphandlers.AdminListFindings), repairRead...)
	findings.Handle(http.MethodGet, "/:id", h(httphandlers.AdminGetFinding), repairRead...)
	findings.Handle(http.MethodPost, "/:id/resolve", h(httphandlers.AdminResolveFinding), findingsResolve...)
}

// merchantAdminOperationMW keeps the authorization gate outermost, then applies
// the user-keyed operation limiter before any merchant DB connection is pinned.
func (opts Options) merchantAdminOperationMW(perm string, operation middleware.AdminOperation, trailing ...router.Middleware) []router.Middleware {
	mw := []router.Middleware{opts.merchantActionPermissionMW(perm)}
	if opts.AdminLimiter != nil && operation != "" {
		mw = append(mw, opts.AdminLimiter.AdminRateLimitMW(operation))
	}
	return append(mw, trailing...)
}

// RegisterWebhookRoutes mounts the ONE standalone webhook surface (#650,
// or#893): POST /webhooks/:provider (NMI/CCBill; the merchant is derived from
// the payload's account identity, not the path) and
// /webhooks/:provider/:account_id (direct Stripe). :provider is a RAIL —
// nmi/ccbill/stripe/solana/basistheory — never a PSP key. This is the live
// production entry point for inbound NMI/CCBill webhooks. Embedded hosts use
// RegisterMerchantWebhookRoutes instead, since they pin one merchant in context
// and have no payload-derived merchant to resolve.
func RegisterWebhookRoutes(rr router.Router, rt *app.Runtime) {
	rr.Handle(http.MethodPost, "/:provider/:account_id", h(httphandlers.Webhook))
	rr.Handle(http.MethodPost, "/:provider", h(httphandlers.Webhook))
}

// RegisterMerchantWebhookRoutes mounts the merchant-scoped webhook surface
// (POST /merchants/:merchant/webhooks/:provider, issue #529): the merchant is
// resolved from the URL slug, then THAT merchant's signing secret verifies the
// payload. This is the EMBEDDED surface only — a host that pins one merchant
// has no payload-derived identity to resolve. or#893 removed the standalone
// mount: there the canonical RegisterWebhookRoutes surface derives the merchant
// from PSP identity, and a URL slug was a second way to say the
// same thing.
func RegisterMerchantWebhookRoutes(rr router.Router, rt *app.Runtime) {
	rr.Handle(http.MethodPost, "/merchants/:merchant/webhooks/:provider", h(httphandlers.MerchantWebhook))
	// #641: per-account endpoint — account_id in the path selects which account the
	// event is for (verify with its secret). For multi-account rails like NMI.
	rr.Handle(http.MethodPost, "/merchants/:merchant/webhooks/:provider/:account_id", h(httphandlers.MerchantWebhook))
}

// RegisterHostWebhookRoutes mounts the Host-routed webhook surface (#734):
// POST /webhooks/:provider[/:account_id], merchant resolved from the request's
// Host header via resolve — the SAME resolver merchant-scoped route
// resolution and the issuer-consistency check use — rather than a URL slug
// (RegisterMerchantWebhookRoutes) or payload account identity
// (RegisterWebhookRoutes). This is the engine half of saas
// #15's "api.<slug>.<domain>" hostname scheme: pkg/embedded mounts it
// alongside the merchant-scoped surface at the SAME canonical path shape the
// standalone provider-only surface uses, since Host (not a path segment)
// carries the merchant here. Opt-in: callers only mount this when a resolver
// is available (an attached control plane).
func RegisterHostWebhookRoutes(rr router.Router, rt *app.Runtime, resolve merchant.HostResolver) {
	handler := h(httphandlers.HostWebhook(resolve))
	rr.Handle(http.MethodPost, "/webhooks/:provider", handler)
	rr.Handle(http.MethodPost, "/webhooks/:provider/:account_id", handler)
}

// registerMerchantInvoiceRoutes reuses the existing support-operation limits.
func registerMerchantInvoiceRoutes(rr router.Router, opts Options, dbMW ...router.Middleware) {
	read := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantInvoicesRead)}, dbMW...)
	update := opts.merchantAdminOperationMW(controlplane.PermMerchantInvoicesUpdate, middleware.AdminOperationDestructive, dbMW...)
	collect := opts.merchantAdminOperationMW(controlplane.PermMerchantInvoicesCollect, middleware.AdminOperationOffChannel, dbMW...)
	remittance := opts.merchantAdminOperationMW(controlplane.PermMerchantInvoicesUpdate, middleware.AdminOperationOffChannel, dbMW...)
	invoices := rr.Group("/invoices")
	invoices.Handle(http.MethodGet, "", h(httphandlers.ListAdminInvoices(opts.Gate)), read...)
	invoices.Handle(http.MethodGet, "/:id", h(httphandlers.GetAdminInvoice(opts.Gate)), read...)
	invoices.Handle(http.MethodGet, "/:id/payments", h(httphandlers.ListAdminInvoicePayments), read...)
	invoices.Handle(http.MethodPost, "/:id/void", h(httphandlers.MutateAdminInvoice("void")), update...)
	invoices.Handle(http.MethodPost, "/:id/uncollectible", h(httphandlers.MutateAdminInvoice("mark_uncollectible")), update...)
	invoices.Handle(http.MethodPost, "/:id/payments", h(httphandlers.MutateAdminInvoice("record_payment")), remittance...)
	invoices.Handle(http.MethodPost, "/:id/retry-collection", h(httphandlers.RetryAdminInvoiceCollection), collect...)
	profileRead := append([]router.Middleware{opts.merchantActionPermissionMW(controlplane.PermMerchantCustomerSettingsRead)}, dbMW...)
	profileWrite := opts.merchantAdminOperationMW(controlplane.PermMerchantCustomerSettingsUpdate, middleware.AdminOperationGrant, dbMW...)
	rr.Handle(http.MethodGet, "/customers/:customer_id/invoice-profile", h(httphandlers.GetAdminInvoiceProfile(opts.Gate)), profileRead...)
	rr.Handle(http.MethodPut, "/customers/:customer_id/invoice-profile", h(httphandlers.PutAdminInvoiceProfile), profileWrite...)
}

func authorizationToken(header string) string {
	fields := strings.Fields(header)
	if len(fields) == 2 && (strings.EqualFold(fields[0], "Bearer") || strings.EqualFold(fields[0], "DPoP")) {
		return fields[1]
	}
	return ""
}
