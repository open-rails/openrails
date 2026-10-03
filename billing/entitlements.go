package billing

import (
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
