package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/service"
)

// invoicesRoutes is invoices and their collection, for the merchant and for
// the customer who owes them.
var invoicesRoutes = []Route{
	{Method: GET, Path: "/v1/merchant/invoices", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantInvoicesRead,
		Query: params(text("currency"), text("customer_id"), text("status")), Responses: []Reply{{200, handlers.PaginatedResponse[service.MerchantInvoiceDTO]{}}}, Errors: codes("invalid_param", "resource_not_found"), Bind: gated(handlers.ListAdminInvoices)},
	{Method: GET, Path: "/v1/merchant/invoices/{id}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantInvoicesRead,
		Responses: []Reply{{200, Untyped{}}}, Errors: codes("invalid_param", "resource_not_found"), Bind: gated(handlers.GetAdminInvoice)},
	{Method: GET, Path: "/v1/merchant/invoices/{id}/payments", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantInvoicesRead,
		Responses: []Reply{{200, Untyped{}}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.ListAdminInvoicePayments)},
	{Method: POST, Path: "/v1/merchant/invoices/{id}/void", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantInvoicesUpdate, Limit: middleware.AdminOperationDestructive,
		Request: billing.RecordInvoicePaymentRequest{}, Responses: []Reply{{200, service.MerchantInvoiceDTO{}}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.MutateAdminInvoice("void"))},
	{Method: POST, Path: "/v1/merchant/invoices/{id}/uncollectible", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantInvoicesUpdate, Limit: middleware.AdminOperationDestructive,
		Request: billing.RecordInvoicePaymentRequest{}, Responses: []Reply{{200, service.MerchantInvoiceDTO{}}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.MutateAdminInvoice("mark_uncollectible"))},
	{Method: POST, Path: "/v1/merchant/invoices/{id}/payments", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantInvoicesUpdate, Limit: middleware.AdminOperationOffChannel,
		Request: billing.RecordInvoicePaymentRequest{}, Responses: []Reply{{200, service.MerchantInvoiceDTO{}}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.MutateAdminInvoice("record_payment"))},
	{Method: POST, Path: "/v1/merchant/invoices/{id}/retry-collection", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantInvoicesCollect, Limit: middleware.AdminOperationOffChannel,
		Request: Untyped{}, Responses: []Reply{{200, service.InvoiceCollectionRetryResult{}}, {202, service.InvoiceCollectionRetryResult{}}}, Errors: codes("invalid_param", "payment_method_required", "rebill_terms_committed", "resource_conflict", "resource_not_found"), Handler: h(handlers.RetryAdminInvoiceCollection)},
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/invoice-profile", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Responses: []Reply{{200, Untyped{}}}, Errors: codes("invalid_param", "resource_not_found", "service_unavailable"), Bind: gated(handlers.GetAdminInvoiceProfile)},
	{Method: PUT, Path: "/v1/merchant/customers/{customer_id}/invoice-profile", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsUpdate, Limit: middleware.AdminOperationGrant,
		Request: service.InvoiceProfileDTO{}, Responses: []Reply{{200, service.InvoiceProfileDTO{}}, {201, service.InvoiceProfileDTO{}}}, Errors: codes("invalid_param", "resource_not_found", "service_unavailable"), Handler: h(handlers.PutAdminInvoiceProfile)},
	{Method: GET, Path: "/v1/me/invoices", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Query: params(integer("limit"), integer("offset")), Responses: []Reply{{200, Untyped{}}}, Errors: codes("authentication_required", "invalid_param"), Handler: h(handlers.GetMyInvoices)},
	{Method: GET, Path: "/v1/me/invoices/{id}", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Responses: []Reply{{200, service.InvoiceDTO{}}}, Errors: codes("authentication_required", "invalid_param", "resource_not_found"), Handler: h(handlers.GetMyInvoice)},
	{Method: POST, Path: "/v1/me/invoices/{id}/pay-now", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Request: billing.PayInvoiceNowRequest{}, Responses: []Reply{{200, billing.InvoicePayNowResult{}}, {202, billing.InvoicePayNowResult{}}}, Errors: codes("authentication_required", "card_declined", "customer_action_required", "customer_payment_unsupported", "invalid_param", "invalid_payment_method", "payment_idempotency_conflict", "payment_in_progress", "payment_method_required", "payment_not_retryable", "payment_provider_rejected", "rebill_terms_committed", "resource_conflict", "resource_not_found", "subscription_not_found"), Handler: h(handlers.PayMyInvoiceNow)},
	{Method: GET, Path: "/v1/customers/{customer_id}/invoices", Group: Treasury, Auth: AuthCustomerGrant, Perm: billing.CustomerBalanceRead,
		Query: params(integer("limit"), integer("offset")), Responses: []Reply{{200, Untyped{}}}, Errors: codes("authentication_required", "invalid_param"), Handler: h(handlers.GetMyInvoices)},
	{Method: GET, Path: "/v1/customers/{customer_id}/invoices/{id}", Group: Treasury, Auth: AuthCustomerGrant, Perm: billing.CustomerBalanceRead,
		Responses: []Reply{{200, service.InvoiceDTO{}}}, Errors: codes("authentication_required", "invalid_param", "resource_not_found"), Handler: h(handlers.GetMyInvoice)},
}
