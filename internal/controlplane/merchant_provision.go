package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ensureMerchantGroup returns merchant mid's AuthKit group, creating it owned by
// ownerUserID when absent. Its AuthKit name is the merchant id: AuthKit names
// are not merchant names (#1106), and a fixed one makes this idempotent under
// concurrent callers.
func (c *ControlPlane) ensureMerchantGroup(ctx context.Context, mid merchant.ID, ownerUserID string) (string, bool, error) {
	core := c.Core()
	if core == nil {
		return "", false, ErrNoControlPlane
	}
	ref := authkit.GroupRef{Persona: MerchantType, Instance: mid.String()}
	groupID, err := core.ResolveGroupIDForSlug(ctx, ref)
	if err == nil || !errors.Is(err, authkit.ErrGroupNotFound) {
		return groupID, false, err
	}
	groupID, err = core.CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{
		Persona: MerchantType, InstanceSlug: ref.Instance, ParentPersona: authkit.RootPersona,
		OwnerSubjectID: strings.TrimSpace(ownerUserID),
	})
	if err != nil {
		// A concurrent caller created it first.
		if existing, rerr := core.ResolveGroupIDForSlug(ctx, ref); rerr == nil {
			return existing, false, nil
		}
		return "", false, fmt.Errorf("controlplane: create merchant group for %s: %w", mid, err)
	}
	return groupID, true, nil
}

// CreateMerchant claims name for a new merchant bound to a new AuthKit group,
// owned by ownerUserID when set. prepare runs against the group before the
// name is claimed. When the name cannot be claimed (ErrMerchantNameTaken) the
// group is deleted again.
func (c *ControlPlane) CreateMerchant(ctx context.Context, name, ownerUserID string, prepare func(context.Context, string) error) (*merchants.Merchant, error) {
	directory, err := c.directory()
	if err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	mid := merchant.ID(id)
	groupID, _, err := c.ensureMerchantGroup(ctx, mid, ownerUserID)
	if err != nil {
		return nil, err
	}
	if prepare != nil {
		err = prepare(ctx, groupID)
	}
	var m *merchants.Merchant
	if err == nil {
		m, _, err = directory.Provision(ctx, merchants.ProvisionRequest{ID: mid, Slug: name, PermissionGroupID: groupID})
	}
	if err != nil {
		if derr := c.Core().DeleteGroupInstanceByID(context.WithoutCancel(ctx), groupID, authkit.DeletePermissionGroupOptions{}); derr != nil {
			log.WithError(derr).WithField("group_id", groupID).Warn("controlplane: delete unclaimed merchant group failed")
		}
		return nil, err
	}
	return m, nil
}

// ProvisionMerchant returns the live merchant a current or former name
// resolves to, or creates one bound to a new AuthKit group owned by
// ownerUserID. created reports whether this call claimed the name.
func (c *ControlPlane) ProvisionMerchant(ctx context.Context, name, ownerUserID string) (*merchants.Merchant, bool, error) {
	directory, err := c.directory()
	if err != nil {
		return nil, false, err
	}
	m, err := directory.GetBySlug(ctx, name)
	if !errors.Is(err, merchants.ErrMerchantNotFound) {
		return m, false, err
	}
	m, err = c.CreateMerchant(ctx, name, ownerUserID, nil)
	if errors.Is(err, merchants.ErrMerchantNameTaken) {
		// A concurrent claim won the name; report it as existing.
		m, err = directory.GetBySlug(ctx, name)
		return m, false, err
	}
	return m, err == nil, err
}
