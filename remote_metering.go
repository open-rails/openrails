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

// ListMeters returns one page of the merchant's meters, by key.
func (c *Client) ListMeters(ctx context.Context, page billing.PageRequest, requestOptions ...RequestOption) (*billing.ListPage[billing.Meter], error) {
	var out billing.ListPage[billing.Meter]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/catalog/meters?"+pageValues(nil, page).Encode(), nil, &out, requestOptions...); err != nil {
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
// definition. A meter with recorded usage keeps its definition
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

// SetMeterRateCard sets the rate card that prices the meter's usage for every
// customer without a rate override.
func (c *Client) SetMeterRateCard(ctx context.Context, key string, params billing.SetMeterRateCardParams, requestOptions ...RequestOption) (*billing.Meter, error) {
	path, err := meterPath(key)
	if err != nil {
		return nil, err
	}
	var out billing.Meter
	if err := c.do(ctx, http.MethodPut, path+"/rate-card", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteMeterRateCard removes a meter's rate card once no customer has a rate
// override of it.
func (c *Client) DeleteMeterRateCard(ctx context.Context, key string, requestOptions ...RequestOption) error {
	path, err := meterPath(key)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, path+"/rate-card", nil, nil, requestOptions...)
}

// ListMeterRateOverrides returns one page of the customers whose negotiated
// price replaces the meter's rate card, by customer.
func (c *Client) ListMeterRateOverrides(ctx context.Context, key string, page billing.PageRequest, requestOptions ...RequestOption) (*billing.ListPage[billing.RateOverride], error) {
	path, err := meterPath(key)
	if err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.RateOverride]
	if err := c.do(ctx, http.MethodGet, path+"/rate-overrides?"+pageValues(nil, page).Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func rateOverridesPath(customerID billing.CustomerID) (string, error) {
	customer, err := requireTypedID("customer_id", customerID)
	if err != nil {
		return "", err
	}
	return "/v1/merchant/customers/" + customer + "/rate-overrides", nil
}

// ListRateOverrides returns one page of a customer's negotiated prices, by
// meter.
func (c *Client) ListRateOverrides(ctx context.Context, customerID billing.CustomerID, page billing.PageRequest, requestOptions ...RequestOption) (*billing.ListPage[billing.RateOverride], error) {
	path, err := rateOverridesPath(customerID)
	if err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.RateOverride]
	if err := c.do(ctx, http.MethodGet, path+"?"+pageValues(nil, page).Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetRateOverride sets a customer's negotiated price for one meter's usage.
func (c *Client) SetRateOverride(ctx context.Context, customerID billing.CustomerID, meterKey string, params billing.SetRateOverrideParams, requestOptions ...RequestOption) (*billing.RateOverride, error) {
	path, err := rateOverridesPath(customerID)
	if err != nil {
		return nil, err
	}
	key, err := pathID("meter_key", meterKey)
	if err != nil {
		return nil, err
	}
	var out billing.RateOverride
	if err := c.do(ctx, http.MethodPut, path+"/"+key, params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteRateOverride removes a customer's negotiated price; the meter's rate
// card prices their usage again.
func (c *Client) DeleteRateOverride(ctx context.Context, customerID billing.CustomerID, meterKey string, requestOptions ...RequestOption) error {
	path, err := rateOverridesPath(customerID)
	if err != nil {
		return err
	}
	key, err := pathID("meter_key", meterKey)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, path+"/"+key, nil, nil, requestOptions...)
}
