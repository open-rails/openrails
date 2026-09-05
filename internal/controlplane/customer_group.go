package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/authkit"
)

// EnsureCustomerPermissionGroup lazily materializes the AuthKit customer group
// for a payer that has started managing spend delegation or credentials.
func (c *ControlPlane) EnsureCustomerPermissionGroup(ctx context.Context, customerID, ownerSubject string) (string, error) {
	core := c.Core()
	if core == nil {
		return "", errors.New("controlplane: core service unavailable")
	}
	customerID = strings.TrimSpace(customerID)
	if customerID == "" {
		return "", errors.New("controlplane: customer id required")
	}
	ownerSubject = strings.TrimSpace(ownerSubject)

	if err := EnsureRootContainment(ctx, core); err != nil {
		return "", fmt.Errorf("controlplane: %w", err)
	}

	groupID, err := core.ResolveGroupIDForSlug(ctx, CustomerGroup(customerID))
	if errors.Is(err, authkit.ErrGroupNotFound) {
		if ownerSubject == "" {
			return "", errors.New("controlplane: customer owner subject required")
		}
		groupID, err = core.CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{
			Persona:        CustomerType,
			InstanceSlug:   customerID,
			ParentPersona:  authkit.RootPersona,
			OwnerSubjectID: ownerSubject,
		})
		if err != nil {
			// ponytail: handles the only expected race, concurrent first writers.
			if id, rerr := core.ResolveGroupIDForSlug(ctx, CustomerGroup(customerID)); rerr == nil {
				groupID = id
			} else {
				return "", fmt.Errorf("controlplane: create customer group %q: %w", customerID, err)
			}
		}
	} else if err != nil {
		return "", fmt.Errorf("controlplane: resolve customer group %q: %w", customerID, err)
	} else if ownerSubject != "" {
		if err := core.Genesis().AssignGroupRole(ctx, CustomerGroup(customerID), authkit.UserSubject(ownerSubject), CustomerRoleOwner); err != nil {
			return "", fmt.Errorf("controlplane: assign customer owner: %w", err)
		}
	}
	return groupID, nil
}
