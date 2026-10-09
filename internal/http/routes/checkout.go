package routes

import (
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/checkoutsession"
)

// checkoutRoutes is buying. A browser buys only through a checkout session:
// the signed-in customer (or the merchant's server) mints one, and the payment
// page reads and pays it by its id. A checkout attempt is one charge on one
// PSP; merchant automation creates them directly. Rail steps (a redirect, a
// Solana Pay link) are an attempt's next_action; Solana Pay's wallet requests
// address the attempt. Public discovery serves the checkout configuration and
// accepted Solana tokens.
var checkoutRoutes = []Route{
	{Method: GET, Path: "/v1/checkout-config", Group: Checkout, Auth: AuthPublic,
		Responses: []Reply{{200, merchants.PublicCheckoutConfig{}}}, Errors: codes("resource_not_found", "service_unavailable"), Handler: h(handlers.GetCheckoutConfig)},
	{Method: GET, Path: "/v1/solana/tokens", Group: Checkout, Auth: AuthPublic, When: FeatureSolana,
		Query: queryOf(handlers.SupportedTokensQuery{}), Responses: []Reply{{200, handlers.SupportedTokensResponse{}}}, Errors: codes("credential_custody_transition_required", "resource_conflict"), Handler: h(handlers.GetSupportedTokens)},
	{Method: GET, Path: "/v1/checkout-sessions/{id}", Group: Checkout, Auth: AuthCheckoutSession, Throttle: ThrottleSessionRead,
		Responses: []Reply{{200, checkoutsession.CheckoutSession{}}}, Errors: codes("card_attempts_blocked", "card_declined", "card_not_saved", "checkout_session_not_found", "checkout_session_unavailable", "credential_custody_transition_required", "idempotency_key_reused", "insufficient_funds", "invalid_param", "payment_method_required", "payment_method_stale", "payment_provider_rejected", "resource_access_denied", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.GetCheckoutSession)},
	{Method: POST, Path: "/v1/checkout-sessions/{id}/pay", Group: Checkout, Auth: AuthCheckoutSession, Throttle: ThrottleSessionPay,
		Request: checkoutsession.PayCheckoutSessionParams{}, Responses: []Reply{{200, checkoutsession.CheckoutSessionPayResult{}}}, Errors: codes("card_attempts_blocked", "card_declined", "card_not_saved", "card_requires_https", "checkout_offer_unavailable", "checkout_payment_in_progress", "checkout_request_invalid", "checkout_session_expired", "checkout_session_not_found", "checkout_session_unavailable", "credential_custody_transition_required", "idempotency_key_reused", "insufficient_funds", "invalid_param", "payment_method_required", "payment_method_stale", "payment_provider_rejected", "resource_access_denied", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.PayCheckoutSession)},
	{Method: GET, Path: "/v1/checkout-attempts/{id}/solana-pay", Group: Checkout, Auth: AuthSessionID, When: FeatureSolana,
		Responses: []Reply{{200, handlers.SolanaPayGetResponse{}}}, Errors: codes("checkout_attempt_expired", "invalid_param", "resource_conflict", "resource_not_found"), Handler: h(handlers.GetSolanaPay)},
	{Method: POST, Path: "/v1/checkout-attempts/{id}/solana-pay", Group: Checkout, Auth: AuthSessionID, When: FeatureSolana,
		Request: handlers.SolanaPayPostRequest{}, Responses: []Reply{{200, handlers.SolanaPayPostResponse{}}}, Errors: codes("checkout_attempt_expired", "invalid_param", "resource_conflict", "resource_not_found"), Handler: h(handlers.PostSolanaPay)},
	{Method: POST, Path: "/v1/me/checkout-sessions", Group: Customer, Auth: AuthCustomer,
		Request: handlers.MintCheckoutSessionParams{}, Responses: []Reply{{201, billing.CheckoutSessionLink{}}}, Errors: codes("authentication_required", "checkout_offer_unavailable", "checkout_session_unavailable", "customer_action_required", "invalid_param", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.CreateCheckoutSession)},
	{Method: POST, Path: "/v1/merchant/checkout-sessions", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCheckoutCreate,
		Request: billing.CreateCheckoutSessionParams{}, Responses: []Reply{{201, billing.CheckoutSessionLink{}}}, Errors: codes("authentication_required", "checkout_offer_unavailable", "checkout_session_unavailable", "invalid_param", "resource_access_denied", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.ServiceCreateCheckoutSession)},
	{Method: POST, Path: "/v1/merchant/checkout-attempts", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCheckoutCreate, IdempotencyKey: true,
		Request: billing.CreateCheckoutAttemptParams{}, Responses: []Reply{{200, billing.CheckoutAttempt{}}}, Errors: codes("authentication_required", "card_attempts_blocked", "card_declined", "card_not_saved", "checkout_attempt_closed", "idempotency_key_reused", "insufficient_funds", "invalid_param", "payment_method_required", "payment_method_stale", "payment_provider_rejected", "resource_access_denied", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.ServiceCreateCheckoutAttempt)},
	{Method: GET, Path: "/v1/merchant/checkout-attempts/{id}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Responses: []Reply{{200, billing.CheckoutAttempt{}}}, Errors: codes("authentication_required", "checkout_attempt_expired", "invalid_param", "resource_access_denied", "resource_not_found", "service_unavailable"), Handler: h(handlers.ServiceGetCheckoutAttempt)},
	{Method: POST, Path: "/v1/merchant/checkout-attempts/{id}/confirm", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCheckoutCreate,
		Request: billing.ConfirmCheckoutAttemptParams{}, Responses: []Reply{{200, billing.CheckoutAttempt{}}, {202, billing.CheckoutAttempt{}}}, Errors: codes("authentication_required", "checkout_attempt_expired", "insufficient_funds", "invalid_param", "resource_access_denied", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.ServiceConfirmCheckoutAttempt)},
	{Method: GET, Path: "/v1/merchant/checkout-config", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Query: params(text("price_id"), text("product_key"), text("price_key")), Responses: []Reply{{200, merchants.PublicCheckoutConfig{}}}, Errors: codes("invalid_param", "resource_not_found", "service_unavailable"), Handler: h(handlers.ServiceGetCheckoutConfig)},

	// Captcha discovery: whether this caller must solve one, and the script
	// that does. The assembly builds both from its captcha configuration.
	{Method: GET, Path: "/v1/captcha/status", Group: Checkout, Auth: AuthPublic, NoConn: true,
		Responses: []Reply{{200, billing.CaptchaStatus{}}}, Bind: external(func(x *External) http.Handler { return x.CaptchaStatus })},
	{Method: GET, Path: "/v1/captcha/client.js", Group: Checkout, Auth: AuthPublic, NoConn: true,
		Responses: []Reply{{200, Stream{"application/javascript"}}}, Bind: external(func(x *External) http.Handler { return x.CaptchaScript })},
}
