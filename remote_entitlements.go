package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// GrantEntitlement records an admin-sourced entitlement for a customer.
func (c *Client) GrantEntitlement(ctx context.Context, customerID string, request billing.GrantEntitlementRequest, requestOptions ...RequestOption) (*billing.EntitlementRecord, error) {
	path, err := customerPath(customerID)
	if err != nil {
		return nil, err
	}
	request.Entitlement, err = requireID("entitlement", request.Entitlement)
	if err != nil {
		return nil, err
	}
	var out billing.EntitlementRecord
	if err := c.do(ctx, http.MethodPost, path+"/entitlements", request, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// RevokeEntitlement revokes one entitlement window owned by the customer.
func (c *Client) RevokeEntitlement(ctx context.Context, customerID string, entitlementID string, requestOptions ...RequestOption) error {
	path, err := customerPath(customerID)
	if err != nil {
		return err
	}
	entitlement, err := pathID("entitlement_id", entitlementID)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, path+"/entitlements/"+entitlement, nil, nil, requestOptions...)
}
