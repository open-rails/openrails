package openrails

import (
	"context"
	"net/http"
	"net/url"
	"strings"
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
	var out EntitlementRecord
	path := "/v1/merchant/customers/" + url.PathEscape(strings.TrimSpace(customerID)) + "/entitlements"
	if err := c.do(ctx, http.MethodPost, path, request, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RevokeEntitlement revokes one entitlement window owned by the customer.
func (c *Client) RevokeEntitlement(ctx context.Context, customerID, entitlementID string) error {
	path := "/v1/merchant/customers/" + url.PathEscape(strings.TrimSpace(customerID)) + "/entitlements/" + url.PathEscape(strings.TrimSpace(entitlementID))
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}
