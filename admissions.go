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

// ReleaseAdmissions frees the holds of 1 to billing.MaxAdmissionBatchItems
// admissions whose work failed, one result per request id in order: each is
// released or refused on its own. Releasing a released admission answers it.
func (c *Client) ReleaseAdmissions(ctx context.Context, requestIDs []string, requestOptions ...RequestOption) ([]billing.AdmissionResult, error) {
	if err := batchSize(len(requestIDs), billing.MaxAdmissionBatchItems); err != nil {
		return nil, err
	}
	var out billing.AdmissionBatchResult
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/admissions/release", billing.ReleaseAdmissionBatchParams{RequestIDs: requestIDs}, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// ExtendAdmissions moves 1 to billing.MaxAdmissionBatchItems open holds'
// deadlines later, one result per item in order. An item refused with
// hold_not_found was captured, released or lapsed; admit it again instead.
func (c *Client) ExtendAdmissions(ctx context.Context, items []billing.ExtendAdmissionParams, requestOptions ...RequestOption) ([]billing.AdmissionResult, error) {
	if err := batchSize(len(items), billing.MaxAdmissionBatchItems); err != nil {
		return nil, err
	}
	body := billing.ExtendAdmissionBatchParams{Items: make([]billing.ExtendAdmissionParams, len(items))}
	for i, item := range items {
		item.ExpiresAt = item.ExpiresAt.UTC()
		body.Items[i] = item
	}
	var out billing.AdmissionBatchResult
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/admissions/extend", body, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// ReportWastedSpend records 1 to billing.MaxBatchItems reports of spend a
// customer's invoker wasted (failed or abusive work), one result per report in
// order: each is handled or refused on its own. Source and SourceID identify a
// report; a retry is handled once (action duplicate).
func (c *Client) ReportWastedSpend(ctx context.Context, items []billing.ReportWastedSpendParams, requestOptions ...RequestOption) ([]billing.WastedSpendResult, error) {
	if err := batchSize(len(items), billing.MaxBatchItems); err != nil {
		return nil, err
	}
	body := billing.ReportWastedSpendBatchParams{Items: make([]billing.ReportWastedSpendParams, len(items))}
	for i, item := range items {
		item.Currency = normalizeCurrency(item.Currency)
		body.Items[i] = item
	}
	var out billing.ReportWastedSpendBatchResult
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/wasted-spend", body, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out.Items, nil
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
