package openrails

import (
	"context"
	"net/http"
)

func meterPath(key string) (string, error) {
	key, err := pathID("key", key)
	if err != nil {
		return "", err
	}
	return "/v1/merchant/catalog/meters/" + key, nil
}

func (c *Client) GetUsageMeter(ctx context.Context, key string) (*UsageMeterDTO, error) {
	path, err := meterPath(key)
	if err != nil {
		return nil, err
	}
	var out UsageMeterDTO
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListUsageMeters(ctx context.Context, options PageOptions) ([]UsageMeterDTO, int64, error) {
	var out struct {
		Items []UsageMeterDTO `json:"items"`
		Total int64           `json:"total"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/catalog/meters?"+pageQuery(options).Encode(), nil, &out); err != nil {
		return nil, 0, err
	}
	return out.Items, out.Total, nil
}

func (c *Client) EnsureUsageMeter(ctx context.Context, spec UsageMeterSpec) error {
	path, err := meterPath(spec.Key)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPut, path, UsageMeterRequest{EventType: spec.EventType, ValueProperty: spec.ValueProperty, Aggregation: spec.Aggregation, Unit: spec.Unit, GroupBy: spec.GroupBy}, nil)
}

func (c *Client) SetDefaultUsageRateCard(ctx context.Context, key string, request DefaultUsageRateCardRequest) (*UsageMeterDTO, error) {
	path, err := meterPath(key)
	if err != nil {
		return nil, err
	}
	var out UsageMeterDTO
	if err := c.do(ctx, http.MethodPut, path+"/rate-card", request, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) DeleteDefaultUsageRateCard(ctx context.Context, key string) error {
	path, err := meterPath(key)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, path+"/rate-card", nil, nil)
}
