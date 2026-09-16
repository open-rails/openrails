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

func (c *Client) ListMerchantInvoices(ctx context.Context, filter MerchantInvoiceFilter, limit, offset int) ([]MerchantInvoiceDTO, int64, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)}}
	if filter.CustomerID != nil {
		q.Set("customer_id", filter.CustomerID.String())
	}
	if filter.Currency != nil {
		q.Set("currency", *filter.Currency)
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
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/invoices?"+q.Encode(), nil, &out); err != nil {
		return nil, 0, err
	}
	return out.Items, out.Total, nil
}

func (c *Client) GetMerchantInvoice(ctx context.Context, id uuid.UUID) (*MerchantInvoiceDTO, error) {
	var out MerchantInvoiceDTO
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/invoices/"+id.String(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) VoidInvoice(ctx context.Context, id uuid.UUID) (*MerchantInvoiceDTO, error) {
	var out MerchantInvoiceDTO
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/invoices/"+id.String()+"/void", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) MarkInvoiceUncollectible(ctx context.Context, id uuid.UUID) (*MerchantInvoiceDTO, error) {
	var out MerchantInvoiceDTO
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/invoices/"+id.String()+"/uncollectible", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) RecordInvoicePayment(ctx context.Context, id uuid.UUID, request RecordInvoicePaymentRequest) (*MerchantInvoiceDTO, error) {
	var out MerchantInvoiceDTO
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/invoices/"+id.String()+"/payments", request, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) RetryInvoiceCollection(ctx context.Context, request InvoiceCollectionRetryRequest) (*InvoiceCollectionRetryResult, error) {
	var out InvoiceCollectionRetryResult
	path := "/v1/merchant/invoices/" + request.InvoiceID.String() + "/retry-collection"
	if err := c.doWithHeaders(ctx, http.MethodPost, path, request, &out, http.Header{"Idempotency-Key": {request.IdempotencyKey}}); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListInvoicePaymentAttempts(ctx context.Context, id uuid.UUID, limit, offset int) ([]InvoicePaymentAttemptDTO, int64, error) {
	var out struct {
		Items []InvoicePaymentAttemptDTO `json:"items"`
		Total int64                      `json:"total"`
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)}}
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/invoices/"+id.String()+"/payments?"+q.Encode(), nil, &out); err != nil {
		return nil, 0, err
	}
	return out.Items, out.Total, nil
}

func (c *Client) GetCustomerInvoiceProfile(ctx context.Context, customerID string) (*InvoiceProfileDTO, error) {
	var out struct {
		Profile *InvoiceProfileDTO `json:"profile"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/customers/"+url.PathEscape(customerID)+"/invoice-profile", nil, &out); err != nil {
		return nil, err
	}
	return out.Profile, nil
}

func (c *Client) SetCustomerInvoiceProfile(ctx context.Context, customerID string, profile InvoiceProfileDTO) error {
	return c.do(ctx, http.MethodPut, "/v1/merchant/customers/"+url.PathEscape(customerID)+"/invoice-profile", profile, nil)
}

// EnsureCustomerInvoiceProfile installs defaults only when no profile exists.
// A concurrent operator update is never overwritten.
func (c *Client) EnsureCustomerInvoiceProfile(ctx context.Context, customerID string, profile InvoiceProfileDTO) (bool, error) {
	err := c.doWithHeaders(ctx, http.MethodPut, "/v1/merchant/customers/"+url.PathEscape(customerID)+"/invoice-profile", profile, nil, http.Header{"If-None-Match": {"*"}})
	var status *StatusError
	if errors.As(err, &status) && status.Status == http.StatusPreconditionFailed {
		return false, nil
	}
	return err == nil, err
}
