package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
)

// customerErrors are the codes of a route addressed to one customer.
var customerErrors = []billing.ErrorCode{billing.CodeInvalidCustomerID, billing.CodeServiceCredentialCustomerScopeDenied}

func withCustomer(list ...billing.ErrorCode) []billing.ErrorCode {
	return codes(append(list, customerErrors...)...)
}

// creditsRoutes is a customer's prepaid money: credit grants, the ledger and
// balances.
var creditsRoutes = []Route{
	{Method: POST, Path: "/v1/admin/credit-grants", Group: Admin, Auth: AuthMerchant, Name: "CreateCreditGrants", Level: LevelUpdate, Limit: middleware.AdminOperationGrant,
		Doc: "CreateCreditGrants grants 1 to billing.MaxBatchItems credits, across any customers, all or none; the " +
			"answer is in request order. Each item's SourceID makes it idempotent per customer: an identical " +
			"retry answers the existing grant with Replayed set, one with a different amount, currency or expiry " +
			"refuses the batch with billing.ErrIdempotencyKeyReused.",
		Request: billing.CreateCreditGrantBatchParams{}, Responses: []Reply{{201, billing.CreateCreditGrantBatchResult{}}, {200, billing.CreateCreditGrantBatchResult{}}},
		Errors: codes("currency_unsupported", "idempotency_key_reused", "invalid_param", billing.CodeServiceCredentialCustomerScopeDenied), Handler: h(handlers.CreateCreditGrants)},
	{Method: GET, Path: "/v1/admin/credit-grants", Group: Admin, Auth: AuthMerchant, Name: "ListCreditGrants", Level: LevelRead,
		Doc: "ListCreditGrants lists params.CustomerID's credit grants, newest first, or the grants params.IDs " +
			"names.",
		Query: billing.CreditGrantListParams{}, Responses: []Reply{{200, billing.ListPage[billing.CreditGrant]{}}},
		Errors: withCustomer("invalid_cursor"), Handler: h(handlers.ListCreditGrants)},
	{Method: GET, Path: "/v1/admin/credit-grants/{id}", Group: Admin, Auth: AuthMerchant, Name: "GetCreditGrant", Level: LevelRead,
		Args:      []any{billing.CreditGrantID{}},
		Doc:       "GetCreditGrant reads one credit grant.",
		Responses: []Reply{{200, billing.CreditGrant{}}}, Errors: codes("credit_grant_not_found", "invalid_param"), Handler: h(handlers.GetCreditGrant)},
	{Method: POST, Path: "/v1/admin/credit-grants/{id}/revoke", Group: Admin, Auth: AuthMerchant, Name: "RevokeCreditGrant", Level: LevelUpdate,
		Args: []any{billing.CreditGrantID{}},
		Doc: "RevokeCreditGrant revokes a grant's unspent remainder. Revoking a revoked grant returns it with " +
			"Replayed set.",
		Request: billing.RevokeCreditGrantParams{}, Responses: []Reply{{200, billing.CreditGrant{}}},
		Errors: codes("credit_grant_held", "credit_grant_not_found", "credit_grant_unavailable", "invalid_param"), Handler: h(handlers.RevokeCreditGrant)},
	{Method: GET, Path: "/v1/admin/customers/{customer_id}/balance/transactions", Group: Admin, Auth: AuthMerchant, Name: "ListBalanceTransactions", Level: LevelRead,
		Args: []any{billing.CustomerID{}},
		Doc: "ListBalanceTransactions lists a customer's balance transactions in one currency, newest first, or " +
			"the transactions params.IDs names.",
		Query: billing.BalanceTransactionListParams{}, Responses: []Reply{{200, billing.ListPage[billing.BalanceTransaction]{}}},
		Errors: withCustomer("currency_unsupported", "invalid_cursor"), Handler: h(handlers.ListBalanceTransactions)},
	{Method: GET, Path: "/v1/admin/customers/{customer_id}/balance", Group: Admin, Auth: AuthMerchant, Name: "GetBalance", Level: LevelRead,
		Args:  []any{billing.CustomerID{}},
		Doc:   "GetBalance reads a customer's balance, holds and owed amount in params.Currency.",
		Query: billing.GetBalanceParams{}, Responses: []Reply{{200, billing.Balance{}}}, Errors: withCustomer("currency_unsupported"), Handler: h(handlers.GetBalance)},
	{Method: GET, Path: "/v1/me", Group: Customer, Auth: AuthCustomer,
		Responses: []Reply{{200, billing.CustomerAccount{}}}, Handler: h(handlers.GetMe)},
	{Method: PUT, Path: "/v1/me/low-balance-lines/{currency}", Group: Customer, Auth: AuthCustomer,
		Request: billing.SetLowBalanceLineParams{}, Responses: []Reply{{200, billing.EffectiveLowBalanceLine{}}},
		Errors: codes("currency_unsupported", "invalid_param"), Handler: h(handlers.SetMyLowBalanceLine)},
	{Method: GET, Path: "/v1/me/balance/transactions", Group: Customer, Auth: AuthCustomer,
		Query: handlers.MyBalanceTransactionListQuery{}, Responses: []Reply{{200, billing.ListPage[billing.BalanceTransaction]{}}},
		Errors: codes("currency_unsupported", "invalid_cursor"), Handler: h(handlers.ListMyBalanceTransactions)},
}
