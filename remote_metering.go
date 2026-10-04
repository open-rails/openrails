package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

func meterPath(key string) (string, error) {
	key, err := pathID("key", key)
	if err != nil {
		return "", err
	}
	return "/v1/merchant/catalog/meters/" + key, nil
}

// GetUsageMeter reads one usage meter by key.
func (c *Client) GetUsageMeter(ctx context.Context, key string, requestOptions ...RequestOption) (*billing.UsageMeterDTO, error) {
	path, err := meterPath(key)
	if err != nil {
		return nil, err
	}
	var out billing.UsageMeterDTO
	if err := c.do(ctx, http.MethodGet, path, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListUsageMeters returns one page of the merchant's usage meters, and the
// total count.
func (c *Client) ListUsageMeters(ctx context.Context, options billing.PageOptions, requestOptions ...RequestOption) ([]billing.UsageMeterDTO, int64, error) {
	var out struct {
		Items []billing.UsageMeterDTO `json:"items"`
		Total int64                   `json:"total"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/catalog/meters?"+pageQuery(options).Encode(), nil, &out, requestOptions...); err != nil {
		return nil, 0, err
	}
	return out.Items, out.Total, nil
}

// EnsureUsageMeter creates the meter spec.Key, or updates its definition.
func (c *Client) EnsureUsageMeter(ctx context.Context, spec billing.UsageMeterSpec, requestOptions ...RequestOption) error {
	path, err := meterPath(spec.Key)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPut, path, billing.UsageMeterRequest{EventType: spec.EventType, ValueProperty: spec.ValueProperty, Aggregation: spec.Aggregation, Unit: spec.Unit, GroupBy: spec.GroupBy}, nil, requestOptions...)
}

// SetDefaultUsageRateCard sets the rate card that prices a meter's usage for
// customers without a negotiated rate.
func (c *Client) SetDefaultUsageRateCard(ctx context.Context, key string, request billing.DefaultUsageRateCardRequest, requestOptions ...RequestOption) (*billing.UsageMeterDTO, error) {
	path, err := meterPath(key)
	if err != nil {
		return nil, err
	}
	var out billing.UsageMeterDTO
	if err := c.do(ctx, http.MethodPut, path+"/rate-card", request, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteDefaultUsageRateCard removes a meter's default rate card.
func (c *Client) DeleteDefaultUsageRateCard(ctx context.Context, key string, requestOptions ...RequestOption) error {
	path, err := meterPath(key)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, path+"/rate-card", nil, nil, requestOptions...)
}
