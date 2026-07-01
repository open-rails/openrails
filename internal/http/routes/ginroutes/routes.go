package ginroutes

import (
	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	httphandlers "github.com/open-rails/openrails/internal/http/handlers"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/request/ginreq"
	"github.com/open-rails/openrails/internal/http/routesurface"
)

// SelfRoutePrefix is the canonical browser self-service billing surface. The
// credential profile may be a delegated JWT in standalone mode or a host/user
// bearer in embedded mode; the URL is intentionally one stable `/me` surface
// and the credential profile lives on the resolved Principal.
const SelfRoutePrefix = "/me"

// CustomerRoutePrefix is the canonical customer-treasury surface. These routes
// are for a customer (any payer) acting over its OWN co-managed/shared balance,
// addressed by the customer's id — not for merchant/seller administration. The
// caller's own balance lives on the /me surface; this surface is the universal
// customer persona (#567).
const CustomerRoutePrefix = "/customers"

func wrapHandler(rt *app.Runtime, fn func(r *httprequest.Request)) gin.HandlerFunc {
	return func(c *gin.Context) {
		fn(ginreq.New(c, rt))
	}
}

// RegisterSelfServiceRoutes mounts the browser self-service billing surface on
// a merchant-scoped PUBLIC route group authenticated by a bearer principal. It
// reuses the existing user-facing handlers — they all read the acting user via
// r.GetUser(), which delegatedMW binds from the resolved Principal — so every
// operation is automatically scoped to the authenticated end-user and their
// merchant. There is no `:user_id` in any path: a browser token can only act on
// its own subject.
//
// delegatedMW must authenticate the delegated token (typically
// ginmw.DelegatedSelfRequired); it pins the resolved merchant + acting user +
// self-permissions onto the context that the per-route RequirePermission gates
// and the handlers read.
func RegisterSelfServiceRoutes(group *gin.RouterGroup, rt *app.Runtime, delegatedMW gin.HandlerFunc) {
	RegisterSelfServiceRoutesWithProviderRoutes(group, rt, delegatedMW, routesurface.AllProviderRoutes())
}

func RegisterSelfServiceRoutesWithProviderRoutes(group *gin.RouterGroup, rt *app.Runtime, delegatedMW gin.HandlerFunc, providerRoutes routesurface.ProviderRoutes) {
	wrap := func(fn func(r *httprequest.Request)) gin.HandlerFunc {
		return wrapHandler(rt, fn)
	}

	group.Use(delegatedMW)
	// Pin a merchant-scoped DB connection AFTER the delegated token resolves the
	// merchant, so RLS constrains every merchant-owned query (issue #227).
	if rt != nil && rt.DB != nil {
		group.Use(ginmw.MerchantDBConn(rt.DB))
	}

	// Customer money self-service. Customer routes operate only on the
	// authenticated subject; no self permissions or customer_id path params.
	group.GET("/balance", wrap(httphandlers.GetMyBalance))
	group.GET("/transactions", wrap(httphandlers.GetMyAccountTransactions))
	group.PUT("/settings", wrap(httphandlers.SetMyCreditAccountSettings))
	group.GET("/status", wrap(httphandlers.GetMyBillingStatus))

	// Usage breakdown (issue #289): the acting user's metered usage rolled up by
	// event_type (endpoint/model) over a [from, to) window, with summed dimensions.
	// Scoped to the token's subject + merchant like every other self route.
	group.GET("/usage", wrap(httphandlers.GetMyUsage))

	// Invoices (issue #303): the acting user's finalized monthly itemized
	// statements (newest first, paginated) and a single invoice with its line
	// items. Scoped to the token's subject + merchant like every other self route.
	group.GET("/invoices", wrap(httphandlers.GetMyInvoices))
	group.GET("/invoices/:id", wrap(httphandlers.GetMyInvoice))

	// Payment / transaction history.
	group.GET("/payments", wrap(httphandlers.GetUserPayments))
	group.GET("/entitlements/active", wrap(httphandlers.SelfGetActiveEntitlements))

	group.GET("/notifications", wrap(httphandlers.GetNotifications))
	group.GET("/notifications/unread-count", wrap(httphandlers.GetUnreadNotificationCount))
	group.POST("/notifications/:id/read", wrap(httphandlers.MarkNotificationRead))
	group.GET("/products", wrap(httphandlers.GetMyProducts))
	group.GET("/products/:product_id/access", wrap(httphandlers.GetMyProductAccess))

	// Subscriptions: every operation is scoped to the authenticated subject.
	subs := group.Group("/subscriptions")
	subs.GET("", wrap(httphandlers.GetMySubscriptions))
	subs.GET("/:id", wrap(httphandlers.GetSubscription))
	subs.POST("/:id/cancel", wrap(httphandlers.CancelSubscription))
	subs.POST("/:id/resume", wrap(httphandlers.ResumeSubscription))
	subs.POST("/:id/change-tier", wrap(httphandlers.ChangeTier))
	subs.POST("/:id/change-tier/preview", wrap(httphandlers.ChangeTierPreview))
	subs.PUT("/:id/payment-method", wrap(httphandlers.UpdateSubscriptionPaymentMethod))
	if providerRoutes.SolanaSigning {
		// App-driven on-chain cancel/revoke (#266/#271): the full prepare -> sign ->
		// confirm -> mirror loop. Needs an OpenRails signer, so gate on SolanaSigning (#661).
		subs.POST("/:id/solana-cancel-tx", wrap(httphandlers.PrepareSolanaCancelTx))
		subs.POST("/:id/solana-cancel", wrap(httphandlers.ConfirmSolanaCancel))
		// App-driven on-chain tier change (#272): the prepare -> sign -> confirm ->
		// mirror loop for changing tier on an existing Solana subscription.
		subs.POST("/:id/solana-tier-change", wrap(httphandlers.PrepareSolanaTierChange))
		subs.POST("/:id/solana-tier-change/confirm", wrap(httphandlers.ConfirmSolanaTierChange))
	}

	// Payment methods.
	pm := group.Group("/payment-methods")
	pm.GET("", wrap(httphandlers.ListPaymentMethods))
	pm.POST("", wrap(httphandlers.CreatePaymentMethod))
	pm.PUT("/:id", wrap(httphandlers.UpdatePaymentMethod))
	pm.DELETE("/:id", wrap(httphandlers.DeletePaymentMethod))

	// Checkout: create a session and read/confirm it (browser self-checkout).
	checkout := group.Group("/checkout")
	checkout.POST("", wrap(httphandlers.CreateCheckoutSession))
	checkout.GET("/:id", wrap(httphandlers.GetCheckoutSession))
	checkout.POST("/:id/confirm", wrap(httphandlers.ConfirmCheckoutSession))

	if providerRoutes.StripePortal {
		group.POST("/billing-portal", wrap(httphandlers.CreatePortalSession))
	}
}

// RegisterCustomerTreasuryRoutes mounts the customer-as-PAYER treasury surface
// (#567). It is deliberately separate
// from `/me` (the caller's OWN self-service balance) and `/merchant`
// (merchant/seller operations): a customer is a distinct top-level persona with
// its own `customer:*` namespace. The customer surface is addressed by the
// customer's id (`:customer_id`) so it serves co-managed/shared balances.
//
// A customer is a PAYER, not a consumer (#566/#567): it holds a balance,
// pre-pays, owes in arrears, sets a billing mode, gets invoices, manages payment
// methods, and delegates spend of its balance — but it does NOT own catalog
// products, entitlements, or personal subscriptions (its MEMBERS consume; the
// customer FUNDS). So this surface mirrors the payer subset of `/v1/me/*` and
// mounts NONE of the consumer routes.
//
// The handlers are SHARED with `/v1/me/*`: CustomerScopeRequired confirms the
// :customer_id scope and rebinds the acting payer to the customer's payable
// subject, so the same money handlers operate on the customer balance — route
// separation, not handler duplication. Every route is gated by a `customer:*`
// permission because the customer balance may be a SHARED resource (unlike
// `/v1/me`, which is authenticated-self and needs no grant). Every customer can
// delegate spend of its balance.
func RegisterCustomerTreasuryRoutes(group *gin.RouterGroup, rt *app.Runtime, delegatedMW gin.HandlerFunc, writeMW ...gin.HandlerFunc) {
	registerCustomerTreasuryRoutes(group, rt, delegatedMW, routesurface.AllProviderRoutes(), writeMW...)
}

func RegisterCustomerTreasuryRoutesWithProviderRoutes(group *gin.RouterGroup, rt *app.Runtime, delegatedMW gin.HandlerFunc, providerRoutes routesurface.ProviderRoutes, writeMW ...gin.HandlerFunc) {
	registerCustomerTreasuryRoutes(group, rt, delegatedMW, providerRoutes, writeMW...)
}

func registerCustomerTreasuryRoutes(group *gin.RouterGroup, rt *app.Runtime, delegatedMW gin.HandlerFunc, providerRoutes routesurface.ProviderRoutes, writeMW ...gin.HandlerFunc) {
	wrap := func(fn func(r *httprequest.Request)) gin.HandlerFunc {
		return wrapHandler(rt, fn)
	}

	group.Use(delegatedMW)
	// Scope to :customer_id and rebind the acting payer to the customer's payable
	// subject (#567) so the shared /v1/me money handlers operate on the customer
	// balance.
	group.Use(ginmw.CustomerScopeRequired())
	if rt != nil && rt.DB != nil {
		group.Use(ginmw.MerchantDBConn(rt.DB))
	}

	// Balance-sharing policy: how the customer lets delegated invokers/roles/tiers
	// draw on its balance (#557). Every customer can delegate spend.
	group.GET("/:customer_id/spend-delegations",
		ginmw.RequirePermission(controlplane.PermCustomerSpendDelegationsRead),
		wrap(httphandlers.GetCustomerSpendDelegations),
	)
	putSpendDelegations := append([]gin.HandlerFunc{
		ginmw.RequirePermission(controlplane.PermCustomerSpendDelegationsUpdate),
	}, writeMW...)
	putSpendDelegations = append(putSpendDelegations, wrap(httphandlers.PutCustomerSpendDelegations))
	group.PUT("/:customer_id/spend-delegations", putSpendDelegations...)

	// Read the payer's money state: balance, ledger transactions, metered usage,
	// payment history, and finalized invoices. `status` is intentionally NOT
	// mounted: the only /me status handler reports subscriptions + entitlements,
	// which are consumer concepts the customer does not own; balance + settings
	// already expose the payer's account state.
	read := ginmw.RequirePermission(controlplane.PermCustomerBalanceRead)
	group.GET("/:customer_id/balance", read, wrap(httphandlers.GetMyBalance))
	group.GET("/:customer_id/transactions", read, wrap(httphandlers.GetMyAccountTransactions))
	group.GET("/:customer_id/usage", read, wrap(httphandlers.GetMyUsage))
	group.GET("/:customer_id/payments", read, wrap(httphandlers.GetUserPayments))
	group.GET("/:customer_id/invoices", read, wrap(httphandlers.GetMyInvoices))
	group.GET("/:customer_id/invoices/:id", read, wrap(httphandlers.GetMyInvoice))

	// Set the payer's billing mode (prepaid|arrears) and self-imposed caps. The
	// shared handler already refuses platform-owned policy fields on this surface.
	group.PUT("/:customer_id/settings",
		ginmw.RequirePermission(controlplane.PermCustomerBillingUpdate),
		wrap(httphandlers.SetMyCreditAccountSettings),
	)

	// Manage the payer's saved payment methods and optional provider portal.
	pmPerm := ginmw.RequirePermission(controlplane.PermCustomerPaymentMethodsUpdate)
	group.GET("/:customer_id/payment-methods", pmPerm, wrap(httphandlers.ListPaymentMethods))
	group.POST("/:customer_id/payment-methods", pmPerm, wrap(httphandlers.CreatePaymentMethod))
	group.PUT("/:customer_id/payment-methods/:id", pmPerm, wrap(httphandlers.UpdatePaymentMethod))
	group.DELETE("/:customer_id/payment-methods/:id", pmPerm, wrap(httphandlers.DeletePaymentMethod))
	if providerRoutes.StripePortal {
		group.POST("/:customer_id/billing-portal", pmPerm, wrap(httphandlers.CreatePortalSession))
	}

	// Pre-pay / load credits onto the customer balance via checkout.
	checkoutPerm := ginmw.RequirePermission(controlplane.PermCustomerCheckoutCreate)
	group.POST("/:customer_id/checkout", checkoutPerm, wrap(httphandlers.CreateCheckoutSession))
	group.GET("/:customer_id/checkout/:id", checkoutPerm, wrap(httphandlers.GetCheckoutSession))
	group.POST("/:customer_id/checkout/:id/confirm", checkoutPerm, wrap(httphandlers.ConfirmCheckoutSession))
}
