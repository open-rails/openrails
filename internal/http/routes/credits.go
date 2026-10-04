package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/service"
)

// creditsRoutes is a customer's prepaid money: balances, credit grants and
// their ledger, the credit line and trust level, and how a customer lets
// others spend its balance (spend delegations).
var creditsRoutes = []Route{
	{Method: PUT, Path: "/v1/merchant/customers/{customer_id}/spend-delegations", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsUpdate,
		Request: handlers.CustomerSpendDelegationsDocument{}, Responses: []Reply{{200, handlers.CustomerSpendDelegationsDocument{}}}, Errors: codes("authentication_required", "invalid_param"), Handler: h(handlers.ServicePutCustomerSpendDelegations)},
	{Method: PUT, Path: "/v1/merchant/customers/{customer_id}/spend-delegations:upsert", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsUpdate,
		Request: handlers.CustomerSpendDelegation{}, Responses: []Reply{{200, handlers.CustomerSpendDelegation{}}}, Errors: codes("authentication_required", "invalid_param"), Handler: h(handlers.ServicePutCustomerSpendDelegation)},
	{Method: DELETE, Path: "/v1/merchant/customers/{customer_id}/spend-delegations/{scope}/{scope_key}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsUpdate,
		Responses: []Reply{{200, Untyped{}}}, Errors: codes("authentication_required", "invalid_param"), Handler: h(handlers.ServiceDeleteCustomerSpendDelegation)},
	{Method: GET, Path: "/v1/merchant/invokers/{invoker}/credits", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Query: params(text("currency"), text("customer_id")), Responses: []Reply{{200, Untyped{}}}, Errors: codes("authentication_required", "currency_unsupported", "invalid_param", "resource_not_found"), Handler: h(handlers.ServiceGetInvokerCredits)},
	{Method: GET, Path: "/v1/merchant/trust-level", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Query: params(text("currency"), text("customer_id")), Responses: []Reply{{200, Untyped{}}}, Errors: codes("authentication_required", "currency_unsupported", "invalid_param"), Handler: h(handlers.ServiceGetTrustLevel)},
	{Method: PUT, Path: "/v1/merchant/credit-limit", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCreditsGrant,
		Request: handlers.ServiceCreditLimitRequest{}, Responses: []Reply{{200, Message{}}}, Errors: codes("authentication_required", "currency_unsupported", "invalid_param"), Handler: h(handlers.ServiceSetCreditLimit)},
	{Method: GET, Path: "/v1/merchant/credit-limit", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Query: params(text("currency"), text("customer_id")), Responses: []Reply{{200, billing.CreditLimitRequest{}}}, Errors: codes("authentication_required", "currency_unsupported", "invalid_param"), Handler: h(handlers.ServiceGetCreditLimit)},
	{Method: GET, Path: "/v1/merchant/credits/balance", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Query: params(text("currency"), text("customer_id")), Responses: []Reply{{200, handlers.ServiceBalanceResponse{}}}, Errors: codes("authentication_required", "currency_unsupported", "invalid_param"), Handler: h(handlers.ServiceGetCreditsBalance)},
	{Method: POST, Path: "/v1/merchant/credits/deposit", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCreditsGrant,
		Request: handlers.ServiceDepositRequest{}, Responses: []Reply{{200, service.CreditTransaction{}}}, Errors: codes("authentication_required", "currency_unsupported", "idempotency_key_reused", "invalid_param"), Handler: h(handlers.ServiceDepositCredits)},
	{Method: GET, Path: "/v1/merchant/credits/deposit", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Query: params(text("customer_id"), text("source_id")), Responses: []Reply{{200, service.CreditTransaction{}}}, Errors: codes("authentication_required", "invalid_param"), Handler: h(handlers.ServiceGetDeposit)},
	{Method: POST, Path: "/v1/merchant/customers/{customer_id}/credits", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCreditsGrant, Limit: middleware.AdminOperationGrant,
		Request: handlers.AdminGrantCreditsRequest{}, Responses: []Reply{{200, service.CreditTransaction{}}}, Errors: codes("currency_unsupported", "idempotency_key_reused", "invalid_param"), Handler: h(handlers.AdminGrantCredits)},
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/credits", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Query: params(text("currency"), integer("limit"), integer("offset")), Responses: []Reply{{200, Untyped{}}}, Errors: codes("currency_unsupported", "invalid_param"), Bind: gated(handlers.ListAdminCreditGrants)},
	{Method: DELETE, Path: "/v1/merchant/customers/{customer_id}/credits/{grant_id}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCreditsRevoke, Limit: middleware.AdminOperationDestructive,
		Responses: []Reply{{200, service.CreditGrantRevocation{}}}, Errors: codes("credit_grant_held", "credit_grant_not_found", "credit_grant_unavailable", "invalid_param"), Handler: h(handlers.RevokeAdminCreditGrant)},
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/credit-transactions", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Query: params(text("currency"), integer("limit"), integer("offset")), Responses: []Reply{{200, Untyped{}}}, Errors: codes("currency_unsupported", "invalid_param"), Handler: h(handlers.ListAdminCreditTransactions)},
	{Method: GET, Path: "/v1/me/spend-limits", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement, InvokerScoped: true,
		Query: params(text("currency")), Responses: []Reply{{200, handlers.SelfSpendLimitsDocument{}}}, Errors: codes("authentication_required", "currency_unsupported", "invalid_param"), Handler: h(handlers.GetMySpendLimits)},
	{Method: GET, Path: "/v1/me/balance", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Query: params(text("currency")), Responses: []Reply{{200, handlers.SelfBalanceResponse{}}}, Errors: codes("authentication_required", "currency_unsupported", "invalid_param"), Handler: h(handlers.GetMyBalance)},
	{Method: GET, Path: "/v1/me/transactions", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Query: params(text("currency"), integer("limit"), integer("offset")), Responses: []Reply{{200, Untyped{}}}, Errors: codes("authentication_required", "currency_unsupported", "invalid_param"), Handler: h(handlers.GetMyAccountTransactions)},
}
