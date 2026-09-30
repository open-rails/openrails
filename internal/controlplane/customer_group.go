package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/authkit/iam"
)

// CustomerGroup addresses a customer's own permission group (#567): it is
// keyed by the customer's id, the payer's user id.
func CustomerGroup(customerID string) iam.GroupRef {
	return iam.GroupByID(strings.TrimSpace(customerID))
}

// EnsureCustomerPermissionGroup creates the hosted customer-portal membership
// used by SaaS, owned by ownerSubject, and returns its id. It is idempotent.
// Billing records and spend policies do not call this.
func (c *ControlPlane) EnsureCustomerPermissionGroup(ctx context.Context, customerID, ownerSubject string) (string, error) {
	if c.Core() == nil {
		return "", errors.New("controlplane: core service unavailable")
	}
	customerID, ownerSubject = strings.TrimSpace(customerID), strings.TrimSpace(ownerSubject)
	if customerID == "" || ownerSubject == "" {
		return "", errors.New("controlplane: customer id and owner subject are required")
	}
	owner := iam.UserSubject(ownerSubject)
	group, err := c.client.CreateGroup(ctx, iam.NewGroup{ID: customerID, Persona: CustomerType, Owner: &owner})
	if err != nil {
		return "", fmt.Errorf("controlplane: create customer group %q: %w", customerID, err)
	}
	// An existing group is returned unchanged: the owner may be another one.
	if _, err := c.client.EnsureUserRole(ctx, CustomerGroup(group.ID), iam.UserByID(ownerSubject), CustomerOwner); err != nil {
		return "", fmt.Errorf("controlplane: assign customer owner: %w", err)
	}
	return group.ID, nil
}
