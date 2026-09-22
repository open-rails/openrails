package openrails

import (
	"context"
	"net/http"
	"time"
)

// GrantEntitlementRequest grants an admin-sourced entitlement window. Omit both
// Hours and EndAt for an indefinite grant; Hours extends the customer's finite
// timeline, EndAt fixes this grant's own end.
type GrantEntitlementRequest struct {
	Entitlement string     `json:"entitlement"`
	Hours       *int       `json:"hours,omitempty"`
	EndAt       *time.Time `json:"end_at,omitempty"`
}

// GrantEntitlement records an admin-sourced entitlement for a customer.
func (c *Client) GrantEntitlement(ctx context.Context, customerID string, request GrantEntitlementRequest) (*EntitlementRecord, error) {
	path, err := customerPath(customerID)
	if err != nil {
		return nil, err
	}
	request.Entitlement, err = requireID("entitlement", request.Entitlement)
	if err != nil {
		return nil, err
	}
	var out EntitlementRecord
	if err := c.do(ctx, http.MethodPost, path+"/entitlements", request, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RevokeEntitlement revokes one entitlement window owned by the customer.
func (c *Client) RevokeEntitlement(ctx context.Context, customerID string, entitlementID string) error {
	path, err := customerPath(customerID)
	if err != nil {
		return err
	}
	entitlement, err := pathID("entitlement_id", entitlementID)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, path+"/entitlements/"+entitlement, nil, nil)
}
