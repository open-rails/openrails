package openrails

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
)

func admissionPath(requestID string) (string, error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return "", invalidErr("request_id is required")
	}
	segment := url.PathEscape(requestID)
	if segment == "." || segment == ".." {
		segment = strings.ReplaceAll(segment, ".", "%2E")
	}
	return "/v1/app/admissions/" + segment, nil
}

// GetAdmission reads an admitted request and its current hold state without
// replaying admission. Denied or no-longer-retained requests are not found.
func (c *Client) GetAdmission(ctx context.Context, requestID string, options ...RequestOption) (*billing.Admission, error) {
	path, err := admissionPath(requestID)
	if err != nil {
		return nil, err
	}
	var out billing.Admission
	if err := c.do(ctx, http.MethodGet, path, nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListAdmissions reads a page of retained admitted requests, newest first.
func (c *Client) ListAdmissions(ctx context.Context, params billing.AdmissionListParams, options ...RequestOption) (*billing.ListPage[billing.Admission], error) {
	q := pageValues(nil, params.PageRequest)
	setQuery(q, map[string]string{"customer_id": params.CustomerID.String(), "currency": params.Currency, "state": string(params.State)})
	if !params.From.IsZero() {
		q.Set("from", params.From.UTC().Format(time.RFC3339Nano))
	}
	if !params.To.IsZero() {
		q.Set("to", params.To.UTC().Format(time.RFC3339Nano))
	}
	var out billing.ListPage[billing.Admission]
	if err := c.do(ctx, http.MethodGet, withQuery("/v1/app/admissions", q), nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetUsageEvent reads a retained event by its stable ID without recording or
// repricing it. Its Replayed flag is false because this is an inspection.
func (c *Client) GetUsageEvent(ctx context.Context, id billing.UsageEventID, options ...RequestOption) (*billing.UsageEvent, error) {
	key, err := requireTypedID("usage_event_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.UsageEvent
	if err := c.do(ctx, http.MethodGet, "/v1/app/usage-events/"+key, nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListUsageEvents reads one page of individual events for reconciliation.
func (c *Client) ListUsageEvents(ctx context.Context, params billing.UsageEventListParams, options ...RequestOption) (*billing.ListPage[billing.UsageEvent], error) {
	q := pageValues(nil, params.PageRequest)
	setQuery(q, map[string]string{"customer_id": params.CustomerID.String(), "currency": params.Currency, "event_type": params.EventType, "source": params.Source, "source_id": params.SourceID, "outcome": string(params.Outcome)})
	if !params.From.IsZero() {
		q.Set("from", params.From.UTC().Format(time.RFC3339Nano))
	}
	if !params.To.IsZero() {
		q.Set("to", params.To.UTC().Format(time.RFC3339Nano))
	}
	var out billing.ListPage[billing.UsageEvent]
	if err := c.do(ctx, http.MethodGet, withQuery("/v1/app/usage-events", q), nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}
