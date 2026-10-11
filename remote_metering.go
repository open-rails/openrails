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
	return "/v1/admin/catalog/meters/" + key, nil
}

// ListMeters returns one page of the merchant's meters, by key.
func (c *Client) ListMeters(ctx context.Context, page billing.PageRequest, requestOptions ...RequestOption) (*billing.ListPage[billing.Meter], error) {
	var out billing.ListPage[billing.Meter]
	if err := c.do(ctx, http.MethodGet, "/v1/admin/catalog/meters?"+pageValues(nil, page).Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetMeter reads one meter with its rate card.
func (c *Client) GetMeter(ctx context.Context, key string, requestOptions ...RequestOption) (*billing.Meter, error) {
	path, err := meterPath(key)
	if err != nil {
		return nil, err
	}
	var out billing.Meter
	if err := c.do(ctx, http.MethodGet, path, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetMeter declares the meter under key, creating it or changing its
// definition, and with params.RateCard sets or (null) removes the rate card
// that prices its usage. A meter with recorded usage keeps its definition
// (billing.ErrConflict, code meter_in_use).
func (c *Client) SetMeter(ctx context.Context, key string, params billing.SetMeterParams, requestOptions ...RequestOption) (*billing.Meter, error) {
	path, err := meterPath(key)
	if err != nil {
		return nil, err
	}
	var out billing.Meter
	if err := c.do(ctx, http.MethodPut, path, params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
