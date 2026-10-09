package openrails

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
)

// Admit decides admissions, one verdict per request in order. A refused
// request is a verdict, not an error.
func (c *Client) Admit(ctx context.Context, requests []billing.AdmitParams, requestOptions ...RequestOption) ([]billing.AdmissionVerdict, error) {
	var out billing.AdmitBatchResult
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/admissions", billing.AdmitBatchParams{Items: requests}, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// admissionPath keeps an arbitrary request id one URL segment: PathEscape
// leaves dot segments intact, which ServeMux would clean away.
func admissionPath(requestID string) (string, error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return "", invalidErr("request_id is required")
	}
	segment := url.PathEscape(requestID)
	if segment == "." || segment == ".." {
		segment = strings.ReplaceAll(segment, ".", "%2E")
	}
	return "/v1/merchant/admissions/" + segment, nil
}

// GetAdmission reads an allowed admission and its hold.
func (c *Client) GetAdmission(ctx context.Context, requestID string, requestOptions ...RequestOption) (*billing.Admission, error) {
	path, err := admissionPath(requestID)
	if err != nil {
		return nil, err
	}
	var out billing.Admission
	if err := c.do(ctx, http.MethodGet, path, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// CaptureAdmission settles an admitted request. An exact retry returns the
// original receipt; a changed amount or usage is
// billing.ErrIdempotencyKeyReused.
func (c *Client) CaptureAdmission(ctx context.Context, requestID string, params billing.CaptureAdmissionParams, requestOptions ...RequestOption) (*billing.CaptureReceipt, error) {
	path, err := admissionPath(requestID)
	if err != nil {
		return nil, err
	}
	var out billing.CaptureReceipt
	if err := c.do(ctx, http.MethodPost, path+"/capture", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReleaseAdmission frees the hold of an admission whose work failed.
// Releasing a released admission returns it.
func (c *Client) ReleaseAdmission(ctx context.Context, requestID string, requestOptions ...RequestOption) (*billing.Admission, error) {
	path, err := admissionPath(requestID)
	if err != nil {
		return nil, err
	}
	var out billing.Admission
	if err := c.do(ctx, http.MethodPost, path+"/release", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ExtendAdmission moves an open hold's deadline later. billing.ErrNotFound
// means the hold was captured, released or lapsed; admit again instead.
func (c *Client) ExtendAdmission(ctx context.Context, requestID string, params billing.ExtendAdmissionParams, requestOptions ...RequestOption) (*billing.Admission, error) {
	path, err := admissionPath(requestID)
	if err != nil {
		return nil, err
	}
	if params.ExpiresAt.IsZero() {
		return nil, invalidErr("expires_at is required")
	}
	params.ExpiresAt = params.ExpiresAt.UTC()
	var out billing.Admission
	if err := c.do(ctx, http.MethodPost, path+"/extend", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReportWastedSpend records spend a customer's invoker wasted (failed or
// abusive work). Source and SourceID identify the report.
func (c *Client) ReportWastedSpend(ctx context.Context, params billing.ReportWastedSpendParams, requestOptions ...RequestOption) (*billing.WastedSpendReport, error) {
	params.Currency = normalizeCurrency(params.Currency)
	var out billing.WastedSpendReport
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/wasted-spend", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// RecordUsage records 1 to billing.MaxUsageBatchItems usage events, one
// result per item in order: each is recorded or refused on its own, exactly
// as recording it alone would be. An event's source and source id make a
// retry record and charge it once (Status 200, Replayed); a retry with a
// different amount is refused with idempotency_key_reused.
func (c *Client) RecordUsage(ctx context.Context, items []billing.RecordUsageParams, requestOptions ...RequestOption) ([]billing.UsageEventResult, error) {
	if err := batchSize(len(items), billing.MaxUsageBatchItems); err != nil {
		return nil, err
	}
	body := billing.RecordUsageBatchParams{Items: make([]billing.RecordUsageParams, len(items))}
	for i, item := range items {
		item.Currency = normalizeCurrency(item.Currency)
		body.Items[i] = item
	}
	var out billing.RecordUsageBatchResult
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/usage-events", body, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// GetUsage reports a customer's usage in one currency over [From, To),
// grouped by GroupBy. A zero window is the month before now.
func (c *Client) GetUsage(ctx context.Context, customer billing.CustomerID, params billing.GetUsageParams, requestOptions ...RequestOption) (*billing.Usage, error) {
	path, err := customerIDPath(customer)
	if err != nil {
		return nil, err
	}
	q := url.Values{"currency": {normalizeCurrency(params.Currency)}}
	if !params.From.IsZero() {
		q.Set("from", params.From.UTC().Format(time.RFC3339Nano))
	}
	if !params.To.IsZero() {
		q.Set("to", params.To.UTC().Format(time.RFC3339Nano))
	}
	if params.GroupBy != "" {
		q.Set("group_by", string(params.GroupBy))
	}
	var out billing.Usage
	if err := c.do(ctx, http.MethodGet, withQuery(path+"/usage", q), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
