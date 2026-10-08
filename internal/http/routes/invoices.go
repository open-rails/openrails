package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
)

var (
	invoiceErrors       = codes("invalid_param", "resource_not_found")
	invoiceActionErrors = codes("invalid_param", "invoice_action_not_allowed", "resource_not_found")
	invoiceChargeErrors = codes("card_declined", "collection_payment_method_invalid", "collection_payment_method_required", "idempotency_key_reused", "invalid_param",
		"invoice_not_retryable", "invoice_retry_idempotency_conflict", "invoice_retry_in_progress", "invoice_retry_outcome_unknown", "payment_method_required",
		"payment_provider_rejected", "resource_not_found", "service_unavailable")
)

// invoicesRoutes is invoices and their collection, for the merchant and for
// the customer who owes them.
var invoicesRoutes = []Route{
	{Method: GET, Path: "/v1/merchant/invoices", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantInvoicesRead,
		Query: params(cursorPage, text("currency"), text("customer_id"), text("period_starts_after"), text("period_starts_before"), text("status")), Responses: []Reply{{200, billing.ListPage[billing.Invoice]{}}}, Errors: codes("invalid_cursor"), Bind: gated(handlers.ListInvoices)},
	{Method: GET, Path: "/v1/merchant/invoices/{id}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantInvoicesRead,
		Responses: []Reply{{200, billing.Invoice{}}}, Errors: invoiceErrors, Bind: gated(handlers.GetInvoice)},
	{Method: GET, Path: "/v1/merchant/invoices/{id}/payments", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantInvoicesRead,
		Query: cursorPage, Responses: []Reply{{200, billing.ListPage[billing.InvoicePayment]{}}}, Errors: codes("invalid_cursor", "invalid_param", "resource_not_found"), Handler: h(handlers.ListInvoicePayments)},
	{Method: POST, Path: "/v1/merchant/invoices/{id}/payments", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantInvoicesUpdate, Limit: middleware.AdminOperationOffChannel,
		Request: billing.CreateInvoicePaymentParams{}, Responses: []Reply{{200, billing.Invoice{}}}, Errors: codes("invalid_param", "invoice_action_not_allowed", "invoice_payment_exceeds_due", "invoice_payment_invalid", "invoice_payment_reference_used", "resource_not_found"), Handler: h(handlers.CreateInvoicePayment)},
	{Method: POST, Path: "/v1/merchant/invoices/{id}/void", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantInvoicesUpdate, Limit: middleware.AdminOperationDestructive,
		Responses: []Reply{{200, billing.Invoice{}}}, Errors: invoiceActionErrors, Handler: h(handlers.VoidInvoice)},
	{Method: POST, Path: "/v1/merchant/invoices/{id}/uncollectible", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantInvoicesUpdate, Limit: middleware.AdminOperationDestructive,
		Responses: []Reply{{200, billing.Invoice{}}}, Errors: invoiceActionErrors, Handler: h(handlers.MarkInvoiceUncollectible)},
	{Method: POST, Path: "/v1/merchant/invoices/{id}/retry-collection", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantInvoicesCollect, Limit: middleware.AdminOperationOffChannel, IdempotencyKey: true,
		Request: billing.RetryInvoiceCollectionParams{}, Responses: []Reply{{200, billing.InvoiceCollection{}}, {202, billing.InvoiceCollection{}}}, Errors: invoiceChargeErrors, Handler: h(handlers.RetryInvoiceCollection)},
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/invoice-profile", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Responses: []Reply{{200, billing.InvoiceProfile{}}}, Errors: codes("invalid_param", "resource_not_found", "service_unavailable"), Handler: h(handlers.GetInvoiceProfile)},
	{Method: PUT, Path: "/v1/merchant/customers/{customer_id}/invoice-profile", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsUpdate, Limit: middleware.AdminOperationGrant,
		Request: billing.InvoiceProfile{}, Responses: []Reply{{200, billing.InvoiceProfile{}}, {201, billing.InvoiceProfile{}}}, Errors: codes("invalid_param", "resource_not_found", "service_unavailable"), Handler: h(handlers.SetInvoiceProfile)},
	{Method: GET, Path: "/v1/me/invoices", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Query: cursorPage, Responses: []Reply{{200, billing.ListPage[billing.Invoice]{}}}, Errors: codes("invalid_cursor"), Handler: h(handlers.ListMyInvoices)},
	{Method: GET, Path: "/v1/me/invoices/{id}", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Responses: []Reply{{200, billing.Invoice{}}}, Errors: invoiceErrors, Handler: h(handlers.GetMyInvoice)},
	{Method: POST, Path: "/v1/me/invoices/{id}/pay-now", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement, IdempotencyKey: true,
		Request: billing.PayInvoiceParams{}, Responses: []Reply{{200, billing.InvoicePayNow{}}, {202, billing.InvoicePayNow{}}}, Errors: codes("card_declined", "customer_action_required", "customer_payment_unsupported", "invalid_param", "invalid_payment_method", "payment_idempotency_conflict", "payment_in_progress", "payment_method_required", "payment_not_retryable", "payment_provider_rejected", "rebill_terms_committed", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.PayMyInvoice)},
}
