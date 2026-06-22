package ginroutes

import (
	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	httphandlers "github.com/open-rails/openrails/internal/http/handlers"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/request/ginreq"
)

// SelfRoutePrefix is the canonical browser self-service billing surface. The
// credential profile may be a delegated JWT in standalone mode or a host/user
// bearer in embedded mode; the URL is intentionally one stable `/me` surface
// and the credential profile lives on the resolved Principal.
const SelfRoutePrefix = "/me"

// OrgRoutePrefix is the canonical org-customer treasury surface. These routes
// are for an AuthKit org acting as a paying customer over its own org balance,
// not for merchant/seller administration and not for an individual's personal
// balance.
const OrgRoutePrefix = "/orgs"

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
	// App-driven on-chain cancel/revoke (#266/#271): the full prepare -> sign ->
	// confirm -> mirror loop. solana-cancel-tx builds the unsigned cancel tx the
	// wallet signs+sends; solana-cancel confirms the signature landed on-chain and
	// then mirrors the cancel into the DB (stops the cranker). Solana is the source
	// of truth — there is no DB-only "soft cancel".
	subs.POST("/:id/solana-cancel-tx", wrap(httphandlers.PrepareSolanaCancelTx))
	subs.POST("/:id/solana-cancel", wrap(httphandlers.ConfirmSolanaCancel))
	// App-driven on-chain tier change (#272): the prepare -> sign -> confirm ->
	// mirror loop for changing tier on an existing Solana subscription. prepare
	// returns the SINGLE ATOMIC cancel-old+subscribe-new tx (co-signed for an
	// upgrade's prorated transfer); confirm verifies it landed on-chain and mirrors
	// the switch into the DB (old cancelled, new active, next_pull_at per kind).
	subs.POST("/:id/solana-tier-change", wrap(httphandlers.PrepareSolanaTierChange))
	subs.POST("/:id/solana-tier-change/confirm", wrap(httphandlers.ConfirmSolanaTierChange))

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

	// Stripe customer portal handoff, moved from the old `/stripe/portal` raw
	// user route to the canonical self surface.
	group.POST("/stripe/portal", wrap(httphandlers.CreatePortalSession))
}

// RegisterOrgTreasuryRoutes mounts the org-as-PAYER treasury surface. It is
// deliberately separate from `/me` (personal self-service) and `/merchant`
// (merchant/seller operations): the same AuthKit org may own a merchant and
// also act as the paying customer for its own org balance, but those are
// distinct permission namespaces.
//
// An org is a PAYER, not a consumer (#566): it holds a balance, pre-pays, owes
// in arrears, sets a billing mode, gets invoices, and manages payment methods —
// but it does NOT own catalog products, entitlements, or personal subscriptions
// (its MEMBERS consume; the org FUNDS). So this surface mirrors the payer subset
// of `/v1/me/*` and mounts NONE of the consumer routes.
//
// The handlers are SHARED with `/v1/me/*`: OrgTreasuryScopeRequired confirms the
// :org_id scope and rebinds the acting payer to the org's payable subject, so the
// same money handlers operate on the org balance — route separation, not handler
// duplication. Every route is gated by a `customer:*` permission because the org
// balance is a SHARED resource (unlike `/v1/me`, which is authenticated-self and
// needs no grant).
func RegisterOrgTreasuryRoutes(group *gin.RouterGroup, rt *app.Runtime, delegatedMW gin.HandlerFunc) {
	wrap := func(fn func(r *httprequest.Request)) gin.HandlerFunc {
		return wrapHandler(rt, fn)
	}

	group.Use(delegatedMW)
	// Scope to :org_id and rebind the acting payer to the org's payable subject
	// (#566) so the shared /v1/me money handlers operate on the org balance.
	group.Use(ginmw.OrgTreasuryScopeRequired())
	if rt != nil && rt.DB != nil {
		group.Use(ginmw.MerchantDBConn(rt.DB))
	}

	// Balance-sharing policy: how the org lets delegated invokers/roles/tiers
	// draw on its balance (#557).
	group.GET("/:org_id/spend-delegations",
		ginmw.RequirePermission(controlplane.PermCustomerSpendDelegationsRead),
		wrap(httphandlers.GetOrgSpendDelegations),
	)
	group.PUT("/:org_id/spend-delegations",
		ginmw.RequirePermission(controlplane.PermCustomerSpendDelegationsUpdate),
		wrap(httphandlers.PutOrgSpendDelegations),
	)

	// Read the payer's money state: balance, ledger transactions, metered usage,
	// payment history, and finalized invoices. `status` is intentionally NOT
	// mounted: the only /me status handler reports subscriptions + entitlements,
	// which are consumer concepts the org does not own; balance + settings already
	// expose the payer's account state.
	read := ginmw.RequirePermission(controlplane.PermCustomerBalanceRead)
	group.GET("/:org_id/balance", read, wrap(httphandlers.GetMyBalance))
	group.GET("/:org_id/transactions", read, wrap(httphandlers.GetMyAccountTransactions))
	group.GET("/:org_id/usage", read, wrap(httphandlers.GetMyUsage))
	group.GET("/:org_id/payments", read, wrap(httphandlers.GetUserPayments))
	group.GET("/:org_id/invoices", read, wrap(httphandlers.GetMyInvoices))
	group.GET("/:org_id/invoices/:id", read, wrap(httphandlers.GetMyInvoice))

	// Set the payer's billing mode (prepaid|arrears) and self-imposed caps. The
	// shared handler already refuses platform-owned policy fields on this surface.
	group.PUT("/:org_id/settings",
		ginmw.RequirePermission(controlplane.PermCustomerBillingUpdate),
		wrap(httphandlers.SetMyCreditAccountSettings),
	)

	// Manage the payer's saved payment methods and Stripe billing portal.
	pmPerm := ginmw.RequirePermission(controlplane.PermCustomerPaymentMethodsUpdate)
	group.GET("/:org_id/payment-methods", pmPerm, wrap(httphandlers.ListPaymentMethods))
	group.POST("/:org_id/payment-methods", pmPerm, wrap(httphandlers.CreatePaymentMethod))
	group.PUT("/:org_id/payment-methods/:id", pmPerm, wrap(httphandlers.UpdatePaymentMethod))
	group.DELETE("/:org_id/payment-methods/:id", pmPerm, wrap(httphandlers.DeletePaymentMethod))
	group.POST("/:org_id/stripe/portal", pmPerm, wrap(httphandlers.CreatePortalSession))

	// Pre-pay / load credits onto the org balance via checkout.
	checkoutPerm := ginmw.RequirePermission(controlplane.PermCustomerCheckoutCreate)
	group.POST("/:org_id/checkout", checkoutPerm, wrap(httphandlers.CreateCheckoutSession))
	group.GET("/:org_id/checkout/:id", checkoutPerm, wrap(httphandlers.GetCheckoutSession))
	group.POST("/:org_id/checkout/:id/confirm", checkoutPerm, wrap(httphandlers.ConfirmCheckoutSession))
}
