package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// ListWorkerHealth returns each background job kind's recent runs.
func (c *Client) ListWorkerHealth(ctx context.Context, options ...RequestOption) (*billing.ListPage[billing.WorkerHealth], error) {
	var out billing.ListPage[billing.WorkerHealth]
	if err := c.do(ctx, http.MethodGet, "/v1/admin/worker-health", nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListFindings pages the findings queue: open findings unless req.Status
// names another, most severe first, then oldest. req.Type is one finding type
// or a prefix ending in ".*" ("catalog.*" lists catalog drift).
func (c *Client) ListFindings(ctx context.Context, req billing.FindingListParams, options ...RequestOption) (*billing.ListPage[billing.Finding], error) {
	q := cursorQuery(req.PageRequest)
	for key, value := range map[string]string{"status": string(req.Status), "severity": req.Severity, "type": req.Type} {
		if value != "" {
			q.Set(key, value)
		}
	}
	if err := setIDs(q, req.IDs); err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.Finding]
	if err := c.do(ctx, http.MethodGet, "/v1/admin/findings?"+q.Encode(), nil, &out, options...); err != nil {
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
	if err := c.do(ctx, http.MethodGet, "/v1/admin/findings/"+id.String(), nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ResolveFinding approves (runs the recommendation) or ignores one open
// finding.
func (c *Client) ResolveFinding(ctx context.Context, id billing.FindingID, req billing.ResolveFindingParams, options ...RequestOption) (*billing.FindingResolution, error) {
	if id.IsZero() {
		return nil, invalidErr("finding id is required")
	}
	var out billing.FindingResolution
	if err := c.do(ctx, http.MethodPost, "/v1/admin/findings/"+id.String()+"/resolve", req, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}
