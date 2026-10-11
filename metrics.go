package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// QueryMetrics runs one metrics query.
func (c *Client) QueryMetrics(ctx context.Context, query billing.MetricsQuery, options ...RequestOption) (*billing.MetricsResult, error) {
	var out billing.MetricsResult
	if err := c.do(ctx, http.MethodPost, "/v1/metrics/query", query, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetMetricsSchema lists every measure, dimension and grain a query may use.
func (c *Client) GetMetricsSchema(ctx context.Context, options ...RequestOption) (*billing.MetricsSchema, error) {
	var out billing.MetricsSchema
	if err := c.do(ctx, http.MethodGet, "/v1/metrics/schema", nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetDashboard returns the merchant's widget layout.
func (c *Client) GetDashboard(ctx context.Context, options ...RequestOption) (*billing.Dashboard, error) {
	var out billing.Dashboard
	if err := c.do(ctx, http.MethodGet, "/v1/metrics/dashboard", nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetDashboard replaces the merchant's widget layout.
func (c *Client) SetDashboard(ctx context.Context, req billing.SetDashboardParams, options ...RequestOption) (*billing.Dashboard, error) {
	var out billing.Dashboard
	if err := c.do(ctx, http.MethodPut, "/v1/merchant/dashboard", req, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}
