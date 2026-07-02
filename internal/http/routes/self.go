package routes

import (
	"net/http"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
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
	mw := []router.Middleware{delegatedMW}
	// Pin a merchant-scoped DB connection AFTER the delegated token resolves the
	// merchant, so RLS constrains every merchant-owned query (#227).
	if rt != nil && rt.DB != nil {
		mw = append(mw, middleware.MerchantDBConnMW(rt.DB))
	}
	group := rr.Group("", mw...)

	// Customer money self-service.
	group.Handle(http.MethodGet, "/balance", h(httphandlers.GetMyBalance))
	group.Handle(http.MethodGet, "/transactions", h(httphandlers.GetMyAccountTransactions))
	group.Handle(http.MethodPut, "/settings", h(httphandlers.SetMyCreditAccountSettings))
	group.Handle(http.MethodGet, "/status", h(httphandlers.GetMyBillingStatus))

	// Usage breakdown (#289) + invoices (#303), scoped to the token's subject.
	group.Handle(http.MethodGet, "/usage", h(httphandlers.GetMyUsage))
	group.Handle(http.MethodGet, "/invoices", h(httphandlers.GetMyInvoices))
	group.Handle(http.MethodGet, "/invoices/:id", h(httphandlers.GetMyInvoice))

	// Payment / transaction history.
	group.Handle(http.MethodGet, "/payments", h(httphandlers.GetUserPayments))
	group.Handle(http.MethodGet, "/entitlements/active", h(httphandlers.SelfGetActiveEntitlements))

	group.Handle(http.MethodGet, "/notifications", h(httphandlers.GetNotifications))
	group.Handle(http.MethodGet, "/notifications/unread-count", h(httphandlers.GetUnreadNotificationCount))
	group.Handle(http.MethodPost, "/notifications/:id/read", h(httphandlers.MarkNotificationRead))
	group.Handle(http.MethodGet, "/products", h(httphandlers.GetMyProducts))
	group.Handle(http.MethodGet, "/products/:product_id/access", h(httphandlers.GetMyProductAccess))

	// Subscriptions: every operation is scoped to the authenticated subject.
	subs := group.Group("/subscriptions")
	subs.Handle(http.MethodGet, "", h(httphandlers.GetMySubscriptions))
	subs.Handle(http.MethodGet, "/:id", h(httphandlers.GetSubscription))
	subs.Handle(http.MethodPost, "/:id/cancel", h(httphandlers.CancelSubscription))
	subs.Handle(http.MethodPost, "/:id/resume", h(httphandlers.ResumeSubscription))
	subs.Handle(http.MethodPost, "/:id/change-tier", h(httphandlers.ChangeTier))
	subs.Handle(http.MethodPost, "/:id/change-tier/preview", h(httphandlers.ChangeTierPreview))
	subs.Handle(http.MethodPut, "/:id/payment-method", h(httphandlers.UpdateSubscriptionPaymentMethod))
	if providerRoutes.SolanaSigning {
		// App-driven on-chain cancel (#266/#271) and tier change (#272): the
		// prepare -> sign -> confirm -> mirror loops. Need an OpenRails signer (#661).
		subs.Handle(http.MethodPost, "/:id/solana-cancel-tx", h(httphandlers.PrepareSolanaCancelTx))
		subs.Handle(http.MethodPost, "/:id/solana-cancel", h(httphandlers.ConfirmSolanaCancel))
		subs.Handle(http.MethodPost, "/:id/solana-tier-change", h(httphandlers.PrepareSolanaTierChange))
		subs.Handle(http.MethodPost, "/:id/solana-tier-change/confirm", h(httphandlers.ConfirmSolanaTierChange))
	}

	// Payment methods.
	pm := group.Group("/payment-methods")
	pm.Handle(http.MethodGet, "", h(httphandlers.ListPaymentMethods))
	pm.Handle(http.MethodPost, "", h(httphandlers.CreatePaymentMethod))
	pm.Handle(http.MethodPut, "/:id", h(httphandlers.UpdatePaymentMethod))
	pm.Handle(http.MethodDelete, "/:id", h(httphandlers.DeletePaymentMethod))

	// Checkout: create a session and read/confirm it (browser self-checkout).
	checkout := group.Group("/checkout")
	checkout.Handle(http.MethodPost, "", h(httphandlers.CreateCheckoutSession))
	checkout.Handle(http.MethodGet, "/:id", h(httphandlers.GetCheckoutSession))
	checkout.Handle(http.MethodPost, "/:id/confirm", h(httphandlers.ConfirmCheckoutSession))

	if providerRoutes.StripePortal {
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
func RegisterCustomerTreasuryRoutes(rr router.Router, rt *app.Runtime, delegatedMW router.Middleware, providerRoutes routesurface.ProviderRoutes, writeMW ...router.Middleware) {
	mw := []router.Middleware{delegatedMW, middleware.CustomerScopeRequired()}
	if rt != nil && rt.DB != nil {
		mw = append(mw, middleware.MerchantDBConnMW(rt.DB))
	}
	group := rr.Group("", mw...)

	// Balance-sharing policy (#557): how the customer lets delegated
	// invokers/roles/tiers draw on its balance.
	group.Handle(http.MethodGet, "/:customer_id/spend-delegations",
		h(httphandlers.GetCustomerSpendDelegations),
		middleware.RequirePermission(controlplane.PermCustomerSpendDelegationsRead),
	)
	putSpendDelegations := append([]router.Middleware{
		middleware.RequirePermission(controlplane.PermCustomerSpendDelegationsUpdate),
	}, writeMW...)
	group.Handle(http.MethodPut, "/:customer_id/spend-delegations",
		h(httphandlers.PutCustomerSpendDelegations),
		putSpendDelegations...,
	)

	// Read the payer's money state. `status` is intentionally NOT mounted (it
	// reports consumer concepts the customer does not own).
	read := middleware.RequirePermission(controlplane.PermCustomerBalanceRead)
	group.Handle(http.MethodGet, "/:customer_id/balance", h(httphandlers.GetMyBalance), read)
	group.Handle(http.MethodGet, "/:customer_id/transactions", h(httphandlers.GetMyAccountTransactions), read)
	group.Handle(http.MethodGet, "/:customer_id/usage", h(httphandlers.GetMyUsage), read)
	group.Handle(http.MethodGet, "/:customer_id/payments", h(httphandlers.GetUserPayments), read)
	group.Handle(http.MethodGet, "/:customer_id/invoices", h(httphandlers.GetMyInvoices), read)
	group.Handle(http.MethodGet, "/:customer_id/invoices/:id", h(httphandlers.GetMyInvoice), read)

	// Set the payer's billing mode (prepaid|arrears) and self-imposed caps.
	group.Handle(http.MethodPut, "/:customer_id/settings",
		h(httphandlers.SetMyCreditAccountSettings),
		middleware.RequirePermission(controlplane.PermCustomerBillingUpdate),
	)

	// Manage the payer's saved payment methods and optional provider portal.
	pmPerm := middleware.RequirePermission(controlplane.PermCustomerPaymentMethodsUpdate)
	group.Handle(http.MethodGet, "/:customer_id/payment-methods", h(httphandlers.ListPaymentMethods), pmPerm)
	group.Handle(http.MethodPost, "/:customer_id/payment-methods", h(httphandlers.CreatePaymentMethod), pmPerm)
	group.Handle(http.MethodPut, "/:customer_id/payment-methods/:id", h(httphandlers.UpdatePaymentMethod), pmPerm)
	group.Handle(http.MethodDelete, "/:customer_id/payment-methods/:id", h(httphandlers.DeletePaymentMethod), pmPerm)
	if providerRoutes.StripePortal {
		group.Handle(http.MethodPost, "/:customer_id/billing-portal", h(httphandlers.CreatePortalSession), pmPerm)
	}

	// Pre-pay / load credits onto the customer balance via checkout.
	checkoutPerm := middleware.RequirePermission(controlplane.PermCustomerCheckoutCreate)
	group.Handle(http.MethodPost, "/:customer_id/checkout", h(httphandlers.CreateCheckoutSession), checkoutPerm)
	group.Handle(http.MethodGet, "/:customer_id/checkout/:id", h(httphandlers.GetCheckoutSession), checkoutPerm)
	group.Handle(http.MethodPost, "/:customer_id/checkout/:id/confirm", h(httphandlers.ConfirmCheckoutSession), checkoutPerm)
}
