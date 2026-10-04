package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// subscriptionsRoutes is recurring agreements: reading, canceling, resuming
// and changing a subscription as the customer or as merchant staff, and the
// merchant's bulk moves (reprices, plan migrations, PSP cutovers).
var subscriptionsRoutes = []Route{
	{Method: GET, Path: "/v1/merchant/subscriptions", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSubscriptionsRead,
		Query: params(queryOf(subscriptions.GetSubscriptionsFilters{}), text("cursor"), integer("limit")), Responses: []Reply{{200, billing.ListPage[billing.Subscription]{}}}, Errors: codes("catalog_scope_mismatch", "invalid_cursor", "invalid_param", "invalid_query"), Handler: h(handlers.GetAdminSubscriptions)},
	{Method: GET, Path: "/v1/merchant/subscriptions/{id}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSubscriptionsRead,
		Responses: []Reply{{200, billing.Subscription{}}}, Errors: codes("catalog_scope_mismatch", "invalid_param", "subscription_not_found"), Handler: h(handlers.GetAdminSubscription)},
	{Method: POST, Path: "/v1/merchant/subscriptions/{id}/cancel", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSubscriptionsUpdate, Limit: middleware.AdminOperationDestructive,
		Request: handlers.AdminCancelSubscriptionRequest{}, Responses: []Reply{{200, billing.Subscription{}}}, Errors: codes("cancel_unsupported_on_rail", "customer_action_required", "invalid_param", "provider_cancel_held", "resource_conflict", "subscription_not_active", "subscription_not_found"), Handler: h(handlers.AdminCancelSubscription)},
	{Method: POST, Path: "/v1/merchant/subscriptions/{id}/resume", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSubscriptionsUpdate,
		Responses: []Reply{{200, billing.Subscription{}}}, Errors: codes("invalid_param", "subscription_not_found"), Handler: h(handlers.AdminResumeSubscription)},
	{Method: POST, Path: "/v1/merchant/subscriptions/{id}/change-tier", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSubscriptionsUpdate, Limit: middleware.AdminOperationOffChannel, IdempotencyKey: true,
		Request: handlers.ChangeTierRequest{}, Responses: []Reply{{200, billing.TierChange{}}, {202, billing.TierChange{}}}, Errors: codes("card_declined", "catalog_scope_mismatch", "customer_action_required", "invalid_param", "payment_method_stale", "payment_provider_rejected", "rebill_terms_committed", "reprice_already_scheduled", "reprice_cross_currency", "resource_conflict", "resource_not_found", "tier_change_already_scheduled", "tier_change_in_flight"), Handler: h(handlers.AdminChangeTier)},
	{Method: POST, Path: "/v1/merchant/subscriptions/{id}/change-tier/preview", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSubscriptionsUpdate,
		Request: handlers.ChangeTierRequest{}, Responses: []Reply{{200, billing.TierChangePreview{}}}, Errors: codes("card_declined", "catalog_scope_mismatch", "customer_action_required", "invalid_param", "payment_method_stale", "payment_provider_rejected", "reprice_cross_currency", "resource_conflict", "resource_not_found", "tier_change_already_scheduled", "tier_change_in_flight"), Handler: h(handlers.AdminChangeTierPreview)},
	{Method: PUT, Path: "/v1/merchant/subscriptions/{id}/payment-method", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSubscriptionsUpdate,
		Request: handlers.UpdateSubscriptionPaymentMethodBody{}, Responses: []Reply{{200, billing.Subscription{}}}, Errors: codes("invalid_param", "payment_method_not_psp_vaulted", "payment_method_psp_mismatch", "payment_method_same_vault", "rebill_terms_committed", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.AdminUpdateSubscriptionPaymentMethod)},
	{Method: POST, Path: "/v1/merchant/subscriptions/{id}/provider-cutover", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSubscriptionsUpdate,
		Query: params(text("idempotency_key")), Request: billing.ProviderCutoverRequest{}, Responses: []Reply{{200, billing.ProviderCutover{}}, {202, billing.ProviderCutover{}}}, Errors: codes("rate_limit_exceeded", "rebill_terms_committed", "resource_conflict"), Handler: h(handlers.ProviderCutover)},
	{Method: GET, Path: "/v1/merchant/subscriptions/{id}/provider-cutover", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSubscriptionsRead,
		Query: params(text("idempotency_key")), Responses: []Reply{{200, billing.ProviderCutover{}}, {202, billing.ProviderCutover{}}}, Errors: codes("rate_limit_exceeded", "rebill_terms_committed", "resource_conflict"), Handler: h(handlers.ProviderCutover)},
	{Method: POST, Path: "/v1/merchant/subscriptions/{id}/provider-cutover/preview", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSubscriptionsRead,
		Query: params(text("idempotency_key")), Request: billing.ProviderCutoverRequest{}, Responses: []Reply{{200, billing.ProviderCutover{}}}, Errors: codes("rate_limit_exceeded", "rebill_terms_committed", "resource_conflict"), Handler: h(handlers.PreviewProviderCutover)},
	{Method: POST, Path: "/v1/merchant/provider-refresh", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSubscriptionsUpdate,
		Responses: []Reply{{202, billing.ProviderRefresh{}}}, Errors: codes("service_unavailable"), Handler: h(handlers.RefreshProviders)},
	{Method: POST, Path: "/v1/merchant/reprice-batches", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSubscriptionsUpdate,
		Request: billing.CreateRepriceBatchParams{}, Responses: []Reply{{201, billing.RepriceBatchResult{}}}, Errors: codes("catalog_scope_mismatch", "invalid_param", "rebill_terms_committed", "reprice_already_scheduled", "reprice_cross_currency", "reprice_cross_product", "reprice_inactive_price", "reprice_not_scheduled", "reprice_notice_window_violation", "reprice_price_key_not_found"), Handler: h(handlers.CreateRepriceBatch)},
	{Method: POST, Path: "/v1/merchant/reprice-batches/preview", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSubscriptionsRead,
		Request: billing.RepriceBatchPreviewParams{}, Responses: []Reply{{200, billing.RepriceBatchPreview{}}}, Errors: codes("catalog_scope_mismatch", "invalid_param", "reprice_price_key_not_found"), Handler: h(handlers.PreviewRepriceBatch)},
	{Method: GET, Path: "/v1/merchant/reprice-batches", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSubscriptionsRead,
		Query: params(queryOf(handlers.RepriceBatchQuery{}), text("cursor"), integer("limit")), Responses: []Reply{{200, billing.ListPage[billing.RepriceBatch]{}}}, Errors: codes("invalid_cursor", "invalid_query"), Handler: h(handlers.ListRepriceBatches)},
	{Method: GET, Path: "/v1/merchant/reprice-batches/{id}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSubscriptionsRead,
		Responses: []Reply{{200, billing.RepriceBatch{}}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.GetRepriceBatch)},
	{Method: POST, Path: "/v1/merchant/reprice-batches/{id}/cancel", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSubscriptionsUpdate,
		Responses: []Reply{{200, billing.RepriceBatchCancel{}}}, Errors: codes("invalid_param", "rebill_terms_committed", "resource_not_found"), Handler: h(handlers.CancelRepriceBatch)},
	{Method: POST, Path: "/v1/merchant/plan-migrations", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSubscriptionsUpdate,
		Request: billing.PlanMigrationRequest{}, Responses: []Reply{{201, billing.PlanMigrationResult{}}}, Errors: codes("catalog_scope_mismatch", "invalid_param", "rebill_terms_committed", "reprice_already_scheduled", "reprice_cross_currency", "reprice_inactive_price", "reprice_not_scheduled", "reprice_notice_window_violation", "resource_not_found"), Handler: h(handlers.CreatePlanMigration)},
	{Method: POST, Path: "/v1/merchant/plan-migrations/preview", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSubscriptionsRead,
		Request: billing.PlanMigrationRequest{}, Responses: []Reply{{200, billing.PlanMigrationResult{}}}, Errors: codes("catalog_scope_mismatch", "invalid_param", "reprice_already_scheduled", "reprice_cross_currency", "reprice_inactive_price", "reprice_notice_window_violation", "resource_not_found"), Handler: h(handlers.PreviewPlanMigration)},
	{Method: GET, Path: "/v1/merchant/reprices", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSubscriptionsRead,
		Query: params(queryOf(handlers.RepriceQuery{}), text("cursor"), integer("limit")), Responses: []Reply{{200, billing.ListPage[billing.Reprice]{}}}, Errors: codes("invalid_cursor", "invalid_query"), Handler: h(handlers.ListReprices)},
	{Method: GET, Path: "/v1/merchant/reprices/{id}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSubscriptionsRead,
		Responses: []Reply{{200, billing.Reprice{}}}, Errors: codes("invalid_param", "reprice_not_found"), Handler: h(handlers.GetReprice)},
	{Method: POST, Path: "/v1/merchant/reprices/{id}/cancel", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSubscriptionsUpdate,
		Responses: []Reply{{200, billing.Reprice{}}}, Errors: codes("invalid_param", "rebill_terms_committed", "reprice_not_found", "reprice_not_scheduled"), Handler: h(handlers.CancelReprice)},
	{Method: POST, Path: "/v1/me/subscriptions/{id}/cancel", Group: Customer, Auth: AuthCustomer, Scope: ScopeSubscriptionManagement,
		Request: handlers.CustomerCancelSubscriptionRequest{}, Responses: []Reply{{200, billing.Subscription{}}}, Errors: codes("authentication_required", "cancel_unsupported_on_rail", "invalid_param", "provider_cancel_held", "service_unavailable", "subscription_not_active", "subscription_not_found"), Handler: h(handlers.CancelSubscription)},
	{Method: POST, Path: "/v1/me/subscriptions/{id}/resume", Group: Customer, Auth: AuthCustomer, Scope: ScopeSubscriptionManagement,
		Responses: []Reply{{200, billing.Subscription{}}}, Errors: codes("authentication_required", "invalid_param", "service_unavailable", "subscription_not_found"), Handler: h(handlers.ResumeSubscription)},
	{Method: PUT, Path: "/v1/me/subscriptions/{id}/payment-method", Group: Customer, Auth: AuthCustomer, Scope: ScopeSubscriptionManagement,
		Request: handlers.UpdateSubscriptionPaymentMethodBody{}, Responses: []Reply{{200, billing.Subscription{}}}, Errors: codes("authentication_required", "invalid_param", "payment_method_not_psp_vaulted", "payment_method_psp_mismatch", "payment_method_same_vault", "rebill_terms_committed", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.UpdateSubscriptionPaymentMethod)},
	{Method: GET, Path: "/v1/me/subscriptions", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Query: params(queryOf(handlers.MySubscriptionsQuery{}), text("cursor"), integer("limit")), Responses: []Reply{{200, billing.ListPage[billing.Subscription]{}}}, Errors: codes("authentication_required", "catalog_scope_mismatch", "invalid_cursor", "invalid_query"), Handler: h(handlers.GetMySubscriptions)},
	{Method: GET, Path: "/v1/me/subscriptions/{id}", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Responses: []Reply{{200, billing.Subscription{}}}, Errors: codes("authentication_required", "catalog_scope_mismatch", "invalid_param", "resource_not_found", "subscription_not_found"), Handler: h(handlers.GetSubscription)},
	{Method: POST, Path: "/v1/me/subscriptions/{id}/retry-now", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Request: billing.RetrySubscriptionNowRequest{}, Responses: []Reply{{200, billing.SubscriptionRetryNowResult{}}, {202, billing.SubscriptionRetryNowResult{}}}, Errors: codes("authentication_required", "card_declined", "catalog_scope_mismatch", "customer_action_required", "customer_payment_unsupported", "invalid_param", "invalid_payment_method", "payment_idempotency_conflict", "payment_in_progress", "payment_not_retryable", "payment_provider_rejected", "rebill_terms_committed", "resource_conflict", "resource_not_found", "subscription_not_found"), Handler: h(handlers.RetryMySubscriptionNow)},
	{Method: POST, Path: "/v1/me/subscriptions/{id}/change-tier", Group: Customer, Auth: AuthCustomer, IdempotencyKey: true,
		Request: handlers.CustomerChangeTierRequest{}, Responses: []Reply{{200, billing.TierChange{}}, {202, billing.TierChange{}}}, Errors: codes("authentication_required", "card_declined", "catalog_scope_mismatch", "customer_action_required", "invalid_param", "payment_method_stale", "payment_provider_rejected", "rebill_terms_committed", "reprice_already_scheduled", "reprice_cross_currency", "resource_conflict", "resource_not_found", "tier_change_in_flight"), Handler: h(handlers.ChangeTier)},
	{Method: POST, Path: "/v1/me/subscriptions/{id}/change-tier/preview", Group: Customer, Auth: AuthCustomer,
		Request: handlers.ChangeTierRequest{}, Responses: []Reply{{200, billing.TierChangePreview{}}}, Errors: codes("authentication_required", "card_declined", "catalog_scope_mismatch", "invalid_param", "payment_method_stale", "payment_provider_rejected", "reprice_cross_currency", "resource_conflict", "resource_not_found", "tier_change_in_flight"), Handler: h(handlers.ChangeTierPreview)},
}
