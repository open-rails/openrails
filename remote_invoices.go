package openrails

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
)

// ListMerchantInvoices returns one page of the merchant's invoices matching
// filter, and the total count.
func (c *Client) ListMerchantInvoices(ctx context.Context, filter billing.MerchantInvoiceFilter, limit, offset int, requestOptions ...RequestOption) ([]billing.MerchantInvoiceDTO, int64, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)}}
	if filter.CustomerID != "" {
		q.Set("customer_id", filter.CustomerID)
	}
	if filter.Currency != nil {
		q.Set("currency", normalizeCurrency(*filter.Currency))
	}
	if filter.Status != nil {
		q.Set("status", *filter.Status)
	}
	if filter.PeriodFrom != nil {
		q.Set("period_from", filter.PeriodFrom.Format(time.RFC3339Nano))
	}
	if filter.PeriodTo != nil {
		q.Set("period_to", filter.PeriodTo.Format(time.RFC3339Nano))
	}
	var out struct {
		Items []billing.MerchantInvoiceDTO `json:"items"`
		Total int64                        `json:"total"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/invoices?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, 0, err
	}
	return out.Items, out.Total, nil
}

func invoicePath(id uuid.UUID) (string, error) {
	invoice, err := requireUUID("invoice_id", id)
	if err != nil {
		return "", err
	}
	return "/v1/merchant/invoices/" + invoice, nil
}

// GetMerchantInvoice reads one invoice.
func (c *Client) GetMerchantInvoice(ctx context.Context, id uuid.UUID, requestOptions ...RequestOption) (*billing.MerchantInvoiceDTO, error) {
	path, err := invoicePath(id)
	if err != nil {
		return nil, err
	}
	var out billing.MerchantInvoiceDTO
	if err := c.do(ctx, http.MethodGet, path, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// VoidInvoice voids a draft, open or past-due invoice. Repeating it changes
// nothing.
func (c *Client) VoidInvoice(ctx context.Context, id uuid.UUID, requestOptions ...RequestOption) (*billing.MerchantInvoiceDTO, error) {
	path, err := invoicePath(id)
	if err != nil {
		return nil, err
	}
	var out billing.MerchantInvoiceDTO
	if err := c.do(ctx, http.MethodPost, path+"/void", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// MarkInvoiceUncollectible marks an open or past-due invoice uncollectible.
// Repeating it changes nothing.
func (c *Client) MarkInvoiceUncollectible(ctx context.Context, id uuid.UUID, requestOptions ...RequestOption) (*billing.MerchantInvoiceDTO, error) {
	path, err := invoicePath(id)
	if err != nil {
		return nil, err
	}
	var out billing.MerchantInvoiceDTO
	if err := c.do(ctx, http.MethodPost, path+"/uncollectible", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// RecordInvoicePayment records money received outside automatic collection.
// The request's Reference identifies the remittance, so a retry records it
// once.
func (c *Client) RecordInvoicePayment(ctx context.Context, id uuid.UUID, request billing.RecordInvoicePaymentRequest, requestOptions ...RequestOption) (*billing.MerchantInvoiceDTO, error) {
	path, err := invoicePath(id)
	if err != nil {
		return nil, err
	}
	var out billing.MerchantInvoiceDTO
	if err := c.do(ctx, http.MethodPost, path+"/payments", request, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// RetryInvoiceCollection collects an open invoice now. The same
// IdempotencyKey replays the attempt.
func (c *Client) RetryInvoiceCollection(ctx context.Context, request billing.InvoiceCollectionRetryRequest, requestOptions ...RequestOption) (*billing.InvoiceCollectionRetryResult, error) {
	path, err := invoicePath(request.InvoiceID)
	if err != nil {
		return nil, err
	}
	var out billing.InvoiceCollectionRetryResult
	if err := c.doWithHeaders(ctx, http.MethodPost, path+"/retry-collection", request, &out, http.Header{"Idempotency-Key": {request.IdempotencyKey}}, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListInvoicePaymentAttempts returns one page of an invoice's payment
// attempts, and the total count.
func (c *Client) ListInvoicePaymentAttempts(ctx context.Context, id uuid.UUID, limit, offset int, requestOptions ...RequestOption) ([]billing.InvoicePaymentAttemptDTO, int64, error) {
	path, err := invoicePath(id)
	if err != nil {
		return nil, 0, err
	}
	var out struct {
		Items []billing.InvoicePaymentAttemptDTO `json:"items"`
		Total int64                              `json:"total"`
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)}}
	if err := c.do(ctx, http.MethodGet, path+"/payments?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, 0, err
	}
	return out.Items, out.Total, nil
}

// GetCustomerInvoiceProfile reads how a customer is invoiced: payment terms,
// collection method, PO number, tax details and billing contacts.
func (c *Client) GetCustomerInvoiceProfile(ctx context.Context, customerID string, requestOptions ...RequestOption) (*billing.InvoiceProfileDTO, error) {
	path, err := customerPath(customerID)
	if err != nil {
		return nil, err
	}
	var out struct {
		Profile *billing.InvoiceProfileDTO `json:"profile"`
	}
	if err := c.do(ctx, http.MethodGet, path+"/invoice-profile", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out.Profile, nil
}

// SetCustomerInvoiceProfile replaces how a customer is invoiced (see
// GetCustomerInvoiceProfile).
func (c *Client) SetCustomerInvoiceProfile(ctx context.Context, customerID string, profile billing.InvoiceProfileDTO, requestOptions ...RequestOption) error {
	path, err := customerPath(customerID)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPut, path+"/invoice-profile", profile, nil, requestOptions...)
}

// EnsureCustomerInvoiceProfile installs defaults only when no profile exists.
// A concurrent operator update is never overwritten.
func (c *Client) EnsureCustomerInvoiceProfile(ctx context.Context, customerID string, profile billing.InvoiceProfileDTO, requestOptions ...RequestOption) (bool, error) {
	path, err := customerPath(customerID)
	if err != nil {
		return false, err
	}
	err = c.doWithHeaders(ctx, http.MethodPut, path+"/invoice-profile", profile, nil, http.Header{"If-None-Match": {"*"}}, requestOptions...)
	var status *billing.StatusError
	if errors.As(err, &status) && status.Status == http.StatusPreconditionFailed {
		return false, nil
	}
	return err == nil, err
}
