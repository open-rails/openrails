package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
)

// invoicesRoutes are purchases (#1168). The customer previews, buys in one
// call (create with a payment), or creates it with its embedded pay URL on
// the checkout origin (create with a pay URL), pays, confirms after
// next_action, cancels and reads; paying needs the customer in person and an Idempotency-Key, and
// a failed payment answers 402 card_error with the invoice. The merchant's
// backend creates unpaid invoices for a customer to pay (/v1/app/invoices);
// staff inspect them, record external payments, waive usage debt or mark it
// uncollectible. Staff never initiate saved-card collection.
var (
	invoiceReadErrors   = codes("resource_not_found", "service_unavailable")
	invoiceLineErrors   = codes("already_owned", "invalid_param", "invoice_line_unavailable", "quantity_not_allowed", "resource_not_found", "service_unavailable")
	invoicePayErrors    = codes("billing_details_required", "card_attempts_blocked", "card_declined", "customer_action_required", "customer_email_required", "entitlement_required", "idempotency_key_in_use", "idempotency_key_required", "idempotency_key_reused", "invalid_param", "invoice_not_payable", "invoice_payment_in_progress", "invoice_total_changed", "payment_failed", "payment_method_stale", "payment_option_unavailable", "payment_provider_rejected", "resource_conflict", "resource_not_found", "service_unavailable")
	invoiceCreateErrors = codes(append(append([]billing.ErrorCode{}, invoicePayErrors...), "already_owned", "embed_origins_required", "customer_session_required", "insufficient_credits", "pay_url_unavailable", "invoice_line_unavailable", "quantity_not_allowed")...)
	usageActionErrors   = codes("invoice_action_not_allowed", "resource_not_found", "service_unavailable")
)

var invoicesRoutes = []Route{
	{Method: POST, Path: "/v1/me/invoices/preview", Group: Customer, Auth: AuthCustomer,
		Request: billing.PreviewMyInvoiceParams{}, Responses: []Reply{{200, billing.InvoicePreview{}}}, Errors: invoiceLineErrors, Handler: h(handlers.PreviewMyInvoice)},
	{Method: POST, Path: "/v1/me/invoices", Group: Customer, Auth: AuthCustomer, IdempotencyKey: true,
		Request: billing.CreateMyInvoiceParams{}, Responses: []Reply{{200, billing.Invoice{}}, {201, billing.Invoice{}}}, Errors: invoiceCreateErrors, Handler: h(handlers.CreateMyInvoice)},
	{Method: GET, Path: "/v1/me/invoices", Group: Customer, Auth: AuthCustomer,
		Query: handlers.MyInvoiceListQuery{}, Responses: []Reply{{200, billing.ListPage[billing.Invoice]{}}}, Errors: codes("invalid_cursor", "invalid_query", "service_unavailable"), Handler: h(handlers.ListMyInvoices)},
	{Method: GET, Path: "/v1/me/invoices/{id}", Group: Customer, Auth: AuthCustomer, PayURL: PayURLInvoice,
		Responses: []Reply{{200, billing.Invoice{}}}, Errors: invoiceReadErrors, Handler: h(handlers.GetMyInvoice)},
	{Method: GET, Path: "/v1/me/invoices/{id}/payment-attempt", Group: Customer, Auth: AuthCustomer, IdempotencyKey: true,
		Responses: []Reply{{200, billing.InvoicePaymentAttempt{}}}, Errors: codes("idempotency_key_required", "invalid_param", "resource_not_found", "service_unavailable"), Handler: h(handlers.GetMyInvoicePaymentAttempt)},
	{Method: POST, Path: "/v1/me/invoices/{id}/pay", Group: Customer, Auth: AuthCustomer, PayURL: PayURLInvoice, IdempotencyKey: true, SubmitsCheckout: true,
		Request: billing.PayInvoiceParams{}, Responses: []Reply{{200, billing.Invoice{}}}, Errors: invoicePayErrors, Handler: h(handlers.PayMyInvoice)},
	{Method: POST, Path: "/v1/me/invoices/{id}/confirm", Group: Customer, Auth: AuthCustomer, PayURL: PayURLInvoice,
		Request: billing.ConfirmInvoiceParams{}, Responses: []Reply{{200, billing.Invoice{}}}, Errors: codes("card_declined", "customer_action_required", "customer_session_required", "invalid_param", "payment_failed", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.ConfirmMyInvoice)},
	// The customer's page could not frame the invoice's pay URL: the operator is
	// alerted, once per episode.
	{Method: POST, Path: "/v1/me/invoices/{id}/pay-url-failure", Group: Customer, Auth: AuthCustomer,
		Responses: []Reply{{204, nil}}, Errors: invoiceReadErrors, Handler: h(handlers.ReportMyPayURLFailure)},
	{Method: POST, Path: "/v1/me/invoices/{id}/cancel", Group: Customer, Auth: AuthCustomer,
		Responses: []Reply{{200, billing.Invoice{}}}, Errors: codes("invoice_not_cancelable", "resource_not_found", "service_unavailable"), Handler: h(handlers.CancelMyInvoice)},
	// A wallet's Solana Pay transaction request for an invoice awaiting it: the
	// label, then the transaction for the wallet's account. No credential:
	// a Solana Pay link is opened by the wallet, not the customer's session,
	// and the invoice names its merchant.
	{Method: GET, Path: "/v1/invoices/{id}/solana-pay", Group: Payment, Auth: AuthPublic, Merchant: MerchantInvoice, When: FeatureSolana,
		Responses: []Reply{{200, handlers.SolanaPayGetResponse{}}}, Errors: codes("invoice_not_payable", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.GetInvoiceSolanaPay)},
	{Method: POST, Path: "/v1/invoices/{id}/solana-pay", Group: Payment, Auth: AuthPublic, Merchant: MerchantInvoice, When: FeatureSolana,
		Request: handlers.SolanaPayPostRequest{}, Responses: []Reply{{200, handlers.SolanaPayPostResponse{}}}, Errors: codes("invalid_param", "invoice_not_payable", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.PostInvoiceSolanaPay)},
	{Method: POST, Path: "/v1/app/invoices/preview", Group: App, Auth: AuthApplication, Permission: needInvoices, Name: "PreviewInvoice", Level: LevelRead,
		Doc: "PreviewInvoice prices lines for one customer, as a merchant invoice would freeze them: per-line " +
			"refusals, the total and what can pay it.",
		Request: billing.PreviewInvoiceParams{}, Responses: []Reply{{200, billing.InvoicePreview{}}}, Errors: invoiceLineErrors, Handler: h(handlers.PreviewMerchantInvoice)},
	{Method: GET, Path: "/v1/app/invoices/{id}/payment-attempt", Group: App, Auth: AuthApplication, Permission: needInvoices, Name: "GetInvoicePaymentAttempt", IdempotencyKey: true,
		Args: []any{billing.InvoiceID{}},
		Doc: "GetInvoicePaymentAttempt reads one retained purchase payment attempt by idempotencyKey, its original " +
			"POST /me/invoices/{id}/pay Idempotency-Key. It does not look up create-invoice-with-payment or " +
			"renewal/usage keys, retry or perform a provider write.",
		Responses: []Reply{{200, billing.InvoicePaymentAttempt{}}}, Errors: codes("idempotency_key_required", "invalid_param", "resource_not_found", "service_unavailable"), Handler: h(handlers.GetInvoicePaymentAttempt)},
	{Method: POST, Path: "/v1/app/invoices", Group: App, Auth: AuthApplication, Permission: needInvoices, Name: "CreateInvoice", IdempotencyKey: true,
		Doc: "CreateInvoice creates a merchant invoice: an unpaid invoice for one customer, which they pay on your site " +
			"(POST /v1/me/invoices/{id}/pay, or billing-ui's InvoicePayURL). It never charges a card. An invoice in one of " +
			"the merchant's own units is paid from the customer's balance in it as it is created (insufficient_credits " +
			"when the balance does not cover it). idempotencyKey is required: a retry with it answers the same invoice.",
		Request: billing.CreateInvoiceParams{}, Responses: []Reply{{200, billing.Invoice{}}, {201, billing.Invoice{}}},
		Errors: codes("already_owned", "embed_origins_required", "entitlement_required", "idempotency_key_in_use", "idempotency_key_required", "idempotency_key_reused", "insufficient_credits", "invalid_param", "invoice_line_unavailable", "quantity_not_allowed", "resource_not_found", "service_unavailable"), Handler: h(handlers.CreateMerchantInvoice)},
	{Method: POST, Path: "/v1/app/invoices/{id}/cancel", Group: App, Auth: AuthApplication, Permission: needInvoices, Name: "CancelInvoice",
		Args:      []any{billing.InvoiceID{}},
		Doc:       "CancelInvoice withdraws an open invoice you created.",
		Responses: []Reply{{200, billing.Invoice{}}}, Errors: codes("invoice_not_cancelable", "resource_not_found", "service_unavailable"), Handler: h(handlers.CancelMerchantInvoice)},
	{Method: GET, Path: "/v1/admin/invoices", Group: Admin, Auth: AuthMerchant, Name: "ListInvoices", Level: LevelRead,
		Doc: "ListInvoices is one page of the merchant's invoices, newest first. Status complete with PriceID answers " +
			"whether a customer bought a price.",
		Query: billing.InvoiceListParams{}, Responses: []Reply{{200, billing.ListPage[billing.Invoice]{}}}, Errors: codes("invalid_cursor", "invalid_query", "service_unavailable"), Handler: h(handlers.ListInvoices)},
	{Method: GET, Path: "/v1/admin/invoices/{id}", Group: Admin, Auth: AuthMerchant, Name: "GetInvoice", Level: LevelRead,
		Args:      []any{billing.InvoiceID{}},
		Doc:       "GetInvoice reads one invoice. Staff never pay, so it carries no next_action.",
		Responses: []Reply{{200, billing.Invoice{}}}, Errors: invoiceReadErrors, Handler: h(handlers.GetInvoice)},
	{Method: POST, Path: "/v1/admin/invoices/{id}/waive-balance", Group: Admin, Auth: AuthMerchant, Name: "WaiveInvoiceBalance", Level: LevelUpdate,
		Args: []any{billing.InvoiceID{}},
		Doc: "WaiveInvoiceBalance forgives the unpaid balance of a usage invoice. Existing payments remain recorded. " +
			"Repeating it changes nothing.",
		Responses: []Reply{{200, billing.Invoice{}}}, Errors: usageActionErrors, Handler: h(handlers.WaiveInvoiceBalance)},
	{Method: POST, Path: "/v1/admin/invoices/{id}/mark-uncollectible", Group: Admin, Auth: AuthMerchant, Name: "MarkInvoiceUncollectible", Level: LevelUpdate,
		Args: []any{billing.InvoiceID{}},
		Doc: "MarkInvoiceUncollectible stops scheduled collection while retaining the debt. A customer payment can " +
			"reopen it. Repeating it changes nothing.",
		Responses: []Reply{{200, billing.Invoice{}}}, Errors: usageActionErrors, Handler: h(handlers.MarkInvoiceUncollectible)},
}
