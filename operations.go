package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// ListRepairAlerts pages ledger repairs that need the merchant.
func (c *Client) ListRepairAlerts(ctx context.Context, req billing.ListRepairAlertsRequest, options ...RequestOption) (billing.ListPage[billing.Notification], error) {
	q := cursorQuery(req.PageRequest)
	if req.Seen != nil {
		q.Set("seen", map[bool]string{true: "true", false: "false"}[*req.Seen])
	}
	var out billing.ListPage[billing.Notification]
	err := c.do(ctx, http.MethodGet, "/v1/merchant/repair-alerts?"+q.Encode(), nil, &out, options...)
	return out, err
}

// ListWorkerHealth returns each background job kind's recent runs.
func (c *Client) ListWorkerHealth(ctx context.Context, options ...RequestOption) (billing.ListPage[billing.WorkerHealth], error) {
	var out billing.ListPage[billing.WorkerHealth]
	err := c.do(ctx, http.MethodGet, "/v1/merchant/worker-health", nil, &out, options...)
	return out, err
}

// ListFindings pages the findings queue: open findings unless req.Status
// names another, most severe first, then oldest.
func (c *Client) ListFindings(ctx context.Context, req billing.ListFindingsRequest, options ...RequestOption) (billing.ListPage[billing.Finding], error) {
	q := cursorQuery(req.PageRequest)
	for key, value := range map[string]string{"status": string(req.Status), "severity": req.Severity, "finding_type": req.Type} {
		if value != "" {
			q.Set(key, value)
		}
	}
	var out billing.ListPage[billing.Finding]
	err := c.do(ctx, http.MethodGet, "/v1/merchant/findings?"+q.Encode(), nil, &out, options...)
	return out, err
}

// GetFindingSummary returns the findings queue at a glance.
func (c *Client) GetFindingSummary(ctx context.Context, options ...RequestOption) (*billing.FindingSummary, error) {
	var out billing.FindingSummary
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/findings/summary", nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetFinding returns one finding with its evidence and recommendation.
func (c *Client) GetFinding(ctx context.Context, id billing.FindingID, options ...RequestOption) (*billing.Finding, error) {
	if id.IsZero() {
		return nil, invalidErr("finding id is required")
	}
	var out billing.Finding
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/findings/"+id.String(), nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ResolveFinding approves (runs the recommendation) or ignores one open
// finding.
func (c *Client) ResolveFinding(ctx context.Context, id billing.FindingID, req billing.ResolveFindingRequest, options ...RequestOption) (*billing.FindingResolution, error) {
	if id.IsZero() {
		return nil, invalidErr("finding id is required")
	}
	var out billing.FindingResolution
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/findings/"+id.String()+"/resolve", req, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}
