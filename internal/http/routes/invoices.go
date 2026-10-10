package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
)

var (
	invoiceErrors       = codes("invalid_param", "resource_not_found")
	invoiceActionErrors = codes("invalid_param", "invoice_action_not_allowed", "resource_not_found")
	invoiceChargeErrors = codes("card_declined", "default_payment_method_invalid", "default_payment_method_required", "idempotency_key_reused", "invalid_param",
		"invoice_not_retryable", "invoice_retry_idempotency_conflict", "invoice_retry_in_progress", "invoice_retry_outcome_unknown", "payment_method_required",
		"payment_provider_rejected", "resource_not_found", "service_unavailable")
)

// invoicesRoutes is invoices and their collection, for the merchant and for
// the customer who owes them.
var invoicesRoutes = []Route{
	{Method: GET, Path: "/v1/admin/invoices", Group: Admin, Auth: AuthMerchant, Name: "ListInvoices", Level: LevelRead,
		Query: params(cursorPage, idsParam, text("currency"), text("customer_id"), Param{Name: "overdue", Kind: "boolean"}, text("period_starts_after"), text("period_starts_before"), text("status")), Responses: []Reply{{200, billing.ListPage[billing.Invoice]{}}}, Errors: codes("invalid_cursor"), Bind: gated(handlers.ListInvoices)},
	{Method: GET, Path: "/v1/admin/invoices/{id}", Group: Admin, Auth: AuthMerchant, Name: "GetInvoice", Level: LevelRead,
		Responses: []Reply{{200, billing.Invoice{}}}, Errors: invoiceErrors, Bind: gated(handlers.GetInvoice)},
	{Method: POST, Path: "/v1/admin/invoices/{id}/void", Group: Admin, Auth: AuthMerchant, Name: "VoidInvoice", Level: LevelWrite, Sensitive: true, Limit: middleware.AdminOperationDestructive,
		Responses: []Reply{{200, billing.Invoice{}}}, Errors: invoiceActionErrors, Handler: h(handlers.VoidInvoice)},
	{Method: POST, Path: "/v1/admin/invoices/{id}/uncollectible", Group: Admin, Auth: AuthMerchant, Name: "MarkInvoiceUncollectible", Level: LevelWrite, Sensitive: true, Limit: middleware.AdminOperationDestructive,
		Responses: []Reply{{200, billing.Invoice{}}}, Errors: invoiceActionErrors, Handler: h(handlers.MarkInvoiceUncollectible)},
	{Method: POST, Path: "/v1/admin/invoices/{id}/retry-collection", Group: Admin, Auth: AuthMerchant, Name: "RetryInvoiceCollection", Level: LevelWrite, Sensitive: true, Limit: middleware.AdminOperationOffChannel, IdempotencyKey: true,
		Request: billing.RetryInvoiceCollectionParams{}, Responses: []Reply{{200, billing.InvoiceCollection{}}, {202, billing.InvoiceCollection{}}}, Errors: invoiceChargeErrors, Handler: h(handlers.RetryInvoiceCollection)},
	{Method: GET, Path: "/v1/me/invoices", Group: Customer, Auth: AuthCustomer,
		Query: cursorPage, Responses: []Reply{{200, billing.ListPage[billing.Invoice]{}}}, Errors: codes("invalid_cursor"), Handler: h(handlers.ListMyInvoices)},
	{Method: GET, Path: "/v1/me/invoices/{id}", Group: Customer, Auth: AuthCustomer,
		Responses: []Reply{{200, billing.Invoice{}}}, Errors: invoiceErrors, Handler: h(handlers.GetMyInvoice)},
	{Method: POST, Path: "/v1/me/invoices/{id}/pay-now", Group: Customer, Auth: AuthCustomer, IdempotencyKey: true,
		Request: billing.PayInvoiceParams{}, Responses: []Reply{{200, billing.InvoicePayNow{}}, {202, billing.InvoicePayNow{}}}, Errors: codes("card_declined", "customer_action_required", "customer_payment_unsupported", "invalid_param", "invalid_payment_method", "payment_idempotency_conflict", "payment_in_progress", "payment_method_required", "payment_not_retryable", "payment_provider_rejected", "rebill_terms_committed", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.PayMyInvoice)},
}
