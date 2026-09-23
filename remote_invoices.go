package openrails

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
)

func (c *Client) ListMerchantInvoices(ctx context.Context, filter MerchantInvoiceFilter, limit, offset int, requestOptions ...RequestOption) ([]MerchantInvoiceDTO, int64, error) {
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
		Items []MerchantInvoiceDTO `json:"items"`
		Total int64                `json:"total"`
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

func (c *Client) GetMerchantInvoice(ctx context.Context, id uuid.UUID, requestOptions ...RequestOption) (*MerchantInvoiceDTO, error) {
	path, err := invoicePath(id)
	if err != nil {
		return nil, err
	}
	var out MerchantInvoiceDTO
	if err := c.do(ctx, http.MethodGet, path, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) VoidInvoice(ctx context.Context, id uuid.UUID, requestOptions ...RequestOption) (*MerchantInvoiceDTO, error) {
	path, err := invoicePath(id)
	if err != nil {
		return nil, err
	}
	var out MerchantInvoiceDTO
	if err := c.do(ctx, http.MethodPost, path+"/void", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) MarkInvoiceUncollectible(ctx context.Context, id uuid.UUID, requestOptions ...RequestOption) (*MerchantInvoiceDTO, error) {
	path, err := invoicePath(id)
	if err != nil {
		return nil, err
	}
	var out MerchantInvoiceDTO
	if err := c.do(ctx, http.MethodPost, path+"/uncollectible", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) RecordInvoicePayment(ctx context.Context, id uuid.UUID, request RecordInvoicePaymentRequest, requestOptions ...RequestOption) (*MerchantInvoiceDTO, error) {
	path, err := invoicePath(id)
	if err != nil {
		return nil, err
	}
	var out MerchantInvoiceDTO
	if err := c.do(ctx, http.MethodPost, path+"/payments", request, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) RetryInvoiceCollection(ctx context.Context, request InvoiceCollectionRetryRequest, requestOptions ...RequestOption) (*InvoiceCollectionRetryResult, error) {
	path, err := invoicePath(request.InvoiceID)
	if err != nil {
		return nil, err
	}
	var out InvoiceCollectionRetryResult
	if err := c.doWithHeaders(ctx, http.MethodPost, path+"/retry-collection", request, &out, http.Header{"Idempotency-Key": {request.IdempotencyKey}}, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListInvoicePaymentAttempts(ctx context.Context, id uuid.UUID, limit, offset int, requestOptions ...RequestOption) ([]InvoicePaymentAttemptDTO, int64, error) {
	path, err := invoicePath(id)
	if err != nil {
		return nil, 0, err
	}
	var out struct {
		Items []InvoicePaymentAttemptDTO `json:"items"`
		Total int64                      `json:"total"`
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)}}
	if err := c.do(ctx, http.MethodGet, path+"/payments?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, 0, err
	}
	return out.Items, out.Total, nil
}

func (c *Client) GetCustomerInvoiceProfile(ctx context.Context, customerID string, requestOptions ...RequestOption) (*InvoiceProfileDTO, error) {
	path, err := customerPath(customerID)
	if err != nil {
		return nil, err
	}
	var out struct {
		Profile *InvoiceProfileDTO `json:"profile"`
	}
	if err := c.do(ctx, http.MethodGet, path+"/invoice-profile", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out.Profile, nil
}

func (c *Client) SetCustomerInvoiceProfile(ctx context.Context, customerID string, profile InvoiceProfileDTO, requestOptions ...RequestOption) error {
	path, err := customerPath(customerID)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPut, path+"/invoice-profile", profile, nil, requestOptions...)
}

// EnsureCustomerInvoiceProfile installs defaults only when no profile exists.
// A concurrent operator update is never overwritten.
func (c *Client) EnsureCustomerInvoiceProfile(ctx context.Context, customerID string, profile InvoiceProfileDTO, requestOptions ...RequestOption) (bool, error) {
	path, err := customerPath(customerID)
	if err != nil {
		return false, err
	}
	err = c.doWithHeaders(ctx, http.MethodPut, path+"/invoice-profile", profile, nil, http.Header{"If-None-Match": {"*"}}, requestOptions...)
	var status *StatusError
	if errors.As(err, &status) && status.Status == http.StatusPreconditionFailed {
		return false, nil
	}
	return err == nil, err
}
