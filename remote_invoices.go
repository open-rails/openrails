package openrails

import (
	"context"
	"net/http"
	"time"

	"github.com/open-rails/openrails/billing"
)

// ListInvoices is one page of the merchant's invoices, newest period first.
func (c *Client) ListInvoices(ctx context.Context, params billing.InvoiceListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.Invoice], error) {
	q := pageValues(nil, params.PageRequest)
	setQuery(q, map[string]string{"customer_id": params.CustomerID.String(), "currency": normalizeCurrency(params.Currency), "status": string(params.Status)})
	if params.PeriodFrom != nil {
		q.Set("period_from", params.PeriodFrom.UTC().Format(time.RFC3339Nano))
	}
	if params.PeriodTo != nil {
		q.Set("period_to", params.PeriodTo.UTC().Format(time.RFC3339Nano))
	}
	var out billing.ListPage[billing.Invoice]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/invoices?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func invoicePath(id billing.InvoiceID) (string, error) {
	invoice, err := requireTypedID("invoice_id", id)
	if err != nil {
		return "", err
	}
	return "/v1/merchant/invoices/" + invoice, nil
}

// invoiceCall posts or reads one invoice route answering T.
func invoiceCall[T any](ctx context.Context, c *Client, method string, id billing.InvoiceID, suffix string, body any, headers http.Header, requestOptions []RequestOption) (*T, error) {
	path, err := invoicePath(id)
	if err != nil {
		return nil, err
	}
	var out T
	if err := c.doWithHeaders(ctx, method, path+suffix, body, &out, headers, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetInvoice reads one invoice.
func (c *Client) GetInvoice(ctx context.Context, id billing.InvoiceID, requestOptions ...RequestOption) (*billing.Invoice, error) {
	return invoiceCall[billing.Invoice](ctx, c, http.MethodGet, id, "", nil, nil, requestOptions)
}

// VoidInvoice voids a draft, open or past-due invoice. Repeating it changes
// nothing.
func (c *Client) VoidInvoice(ctx context.Context, id billing.InvoiceID, requestOptions ...RequestOption) (*billing.Invoice, error) {
	return invoiceCall[billing.Invoice](ctx, c, http.MethodPost, id, "/void", nil, nil, requestOptions)
}

// MarkInvoiceUncollectible marks an open or past-due invoice uncollectible.
// Repeating it changes nothing.
func (c *Client) MarkInvoiceUncollectible(ctx context.Context, id billing.InvoiceID, requestOptions ...RequestOption) (*billing.Invoice, error) {
	return invoiceCall[billing.Invoice](ctx, c, http.MethodPost, id, "/uncollectible", nil, nil, requestOptions)
}

// CreateInvoicePayment records money received outside collection and
// answers the invoice. Reference identifies the remittance, so a retry
// records it once.
func (c *Client) CreateInvoicePayment(ctx context.Context, id billing.InvoiceID, params billing.CreateInvoicePaymentParams, requestOptions ...RequestOption) (*billing.Invoice, error) {
	return invoiceCall[billing.Invoice](ctx, c, http.MethodPost, id, "/payments", params, nil, requestOptions)
}

// ListInvoicePayments is one page of an invoice's payments, newest first.
func (c *Client) ListInvoicePayments(ctx context.Context, id billing.InvoiceID, page billing.PageRequest, requestOptions ...RequestOption) (*billing.ListPage[billing.InvoicePayment], error) {
	return invoiceCall[billing.ListPage[billing.InvoicePayment]](ctx, c, http.MethodGet, id, "/payments?"+pageValues(nil, page).Encode(), nil, nil, requestOptions)
}

// RetryInvoiceCollection charges an open invoice to one of its customer's
// cards now. The same IdempotencyKey answers the first attempt.
func (c *Client) RetryInvoiceCollection(ctx context.Context, id billing.InvoiceID, params billing.RetryInvoiceCollectionParams, requestOptions ...RequestOption) (*billing.InvoiceCollection, error) {
	if params.IdempotencyKey == "" {
		return nil, invalidErr("Idempotency-Key required")
	}
	return invoiceCall[billing.InvoiceCollection](ctx, c, http.MethodPost, id, "/retry-collection", params, http.Header{"Idempotency-Key": {params.IdempotencyKey}}, requestOptions)
}

// GetInvoiceProfile reads how a customer is invoiced: payment terms,
// collection method, PO number, tax details and billing contacts. A customer
// with none is billing.ErrNotFound.
func (c *Client) GetInvoiceProfile(ctx context.Context, customerID billing.CustomerID, requestOptions ...RequestOption) (*billing.InvoiceProfile, error) {
	path, err := customerIDPath(customerID)
	if err != nil {
		return nil, err
	}
	var out billing.InvoiceProfile
	if err := c.do(ctx, http.MethodGet, path+"/invoice-profile", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetInvoiceProfile replaces how a customer is invoiced, or with
// IfAbsent sets it only when none is set, answering an existing profile
// unchanged.
func (c *Client) SetInvoiceProfile(ctx context.Context, customerID billing.CustomerID, params billing.SetInvoiceProfileParams, requestOptions ...RequestOption) (*billing.InvoiceProfile, error) {
	path, err := customerIDPath(customerID)
	if err != nil {
		return nil, err
	}
	var headers http.Header
	if params.IfAbsent {
		headers = http.Header{"If-None-Match": {"*"}}
	}
	var out billing.InvoiceProfile
	if err := c.doWithHeaders(ctx, http.MethodPut, path+"/invoice-profile", params.InvoiceProfile, &out, headers, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
