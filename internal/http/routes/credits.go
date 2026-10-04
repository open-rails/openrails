package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
)

// customerErrors are the codes of a route addressed to one customer.
var customerErrors = []string{"invalid_customer_id", billing.CodeServiceCredentialCustomerScopeDenied}

func withCustomer(list ...string) []string { return codes(append(list, customerErrors...)...) }

// creditsRoutes is a customer's prepaid money: credit grants and the ledger,
// balances, the credit limit and trust level, and the delegations that let
// invokers spend a customer's balance.
var creditsRoutes = []Route{
	{Method: POST, Path: "/v1/merchant/customers/{customer_id}/credit-grants", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCreditsGrant, Limit: middleware.AdminOperationGrant,
		Request: billing.CreditGrantParams{}, Responses: []Reply{{201, billing.CreditGrant{}}, {200, billing.CreditGrant{}}},
		Errors: withCustomer("currency_unsupported", "idempotency_key_reused", "invalid_param"), Handler: h(handlers.CreateCreditGrant)},
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/credit-grants", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Query: params(queryOf(billing.CreditGrantListParams{}), text("cursor"), integer("limit")), Responses: []Reply{{200, billing.ListPage[billing.CreditGrant]{}}},
		Errors: withCustomer("invalid_cursor"), Handler: h(handlers.ListCreditGrants)},
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/credit-grants/{grant_id}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Responses: []Reply{{200, billing.CreditGrant{}}}, Errors: withCustomer("credit_grant_not_found", "invalid_param"), Handler: h(handlers.GetCreditGrant)},
	{Method: POST, Path: "/v1/merchant/customers/{customer_id}/credit-grants/{grant_id}/revoke", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCreditsRevoke, Limit: middleware.AdminOperationDestructive,
		Request: billing.RevokeCreditGrantParams{}, Responses: []Reply{{200, billing.CreditGrant{}}},
		Errors: withCustomer("credit_grant_held", "credit_grant_not_found", "credit_grant_unavailable", "invalid_param"), Handler: h(handlers.RevokeCreditGrant)},
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/transactions", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Query: params(queryOf(billing.CreditTransactionListParams{}), text("cursor"), integer("limit")), Responses: []Reply{{200, billing.ListPage[billing.CreditTransaction]{}}},
		Errors: withCustomer("currency_unsupported", "invalid_cursor"), Handler: h(handlers.ListCreditTransactions)},
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/balance", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Query: params(text("currency")), Responses: []Reply{{200, billing.Balance{}}}, Errors: withCustomer("currency_unsupported"), Handler: h(handlers.GetBalance)},
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/credit-limit", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Query: params(text("currency")), Responses: []Reply{{200, billing.CreditLimit{}}}, Errors: withCustomer("currency_unsupported"), Handler: h(handlers.GetCreditLimit)},
	{Method: PUT, Path: "/v1/merchant/customers/{customer_id}/credit-limit", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCreditsGrant,
		Request: billing.CreditLimitParams{}, Responses: []Reply{{200, billing.CreditLimit{}}}, Errors: withCustomer("currency_unsupported"), Handler: h(handlers.SetCreditLimit)},
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/trust-level", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Query: params(text("currency")), Responses: []Reply{{200, billing.TrustLevel{}}}, Errors: withCustomer("currency_unsupported"), Handler: h(handlers.GetTrustLevel)},
	{Method: PUT, Path: "/v1/merchant/customers/{customer_id}/trust-level", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsUpdate,
		Request: billing.TrustLevelParams{}, Responses: []Reply{{200, billing.TrustLevel{}}}, Errors: withCustomer("currency_unsupported"), Handler: h(handlers.SetTrustLevel)},
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/spend-delegations", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Responses: []Reply{{200, billing.ListPage[billing.SpendDelegation]{}}}, Errors: withCustomer(), Handler: h(handlers.ListSpendDelegations)},
	{Method: PUT, Path: "/v1/merchant/customers/{customer_id}/spend-delegations", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsUpdate,
		Request: billing.SpendDelegationsParams{}, Responses: []Reply{{200, billing.ListPage[billing.SpendDelegation]{}}}, Errors: withCustomer(), Handler: h(handlers.SetSpendDelegations)},
	{Method: PUT, Path: "/v1/merchant/customers/{customer_id}/spend-delegations/{scope}/{scope_key}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsUpdate,
		Request: billing.SpendDelegationParams{}, Responses: []Reply{{200, billing.SpendDelegation{}}}, Errors: withCustomer(), Handler: h(handlers.SetSpendDelegation)},
	{Method: DELETE, Path: "/v1/merchant/customers/{customer_id}/spend-delegations/{scope}/{scope_key}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsUpdate,
		Responses: []Reply{{204, nil}}, Errors: withCustomer("spend_delegation_not_found"), Handler: h(handlers.DeleteSpendDelegation)},
	{Method: GET, Path: "/v1/me/spend-limits", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement, InvokerScoped: true,
		Query: params(text("currency")), Responses: []Reply{{200, billing.SpendLimits{}}}, Errors: codes("currency_unsupported"), Handler: h(handlers.GetMySpendLimits)},
	{Method: GET, Path: "/v1/me/balance", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Query: params(text("currency")), Responses: []Reply{{200, billing.Balance{}}}, Errors: codes("currency_unsupported"), Handler: h(handlers.GetMyBalance)},
	{Method: GET, Path: "/v1/me/transactions", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Query: params(queryOf(billing.CreditTransactionListParams{}), text("cursor"), integer("limit")), Responses: []Reply{{200, billing.ListPage[billing.CreditTransaction]{}}},
		Errors: codes("currency_unsupported", "invalid_cursor"), Handler: h(handlers.GetMyCreditTransactions)},
}
