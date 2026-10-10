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
// that prices its usage for every customer without a rate override. A meter
// with recorded usage keeps its definition (billing.ErrConflict, code
// meter_in_use); a rate card with overrides is not removed (code
// rate_card_has_overrides).
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

func rateOverridePath(customerID billing.CustomerID, meterKey string) (string, error) {
	customer, err := requireTypedID("customer_id", customerID)
	if err != nil {
		return "", err
	}
	key, err := pathID("meter_key", meterKey)
	if err != nil {
		return "", err
	}
	return "/v1/admin/catalog/rate-overrides/" + customer + "/" + key, nil
}

// ListRateOverrides returns one page of customers' negotiated prices, by
// customer then meter: of params.CustomerID and params.MeterKey when set.
func (c *Client) ListRateOverrides(ctx context.Context, params billing.RateOverrideListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.RateOverride], error) {
	q := pageValues(nil, params.PageRequest)
	if !params.CustomerID.IsZero() {
		q.Set("customer_id", params.CustomerID.String())
	}
	if params.MeterKey != "" {
		q.Set("meter_key", params.MeterKey)
	}
	var out billing.ListPage[billing.RateOverride]
	if err := c.do(ctx, http.MethodGet, "/v1/admin/catalog/rate-overrides?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetRateOverride sets a customer's negotiated price for one meter's usage.
func (c *Client) SetRateOverride(ctx context.Context, customerID billing.CustomerID, meterKey string, params billing.SetRateOverrideParams, requestOptions ...RequestOption) (*billing.RateOverride, error) {
	path, err := rateOverridePath(customerID, meterKey)
	if err != nil {
		return nil, err
	}
	var out billing.RateOverride
	if err := c.do(ctx, http.MethodPut, path, params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteRateOverride removes a customer's negotiated price; the meter's rate
// card prices their usage again.
func (c *Client) DeleteRateOverride(ctx context.Context, customerID billing.CustomerID, meterKey string, requestOptions ...RequestOption) error {
	path, err := rateOverridePath(customerID, meterKey)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, path, nil, nil, requestOptions...)
}
