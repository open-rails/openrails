package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"
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

	if _, err := core.EnsureRootGroup(ctx); err != nil {
		return "", fmt.Errorf("controlplane: ensure root group: %w", err)
	}
	if err := core.SeedPermissionGroupContainment(ctx); err != nil {
		return "", fmt.Errorf("controlplane: seed permission-group containment: %w", err)
	}

	groupID, err := core.ResolveGroupIDForSlug(ctx, CustomerType, customerID)
	if errors.Is(err, authkit.ErrGroupNotFound) {
		if ownerSubject == "" {
			return "", errors.New("controlplane: customer owner subject required")
		}
		groupID, err = core.CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{
			Persona:        CustomerType,
			InstanceSlug:   customerID,
			ParentPersona:  authcore.RootPersona,
			OwnerSubjectID: ownerSubject,
		})
		if err != nil {
			// ponytail: handles the only expected race, concurrent first writers.
			if id, rerr := core.ResolveGroupIDForSlug(ctx, CustomerType, customerID); rerr == nil {
				groupID = id
			} else {
				return "", fmt.Errorf("controlplane: create customer group %q: %w", customerID, err)
			}
		}
	} else if err != nil {
		return "", fmt.Errorf("controlplane: resolve customer group %q: %w", customerID, err)
	} else if ownerSubject != "" {
		if err := core.Genesis().AssignGroupRole(ctx, CustomerType, customerID, ownerSubject, authcore.SubjectKindUser, CustomerRoleOwner); err != nil {
			return "", fmt.Errorf("controlplane: assign customer owner: %w", err)
		}
	}
	return groupID, nil
}

// AuthorizePermissionGroupRoute backs AuthKit's generated group-management
// surface. Customer groups are self-owned and lazy: the first authenticated user
// request against /customer/{their-user-id}/... materializes the group, then the
// normal AuthKit Can check decides the route.
func (c *ControlPlane) AuthorizePermissionGroupRoute(ctx context.Context, subjectID, persona, instanceSlug, perm string) (bool, error) {
	core := c.Core()
	if core == nil {
		return false, errors.New("controlplane: core service unavailable")
	}
	subjectID = strings.TrimSpace(subjectID)
	persona = strings.TrimSpace(persona)
	instanceSlug = strings.TrimSpace(instanceSlug)
	perm = strings.TrimSpace(perm)
	if subjectID == "" || persona == "" || instanceSlug == "" || perm == "" {
		return false, nil
	}
	if persona == CustomerType && subjectID == instanceSlug {
		if _, err := c.EnsureCustomerPermissionGroup(ctx, instanceSlug, subjectID); err != nil {
			return false, err
		}
	}
	return core.Can(ctx, subjectID, authcore.SubjectKindUser, persona, instanceSlug, perm)
}
