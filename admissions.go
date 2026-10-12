package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// Admit decides admissions, one verdict per request in order. A refused
// request is a verdict, not an error.
func (c *Client) Admit(ctx context.Context, requests []billing.AdmitParams, requestOptions ...RequestOption) ([]billing.AdmissionVerdict, error) {
	var out billing.AdmitBatchResult
	if err := c.do(ctx, http.MethodPost, "/v1/app/admissions", billing.AdmitBatchParams{Items: requests}, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// CaptureAdmissions settles 1 to billing.MaxAdmissionBatchItems admitted
// requests, one result per item in order: each is captured or refused on its
// own. A capture that names its usage is priced by the admission's pinned
// rate cards; above the hold the rest is owed. An exact retry answers the
// first receipt; changed terms are billing.ErrIdempotencyKeyReused.
func (c *Client) CaptureAdmissions(ctx context.Context, items []billing.CaptureAdmissionParams, requestOptions ...RequestOption) ([]billing.CaptureResult, error) {
	if err := batchSize(len(items), billing.MaxAdmissionBatchItems); err != nil {
		return nil, err
	}
	var out billing.CaptureBatchResult
	if err := c.do(ctx, http.MethodPost, "/v1/app/admissions/capture", billing.CaptureAdmissionBatchParams{Items: items}, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// ReleaseAdmissions frees the holds of 1 to billing.MaxAdmissionBatchItems
// admissions whose work failed, one result per request id in order: each is
// released or refused on its own. Releasing a released admission answers it.
func (c *Client) ReleaseAdmissions(ctx context.Context, requestIDs []string, requestOptions ...RequestOption) ([]billing.AdmissionResult, error) {
	if err := batchSize(len(requestIDs), billing.MaxAdmissionBatchItems); err != nil {
		return nil, err
	}
	var out billing.AdmissionBatchResult
	if err := c.do(ctx, http.MethodPost, "/v1/app/admissions/release", billing.ReleaseAdmissionBatchParams{RequestIDs: requestIDs}, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// ExtendAdmissions moves 1 to billing.MaxAdmissionBatchItems open holds'
// deadlines later, one result per item in order. An item refused with
// hold_not_found was captured, released or lapsed. New work needs a new
// request ID; replaying the old request never reopens its hold.
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
	if err := c.do(ctx, http.MethodPost, "/v1/app/admissions/extend", body, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// RecordUsage records 1 to billing.MaxUsageBatchItems usage events, one
// result per item in order: each is recorded or refused on its own, exactly
// as recording it alone would be. An event's source and source id make a
// retry record and charge it once (Status 200, Replayed); a retry with a
// different amount or outcome is refused with idempotency_key_reused. An
// item with Outcome failed is forgiven up to the customer's grace and charged
// past it (see billing.UsageFailed).
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
	if err := c.do(ctx, http.MethodPost, "/v1/app/usage-events", body, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out.Items, nil
}
