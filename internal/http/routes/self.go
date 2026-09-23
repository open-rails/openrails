package routes

import (
	"github.com/open-rails/openrails/permissions"
	"net/http"

	"github.com/open-rails/openrails/internal/app"
	httphandlers "github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routesurface"
)

// SelfRoutePrefix is the canonical browser self-service billing surface. The
// credential profile may be a delegated JWT in standalone mode or a host/user
// bearer in embedded mode; the URL is intentionally one stable `/me` surface
// and the credential profile lives on the resolved Principal.
const SelfRoutePrefix = "/me"

// CustomerRoutePrefix is the canonical customer-treasury surface (#567): a
// customer (any payer) acting over its OWN co-managed/shared balance, addressed
// by the customer's id — not merchant/seller administration.
const CustomerRoutePrefix = "/customers"

// RegisterSelfServiceRoutes mounts the browser self-service billing surface,
// authenticated by delegatedMW (middleware.DelegatedSelfRequired or
// middleware.DelegatedPrincipalRequired). It reuses the user-facing handlers —
// they read the acting user via r.GetUser(), which delegatedMW binds from the
// resolved principal — so every operation is scoped to the authenticated
// end-user and their merchant. No `:user_id` in any path: a browser token can
// only act on its own subject.
func RegisterSelfServiceRoutes(rr router.Router, rt *app.Runtime, delegatedMW router.Middleware, providerRoutes routesurface.ProviderRoutes) {
	registerSelfServiceRoutes(rr, rt, delegatedMW, providerRoutes, false)
}

// RegisterCustomerBillingManagementRoutes exposes the customer's existing
// billing history, access, payment methods and agreement management. Creating a
// checkout or selecting a different product/price remains with the host.
func RegisterCustomerBillingManagementRoutes(rr router.Router, rt *app.Runtime, delegatedMW router.Middleware, providerRoutes routesurface.ProviderRoutes) {
	registerSelfServiceRoutes(rr, rt, delegatedMW, providerRoutes, true)
}

func registerSelfServiceRoutes(rr router.Router, rt *app.Runtime, delegatedMW router.Middleware, providerRoutes routesurface.ProviderRoutes, managementOnly bool) {
	mw := []router.Middleware{delegatedMW}
	// Pin the request's merchant-scoped DB connection after the verified
	// principal resolves the merchant used by repository predicates.
	if rt != nil && rt.DB != nil {
		mw = append(mw, middleware.MerchantDBConnMW(rt.DB))
	}

	// or#930: the delegated invoker's own spend-window read is the ONE thing an
	// INVOKER-SCOPED principal may do — it spends the payer's money without being
	// the payer, so the bound subject names an account it does not own. This route
	// runs without PayerScopedRequired; everything below it runs with, so the
	// narrow credential class cannot reach the payer's money surface.
	rr.Group("", mw...).Handle(http.MethodGet, "/spend-limits", h(httphandlers.GetMySpendLimits))

	payerMW := make([]router.Middleware, 0, len(mw)+1)
	payerMW = append(payerMW, mw...)
	payerMW = append(payerMW, middleware.PayerScopedRequired())
	group := rr.Group("", payerMW...)
	registerCustomerSubscriptionManagement(group)

	// Customer money self-service.
	group.Handle(http.MethodGet, "/balance", h(httphandlers.GetMyBalance))
	group.Handle(http.MethodGet, "/transactions", h(httphandlers.GetMyAccountTransactions))
	group.Handle(http.MethodGet, "/status", h(httphandlers.GetMyBillingStatus))

	// Usage breakdown (#289) + invoices (#303), scoped to the token's subject.
	group.Handle(http.MethodGet, "/usage", h(httphandlers.GetMyUsage))
	group.Handle(http.MethodGet, "/invoices", h(httphandlers.GetMyInvoices))
	group.Handle(http.MethodGet, "/invoices/:id", h(httphandlers.GetMyInvoice))
	group.Handle(http.MethodPost, "/invoices/:id/pay-now", h(httphandlers.PayMyInvoiceNow))

	// Authentication continues the original accepted Stripe payment.
	group.Handle(http.MethodGet, "/payment-operations/:id/authentication", h(httphandlers.GetStripePaymentAuthentication))
	group.Handle(http.MethodPost, "/payment-operations/:id/authentication/confirm", h(httphandlers.ConfirmStripePaymentAuthentication))

	// Payment / transaction history.
	group.Handle(http.MethodGet, "/payments", h(httphandlers.GetUserPayments))
	group.Handle(http.MethodGet, "/entitlements/active", h(httphandlers.SelfGetActiveEntitlements))
	// Effective-tier resolution (or#912): ?group= is required.
	group.Handle(http.MethodGet, "/tier", h(httphandlers.GetMyTier))

	group.Handle(http.MethodGet, "/notifications", h(httphandlers.GetNotifications))
	group.Handle(http.MethodGet, "/notifications/unread-count", h(httphandlers.GetUnreadNotificationCount))
	group.Handle(http.MethodPost, "/notifications/:id/read", h(httphandlers.MarkNotificationRead))
	group.Handle(http.MethodGet, "/products", h(httphandlers.GetMyProducts))
	group.Handle(http.MethodGet, "/products/:product_id/access", h(httphandlers.GetMyProductAccess))

	// Subscriptions: every operation is scoped to the authenticated subject.
	subs := group.Group("/subscriptions")
	subs.Handle(http.MethodGet, "", h(httphandlers.GetMySubscriptions))
	subs.Handle(http.MethodGet, "/:id", h(httphandlers.GetSubscription))
	subs.Handle(http.MethodPost, "/:id/retry-now", h(httphandlers.RetryMySubscriptionNow))
	if !managementOnly {
		subs.Handle(http.MethodPost, "/:id/change-tier", h(httphandlers.ChangeTier))
		subs.Handle(http.MethodPost, "/:id/change-tier/preview", h(httphandlers.ChangeTierPreview))
		subs.Handle(http.MethodPost, "/:id/provider-cutover", h(httphandlers.MyProviderCutover))
		subs.Handle(http.MethodGet, "/:id/provider-cutover", h(httphandlers.MyProviderCutover))
		subs.Handle(http.MethodPost, "/:id/provider-cutover/preview", h(httphandlers.PreviewMyProviderCutover))
	}
	if providerRoutes.SolanaSigning {
		// App-driven on-chain cancel (#266/#271) and tier change (#272): the
		// prepare -> sign -> confirm -> mirror loops. Need an OpenRails signer (#661).
		subs.Handle(http.MethodPost, "/:id/solana-cancel-tx", h(httphandlers.PrepareSolanaCancelTx))
		subs.Handle(http.MethodPost, "/:id/solana-cancel", h(httphandlers.ConfirmSolanaCancel))
		if !managementOnly {
			subs.Handle(http.MethodPost, "/:id/solana-tier-change", h(httphandlers.PrepareSolanaTierChange))
			subs.Handle(http.MethodPost, "/:id/solana-tier-change/confirm", h(httphandlers.ConfirmSolanaTierChange))
		}
	}

	// Payment methods.
	pm := group.Group("/payment-methods")
	pm.Handle(http.MethodGet, "", h(httphandlers.ListPaymentMethods))
	pm.Handle(http.MethodPost, "/stripe-setup", h(httphandlers.CreateStripeMethodSetup))
	pm.Handle(http.MethodGet, "/stripe-setup/:id", h(httphandlers.GetStripeMethodSetup))
	pm.Handle(http.MethodPost, "/stripe-setup/:id/confirm", h(httphandlers.ConfirmStripeMethodSetup))
	pm.Handle(http.MethodPost, "", h(httphandlers.CreatePaymentMethod))
	pm.Handle(http.MethodPut, "/:id", h(httphandlers.UpdatePaymentMethod))
	pm.Handle(http.MethodDelete, "/:id", h(httphandlers.DeletePaymentMethod))

	// Checkout creation is disabled in management-only scope, but an app-created
	// session may still be read and confirmed by its signed-in customer. This
	// preserves the accepted quote and interactive payer proof without exposing a
	// generic browser purchase route.
	checkout := group.Group("/checkout")
	if !managementOnly {
		checkout.Handle(http.MethodPost, "", h(httphandlers.CreateCheckoutSession))
	}
	checkout.Handle(http.MethodGet, "/:id", h(httphandlers.GetCheckoutSession))
	checkout.Handle(http.MethodPost, "/:id/confirm", h(httphandlers.ConfirmCheckoutSession))

	if providerRoutes.StripePortal && !managementOnly {
		group.Handle(http.MethodPost, "/billing-portal", h(httphandlers.CreatePortalSession))
	}
}

// RegisterCustomerTreasuryRoutes mounts the customer-as-PAYER treasury surface
// (#567), deliberately separate from `/me` (the caller's OWN balance) and
// `/merchant` (seller operations). The customer is a PAYER, not a consumer: it
// holds a balance, pre-pays, owes in arrears, sets a billing mode, gets
// invoices, manages payment methods, and delegates spend — but owns no catalog
// products, entitlements, or personal subscriptions. Handlers are SHARED with
// `/v1/me/*`: CustomerScopeRequired confirms the :customer_id scope and rebinds
// the acting payer to the customer's payable subject. Every route is gated by a
// `customer:*` permission because the balance may be a SHARED resource.
func RegisterCustomerTreasuryRoutes(rr router.Router, rt *app.Runtime, delegatedMW router.Middleware, providerRoutes routesurface.ProviderRoutes) {
	// PayerScopedRequired (or#930): the treasury surface acts ON a payer account,
	// so an invoker-scoped principal — which spends a payer's money without being
	// the payer — is refused here outright, ahead of the customer:* gates.
	mw := []router.Middleware{delegatedMW, middleware.PayerScopedRequired(), middleware.CustomerScopeRequired()}
	if rt != nil && rt.DB != nil {
		mw = append(mw, middleware.MerchantDBConnMW(rt.DB))
	}
	group := rr.Group("", mw...)

	// Balance-sharing policy (#557): how the customer lets delegated
	// invokers/roles/tiers draw on its balance.
	group.Handle(http.MethodGet, "/:customer_id/spend-delegations",
		h(httphandlers.GetCustomerSpendDelegations),
		middleware.RequirePermission(permissions.CustomerSpendDelegationsRead),
	)
	putSpendDelegations := middleware.RequirePermission(permissions.CustomerSpendDelegationsUpdate)
	group.Handle(http.MethodPut, "/:customer_id/spend-delegations",
		h(httphandlers.PutCustomerSpendDelegations),
		putSpendDelegations,
	)
	group.Handle(http.MethodPut, "/:customer_id/spend-delegations:upsert",
		h(httphandlers.PutCustomerSpendDelegation),
		putSpendDelegations,
	)
	// or#911: single-grant revocation. One addressed delegation dies; every
	// sibling grant is untouched (a replace-all could clobber them).
	group.Handle(http.MethodDelete, "/:customer_id/spend-delegations/:scope/:scope_key",
		h(httphandlers.DeleteCustomerSpendDelegation),
		putSpendDelegations,
	)

	// Read the payer's money state. `status` is intentionally NOT mounted (it
	// reports consumer concepts the customer does not own).
	read := middleware.RequirePermission(permissions.CustomerBalanceRead)
	group.Handle(http.MethodGet, "/:customer_id/balance", h(httphandlers.GetMyBalance), read)
	group.Handle(http.MethodGet, "/:customer_id/transactions", h(httphandlers.GetMyAccountTransactions), read)
	group.Handle(http.MethodGet, "/:customer_id/usage", h(httphandlers.GetMyUsage), read)
	group.Handle(http.MethodGet, "/:customer_id/payments", h(httphandlers.GetUserPayments), read)
	group.Handle(http.MethodGet, "/:customer_id/invoices", h(httphandlers.GetMyInvoices), read)
	group.Handle(http.MethodGet, "/:customer_id/invoices/:id", h(httphandlers.GetMyInvoice), read)

	// Choose the payer's automatic invoice collection method per currency.
	group.Handle(http.MethodPut, "/:customer_id/collection-payment-method",
		h(httphandlers.SetMyCollectionPaymentMethod),
		middleware.RequirePermission(permissions.CustomerBillingUpdate),
	)

	// Manage the payer's saved payment methods and optional provider portal.
	pmPerm := middleware.RequirePermission(permissions.CustomerPaymentMethodsUpdate)
	group.Handle(http.MethodGet, "/:customer_id/payment-methods", h(httphandlers.ListPaymentMethods), pmPerm)
	group.Handle(http.MethodPost, "/:customer_id/payment-methods", h(httphandlers.CreatePaymentMethod), pmPerm)
	group.Handle(http.MethodPut, "/:customer_id/payment-methods/:id", h(httphandlers.UpdatePaymentMethod), pmPerm)
	group.Handle(http.MethodDelete, "/:customer_id/payment-methods/:id", h(httphandlers.DeletePaymentMethod), pmPerm)
	if providerRoutes.StripePortal {
		group.Handle(http.MethodPost, "/:customer_id/billing-portal", h(httphandlers.CreatePortalSession), pmPerm)
	}

	// Pre-pay / load credits onto the customer balance via checkout.
	checkoutPerm := middleware.RequirePermission(permissions.CustomerCheckoutCreate)
	group.Handle(http.MethodPost, "/:customer_id/checkout", h(httphandlers.CreateCheckoutSession), checkoutPerm)
	group.Handle(http.MethodGet, "/:customer_id/checkout/:id", h(httphandlers.GetCheckoutSession), checkoutPerm)
	group.Handle(http.MethodPost, "/:customer_id/checkout/:id/confirm", h(httphandlers.ConfirmCheckoutSession), checkoutPerm)
}

// RegisterCustomerSubscriptionManagementRoutes exposes only customer-owned
// cancellation, resumption and payment-method selection. It shares the full
// self-service route implementations and payer ownership gates.
func RegisterCustomerSubscriptionManagementRoutes(rr router.Router, rt *app.Runtime, delegatedMW router.Middleware) {
	mw := []router.Middleware{delegatedMW, middleware.PayerScopedRequired()}
	if rt != nil && rt.DB != nil {
		mw = append(mw, middleware.MerchantDBConnMW(rt.DB))
	}
	registerCustomerSubscriptionManagement(rr.Group("", mw...))
}

func registerCustomerSubscriptionManagement(group router.Router) {
	group.Handle(http.MethodPut, "/collection-payment-method", h(httphandlers.SetMyCollectionPaymentMethod))
	group.Handle(http.MethodPost, "/subscriptions/:id/cancel", h(httphandlers.CancelSubscription))
	group.Handle(http.MethodPost, "/subscriptions/:id/resume", h(httphandlers.ResumeSubscription))
	group.Handle(http.MethodPut, "/subscriptions/:id/payment-method", h(httphandlers.UpdateSubscriptionPaymentMethod))
}
