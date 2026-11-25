package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
)

// priceMigrationErrors are a price migration's refusals, created or
// previewed; a creation is also a destructive command, admitted for every
// subscriber it counts.
var (
	priceMigrationErrors       = codes("invalid_param", "price_change_currency_mismatch", "price_change_target_archived", "price_key_not_found", "price_not_found", "product_not_found")
	priceMigrationCreateErrors = codes("catalog_revision_conflict", "idempotency_key_reused", "invalid_param", "price_change_currency_mismatch", "price_change_target_archived", "price_key_not_found", "price_migration_in_progress", "price_not_found", "product_not_found", "rate_limit_exceeded")
)

// subscriptionsRoutes is recurring agreements: reading, canceling, resuming
// and changing a subscription as the customer or as merchant staff, and the
// merchant's price migrations.
var subscriptionsRoutes = []Route{
	{Method: GET, Path: "/v1/admin/subscriptions", Group: Admin, Auth: AuthMerchant, Name: "ListSubscriptions", Level: LevelRead,
		Doc:   "ListSubscriptions returns one page of the merchant's subscriptions matching params, newest first.",
		Query: billing.SubscriptionListParams{}, Responses: []Reply{{200, billing.ListPage[billing.Subscription]{}}}, Errors: codes("invalid_cursor", "invalid_param", "invalid_query"), Handler: h(handlers.GetAdminSubscriptions)},
	{Method: GET, Path: "/v1/admin/subscriptions/{id}", Group: Admin, Auth: AuthMerchant, Name: "GetSubscription", Level: LevelRead,
		Args:      []any{billing.SubscriptionID{}},
		Doc:       "GetSubscription reads one subscription.",
		Responses: []Reply{{200, billing.Subscription{}}}, Errors: codes("invalid_param", "subscription_not_found"), Handler: h(handlers.GetAdminSubscription)},
	{Method: GET, Path: "/v1/admin/subscriptions/{id}/history", Group: Admin, Auth: AuthMerchant, Name: "GetSubscriptionHistory", Level: LevelRead,
		Args: []any{billing.SubscriptionID{}},
		Doc: "GetSubscriptionHistory reads retained subscription evidence, newest first. It uses the merchant's " +
			"subscription read permission, including when embedded.",
		Query: billing.PageRequest{}, Responses: []Reply{{200, billing.SubscriptionHistory{}}}, Errors: codes("invalid_cursor", "invalid_param", "invalid_query", "subscription_not_found"), Handler: h(handlers.GetSubscriptionHistory)},
	{Method: POST, Path: "/v1/admin/subscriptions/{id}/cancel", Group: Admin, Auth: AuthMerchant, Name: "CancelSubscription", Level: LevelUpdate,
		Args: []any{billing.SubscriptionID{}},
		Doc: "CancelSubscription cancels at the end of the paid period or, with RevokeAccess, immediately, and " +
			"returns the subscription. Both stop provider billing.",
		Request: handlers.AdminCancelSubscriptionRequest{}, Responses: []Reply{{200, billing.Subscription{}}}, Errors: codes("cancel_unsupported_on_rail", "invalid_param", "payment_in_progress", "provider_cancel_held", "resource_conflict", "subscription_not_active", "subscription_not_found"), Handler: h(handlers.AdminCancelSubscription)},
	{Method: POST, Path: "/v1/admin/subscriptions/{id}/resume", Group: Admin, Auth: AuthMerchant, Name: "ResumeSubscription", Level: LevelUpdate,
		Args: []any{billing.SubscriptionID{}},
		Doc: "ResumeSubscription undoes a scheduled cancel before the paid period ends and returns the " +
			"subscription.",
		Responses: []Reply{{200, billing.Subscription{}}}, Errors: codes("default_payment_method_required", "invalid_param", "payment_method_stale", "provider_writes_disabled", "service_unavailable", "solana_rpc_unavailable", "subscription_not_found"), Handler: h(handlers.AdminResumeSubscription)},
	{Method: POST, Path: "/v1/admin/subscriptions/{id}/change", Group: Admin, Auth: AuthMerchant, Name: "ChangeSubscription", Level: LevelUpdate, Limit: middleware.AdminOperationOffChannel, IdempotencyKey: true,
		Args: []any{billing.SubscriptionID{}},
		Doc: "ChangeSubscription moves a subscription to another price of its tier group, to other seats of a " +
			"per-seat price, or both. As staff it charges nothing: the change applies at the next renewal. " +
			"idempotencyKey is required: reuse it until the change resolves; the same key replays it.",
		Request: handlers.ChangeSubscriptionRequest{}, Responses: []Reply{{200, billing.SubscriptionChange{}}, {202, billing.SubscriptionChange{}}}, Errors: codes("card_declined", "customer_action_required", "customer_email_required", "invalid_param", "payment_duplicate_refused", "payment_method_stale", "payment_provider_rejected", "price_change_currency_mismatch", "price_not_found", "product_not_found", "quantity_not_allowed", "rebill_terms_committed", "resource_conflict", "resource_not_found", "scheduled_change_exists", "service_unavailable", "stored_credential_required", "subscription_change_already_scheduled", "subscription_change_credit_exceeds_price", "subscription_change_cycle_unknown", "subscription_change_idempotency_conflict", "subscription_change_idempotency_key_required", "subscription_change_in_flight", "subscription_change_period_unknown", "subscription_change_provider_conflict", "subscription_change_refused", "subscription_change_renewal_due", "subscription_change_requires_linked_plan", "subscription_change_target_inactive", "subscription_change_unsupported_on_rail", "subscription_not_active", "subscription_not_found"), Handler: h(handlers.AdminChangeSubscription)},
	{Method: GET, Path: "/v1/admin/subscriptions/{id}/change/options", Group: Admin, Auth: AuthMerchant, Name: "ListSubscriptionChangeOptions", Level: LevelRead,
		Args: []any{billing.SubscriptionID{}},
		Doc: "ListSubscriptionChangeOptions lists the prices staff can change a subscription to, each with what " +
			"the customer would pay from the next renewal. A target the change would refuse is left out.",
		Responses: []Reply{{200, billing.ListPage[billing.SubscriptionChangeOption]{}}}, Errors: codes("customer_action_required", "invalid_param", "resource_conflict", "resource_not_found", "service_unavailable", "subscription_change_already_scheduled", "subscription_change_in_flight", "subscription_not_active", "subscription_not_found"), Handler: h(handlers.AdminSubscriptionChangeOptions)},
	{Method: POST, Path: "/v1/admin/subscriptions/{id}/change/preview", Group: Admin, Auth: AuthMerchant, Name: "PreviewSubscriptionChange", Level: LevelRead,
		Args: []any{billing.SubscriptionID{}},
		Doc: "PreviewSubscriptionChange reports what changing a subscription's price or seats would charge and " +
			"when it would take effect, without changing anything. A staff change charges nothing and waits for " +
			"the next renewal.",
		Request: handlers.ChangeSubscriptionRequest{}, Responses: []Reply{{200, billing.SubscriptionChangePreview{}}}, Errors: codes("card_declined", "customer_action_required", "customer_email_required", "invalid_param", "payment_method_stale", "payment_provider_rejected", "price_change_currency_mismatch", "price_not_found", "product_not_found", "quantity_not_allowed", "resource_conflict", "resource_not_found", "service_unavailable", "subscription_change_already_scheduled", "subscription_change_credit_exceeds_price", "subscription_change_cycle_unknown", "subscription_change_in_flight", "subscription_change_period_unknown", "subscription_change_provider_conflict", "subscription_change_renewal_due", "subscription_change_requires_linked_plan", "subscription_change_target_inactive", "subscription_change_unsupported_on_rail", "subscription_not_active", "subscription_not_found"), Handler: h(handlers.AdminPreviewSubscriptionChange)},
	{Method: PUT, Path: "/v1/admin/subscriptions/{id}/payment-method", Group: Admin, Auth: AuthMerchant, Name: "SetSubscriptionPaymentMethod", Level: LevelUpdate,
		Args: []any{billing.SubscriptionID{}},
		Doc: "SetSubscriptionPaymentMethod gives a subscription its own card, another of the customer's saved " +
			"cards, or with a nil PaymentMethodID makes it follow the customer's default card for its currency, " +
			"and returns the subscription. Staff selection preserves the retry schedule and does not resume " +
			"stopped dunning; it is not an immediate collection request.",
		Request: handlers.UpdateSubscriptionPaymentMethodBody{}, Responses: []Reply{{200, billing.Subscription{}}}, Errors: codes("card_declined", "default_payment_method_required", "invalid_param", "payment_method_not_psp_vaulted", "payment_method_psp_mismatch", "payment_method_same_vault", "payment_method_stale", "payment_provider_rejected", "rebill_terms_committed", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.AdminUpdateSubscriptionPaymentMethod)},
	{Method: POST, Path: "/v1/catalog/price-migrations", Group: CatalogAdmin, Auth: AuthMerchant, Name: "CreatePriceMigration", Level: LevelUpdate,
		Doc: "CreatePriceMigration moves the subscribers of one price, or of every version of a price key, to " +
			"another price at each one's renewal. It admits every subscriber it counts before reading any and " +
			"reaches them a page at a time: preparing is true until the last page.",
		Request: billing.CreatePriceMigrationParams{}, Responses: []Reply{{201, billing.PriceMigration{}}}, Errors: priceMigrationCreateErrors, Handler: h(handlers.CreatePriceMigration)},
	{Method: POST, Path: "/v1/catalog/price-migrations/preview", Group: CatalogAdmin, Auth: AuthMerchant, Name: "PreviewPriceMigration", Level: LevelRead,
		Doc:     "PreviewPriceMigration answers what CreatePriceMigration would do, writing nothing.",
		Request: billing.CreatePriceMigrationParams{}, Responses: []Reply{{200, billing.PriceMigrationPreview{}}}, Errors: priceMigrationErrors, Handler: h(handlers.PreviewPriceMigration)},
	{Method: GET, Path: "/v1/catalog/price-migrations", Group: CatalogAdmin, Auth: AuthMerchant, Name: "ListPriceMigrations", Level: LevelRead,
		Doc:   "ListPriceMigrations lists the merchant's price migrations, newest first.",
		Query: billing.PriceMigrationListParams{}, Responses: []Reply{{200, billing.ListPage[billing.PriceMigration]{}}}, Errors: codes("invalid_cursor", "invalid_param", "invalid_query"), Handler: h(handlers.ListPriceMigrations)},
	{Method: GET, Path: "/v1/catalog/price-migrations/{id}", Group: CatalogAdmin, Auth: AuthMerchant, Name: "GetPriceMigration", Level: LevelRead,
		Args:      []any{billing.PriceMigrationID{}},
		Doc:       "GetPriceMigration reads one price migration with its moves counted.",
		Responses: []Reply{{200, billing.PriceMigration{}}}, Errors: codes("invalid_param", "price_migration_not_found"), Handler: h(handlers.GetPriceMigration)},
	{Method: POST, Path: "/v1/catalog/price-migrations/{id}/cancel", Group: CatalogAdmin, Auth: AuthMerchant, Name: "CancelPriceMigration", Level: LevelUpdate,
		Args:      []any{billing.PriceMigrationID{}},
		Doc:       "CancelPriceMigration cancels the migration's still-scheduled moves.",
		Responses: []Reply{{200, billing.PriceMigrationCancel{}}}, Errors: codes("invalid_param", "price_migration_not_found", "rebill_terms_committed"), Handler: h(handlers.CancelPriceMigration)},
	{Method: POST, Path: "/v1/me/subscriptions/{id}/cancel", Group: Customer, Auth: AuthCustomer,
		Request: handlers.CustomerCancelSubscriptionParams{}, Responses: []Reply{{200, billing.Subscription{}}}, Errors: codes("authentication_required", "cancel_unsupported_on_rail", "invalid_param", "payment_in_progress", "service_unavailable", "solana_rpc_unavailable", "solana_transaction_refused", "subscription_not_active", "subscription_not_found"), Handler: h(handlers.CancelSubscription)},
	{Method: POST, Path: "/v1/me/subscriptions/{id}/resume", Group: Customer, Auth: AuthCustomer,
		Responses: []Reply{{200, billing.Subscription{}}}, Errors: codes("authentication_required", "default_payment_method_required", "invalid_param", "payment_method_stale", "provider_writes_disabled", "service_unavailable", "solana_rpc_unavailable", "subscription_not_found"), Handler: h(handlers.ResumeSubscription)},
	{Method: PUT, Path: "/v1/me/subscriptions/{id}/payment-method", Group: Customer, Auth: AuthCustomer,
		Request: handlers.UpdateSubscriptionPaymentMethodBody{}, Responses: []Reply{{200, billing.Subscription{}}}, Errors: codes("authentication_required", "card_declined", "default_payment_method_required", "invalid_param", "payment_method_not_psp_vaulted", "payment_method_psp_mismatch", "payment_method_same_vault", "payment_method_stale", "payment_provider_rejected", "rebill_terms_committed", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.SetSubscriptionPaymentMethod)},
	{Method: GET, Path: "/v1/me/subscriptions", Group: Customer, Auth: AuthCustomer,
		Query: handlers.MySubscriptionListQuery{}, Responses: []Reply{{200, billing.ListPage[billing.Subscription]{}}}, Errors: codes("authentication_required", "invalid_cursor", "invalid_query"), Handler: h(handlers.GetMySubscriptions)},
	{Method: GET, Path: "/v1/me/subscriptions/{id}", Group: Customer, Auth: AuthCustomer,
		Responses: []Reply{{200, billing.Subscription{}}}, Errors: codes("authentication_required", "invalid_param", "resource_not_found", "subscription_not_found"), Handler: h(handlers.GetSubscription)},
	{Method: POST, Path: "/v1/me/subscriptions/{id}/retry-now", Group: Customer, Auth: AuthCustomer,
		Request: billing.RetrySubscriptionNowParams{}, Responses: []Reply{{200, billing.SubscriptionRetryNowResult{}}, {202, billing.SubscriptionRetryNowResult{}}}, Errors: codes("authentication_required", "card_attempts_blocked", "card_declined", "customer_action_required", "customer_payment_unsupported", "invalid_param", "invalid_payment_method", "payment_idempotency_conflict", "payment_in_progress", "payment_not_retryable", "payment_provider_rejected", "rebill_terms_committed", "resource_conflict", "resource_not_found", "service_unavailable", "subscription_not_found"), Handler: h(handlers.RetryMySubscriptionNow)},
	{Method: POST, Path: "/v1/me/subscriptions/{id}/change", Group: Customer, Auth: AuthCustomer, IdempotencyKey: true,
		Request: handlers.CustomerChangeSubscriptionParams{}, Responses: []Reply{{200, billing.SubscriptionChange{}}, {202, billing.SubscriptionChange{}}}, Errors: codes("authentication_required", "card_declined", "customer_action_required", "customer_email_required", "invalid_param", "payment_duplicate_refused", "payment_method_stale", "payment_provider_rejected", "price_change_currency_mismatch", "price_not_found", "product_not_found", "quantity_not_allowed", "rebill_terms_committed", "resource_conflict", "resource_not_found", "scheduled_change_exists", "service_unavailable", "solana_rpc_unavailable", "solana_transaction_refused", "subscription_change_already_scheduled", "subscription_change_credit_exceeds_price", "subscription_change_cycle_unknown", "subscription_change_idempotency_conflict", "subscription_change_idempotency_key_required", "subscription_change_in_flight", "subscription_change_period_unknown", "subscription_change_provider_conflict", "subscription_change_refused", "subscription_change_renewal_due", "subscription_change_requires_linked_plan", "subscription_change_target_inactive", "subscription_change_unsupported_on_rail", "subscription_not_active", "subscription_not_found"), Handler: h(handlers.ChangeSubscription)},
	{Method: POST, Path: "/v1/me/subscriptions/{id}/confirm", Group: Customer, Auth: AuthCustomer,
		Responses: []Reply{{200, billing.SubscriptionChange{}}, {202, billing.SubscriptionChange{}}}, Errors: codes("authentication_required", "card_declined", "customer_session_required", "invalid_param", "payment_not_found", "payment_provider_rejected", "resource_conflict", "resource_not_found", "service_unavailable", "subscription_change_refused", "subscription_not_found"), Handler: h(handlers.ConfirmSubscriptionChange)},
	{Method: GET, Path: "/v1/me/subscriptions/{id}/change/options", Group: Customer, Auth: AuthCustomer,
		Responses: []Reply{{200, billing.ListPage[billing.SubscriptionChangeOption]{}}}, Errors: codes("authentication_required", "invalid_param", "resource_conflict", "service_unavailable", "subscription_change_in_flight", "subscription_not_active", "subscription_not_found"), Handler: h(handlers.SubscriptionChangeOptions)},
	{Method: POST, Path: "/v1/me/subscriptions/{id}/change/preview", Group: Customer, Auth: AuthCustomer,
		Request: handlers.PreviewSubscriptionChangeParams{}, Responses: []Reply{{200, billing.SubscriptionChangePreview{}}}, Errors: codes("authentication_required", "card_declined", "customer_email_required", "invalid_param", "payment_method_stale", "payment_provider_rejected", "price_change_currency_mismatch", "price_not_found", "product_not_found", "quantity_not_allowed", "resource_conflict", "resource_not_found", "service_unavailable", "subscription_change_already_scheduled", "subscription_change_credit_exceeds_price", "subscription_change_cycle_unknown", "subscription_change_in_flight", "subscription_change_period_unknown", "subscription_change_provider_conflict", "subscription_change_renewal_due", "subscription_change_requires_linked_plan", "subscription_change_target_inactive", "subscription_change_unsupported_on_rail", "subscription_not_active", "subscription_not_found"), Handler: h(handlers.PreviewSubscriptionChange)},
}
