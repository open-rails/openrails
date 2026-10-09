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
	{Method: POST, Path: "/v1/admin/credit-grants", Group: Admin, Auth: AuthMerchant, Name: "CreateCreditGrants", Level: LevelWrite, Sensitive: true, Limit: middleware.AdminOperationGrant,
		Request: billing.CreateCreditGrantBatchParams{}, Responses: []Reply{{201, billing.CreateCreditGrantBatchResult{}}, {200, billing.CreateCreditGrantBatchResult{}}},
		Errors: codes("currency_unsupported", "idempotency_key_reused", "invalid_param", billing.CodeServiceCredentialCustomerScopeDenied), Handler: h(handlers.CreateCreditGrants)},
	{Method: GET, Path: "/v1/admin/customers/{customer_id}/credit-grants", Group: Admin, Auth: AuthMerchant, Name: "ListCreditGrants", Level: LevelRead,
		Query: params(queryOf(billing.CreditGrantListParams{}), idsParam, text("cursor"), integer("limit")), Responses: []Reply{{200, billing.ListPage[billing.CreditGrant]{}}},
		Errors: withCustomer("invalid_cursor"), Handler: h(handlers.ListCreditGrants)},
	{Method: GET, Path: "/v1/admin/customers/{customer_id}/credit-grants/{id}", Group: Admin, Auth: AuthMerchant, Name: "GetCreditGrant", Level: LevelRead,
		Responses: []Reply{{200, billing.CreditGrant{}}}, Errors: withCustomer("credit_grant_not_found", "invalid_param"), Handler: h(handlers.GetCreditGrant)},
	{Method: POST, Path: "/v1/admin/customers/{customer_id}/credit-grants/{id}/revoke", Group: Admin, Auth: AuthMerchant, Name: "RevokeCreditGrant", Level: LevelWrite, Sensitive: true, Limit: middleware.AdminOperationDestructive,
		Request: billing.RevokeCreditGrantParams{}, Responses: []Reply{{200, billing.CreditGrant{}}},
		Errors: withCustomer("credit_grant_held", "credit_grant_not_found", "credit_grant_unavailable", "invalid_param"), Handler: h(handlers.RevokeCreditGrant)},
	{Method: GET, Path: "/v1/admin/customers/{customer_id}/balance/transactions", Group: Admin, Auth: AuthMerchant, Name: "ListBalanceTransactions", Level: LevelRead,
		Query: params(queryOf(billing.BalanceTransactionListParams{}), idsParam, text("cursor"), integer("limit")), Responses: []Reply{{200, billing.ListPage[billing.BalanceTransaction]{}}},
		Errors: withCustomer("currency_unsupported", "invalid_cursor"), Handler: h(handlers.ListBalanceTransactions)},
	{Method: GET, Path: "/v1/admin/customers/{customer_id}/balance", Group: Admin, Auth: AuthMerchant, Name: "GetBalance", Level: LevelRead,
		Query: params(text("currency")), Responses: []Reply{{200, billing.Balance{}}}, Errors: withCustomer("currency_unsupported"), Handler: h(handlers.GetBalance)},
	{Method: GET, Path: "/v1/admin/customers/{customer_id}/spend-delegations", Group: Admin, Auth: AuthMerchant, Name: "ListSpendDelegations", Level: LevelRead,
		Responses: []Reply{{200, billing.ListPage[billing.SpendDelegation]{}}}, Errors: withCustomer(), Handler: h(handlers.ListSpendDelegations)},
	{Method: PUT, Path: "/v1/admin/customers/{customer_id}/spend-delegations", Group: Admin, Auth: AuthMerchant, Name: "SetSpendDelegations", Level: LevelWrite, Sensitive: true,
		Request: billing.SetSpendDelegationsParams{}, Responses: []Reply{{200, billing.ListPage[billing.SpendDelegation]{}}}, Errors: withCustomer(), Handler: h(handlers.SetSpendDelegations)},
	{Method: DELETE, Path: "/v1/admin/customers/{customer_id}/spend-delegations/{scope}/{scope_key}", Group: Admin, Auth: AuthMerchant, Name: "DeleteSpendDelegation", Level: LevelWrite, Sensitive: true,
		Responses: []Reply{{204, nil}}, Errors: withCustomer("spend_delegation_not_found"), Handler: h(handlers.DeleteSpendDelegation)},
	{Method: GET, Path: "/v1/me/spend-limits", Group: Customer, Auth: AuthCustomer, InvokerScoped: true,
		Query: params(text("currency")), Responses: []Reply{{200, billing.SpendLimits{}}}, Errors: codes("currency_unsupported"), Handler: h(handlers.GetMySpendLimits)},
	{Method: GET, Path: "/v1/me", Group: Customer, Auth: AuthCustomer,
		Responses: []Reply{{200, billing.CustomerAccount{}}}, Handler: h(handlers.GetMe)},
	{Method: GET, Path: "/v1/me/balance/transactions", Group: Customer, Auth: AuthCustomer,
		Query: params(queryOf(billing.BalanceTransactionListParams{}), text("cursor"), integer("limit")), Responses: []Reply{{200, billing.ListPage[billing.BalanceTransaction]{}}},
		Errors: codes("currency_unsupported", "invalid_cursor"), Handler: h(handlers.ListMyBalanceTransactions)},
}
