package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// QueryMetrics runs one metrics query.
func (c *Client) QueryMetrics(ctx context.Context, query billing.MetricsQuery, options ...RequestOption) (*billing.MetricsResult, error) {
	var out billing.MetricsResult
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/metrics/query", query, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetMetricsSchema lists every measure, dimension and grain a query may use.
func (c *Client) GetMetricsSchema(ctx context.Context, options ...RequestOption) (*billing.MetricsSchema, error) {
	var out billing.MetricsSchema
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/metrics/schema", nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// AskMetrics answers a question about the merchant's metrics with a model that
// runs queries; the deployment must enable it.
func (c *Client) AskMetrics(ctx context.Context, req billing.AskMetricsRequest, options ...RequestOption) (*billing.MetricsAnswer, error) {
	var out billing.MetricsAnswer
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/metrics/ask", req, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetDashboard returns the merchant's widget layout.
func (c *Client) GetDashboard(ctx context.Context, options ...RequestOption) (*billing.Dashboard, error) {
	var out billing.Dashboard
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/dashboard", nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetDashboard replaces the merchant's widget layout.
func (c *Client) SetDashboard(ctx context.Context, req billing.SetDashboardRequest, options ...RequestOption) (*billing.Dashboard, error) {
	var out billing.Dashboard
	if err := c.do(ctx, http.MethodPut, "/v1/merchant/dashboard", req, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GenerateDashboardWidget writes a widget from a description; the deployment
// must enable it.
func (c *Client) GenerateDashboardWidget(ctx context.Context, req billing.GenerateWidgetRequest, options ...RequestOption) (*billing.GeneratedWidget, error) {
	var out billing.GeneratedWidget
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/dashboard/widgets/generate", req, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}
