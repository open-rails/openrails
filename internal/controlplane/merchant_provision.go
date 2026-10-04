package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// createMerchantGroup creates merchant mid's AuthKit group, keyed by mid and
// owned by ownerUserID when set, inside tx. It is idempotent: the group of a
// merchant that already has one is returned unchanged.
func (c *ControlPlane) createMerchantGroup(ctx context.Context, tx pgx.Tx, mid merchant.ID, ownerUserID string) (iam.GroupRef, error) {
	g := iam.NewGroup{ID: mid.String(), Persona: MerchantType}
	if owner := strings.TrimSpace(ownerUserID); owner != "" {
		s := iam.UserSubject(owner)
		g.Owner = &s
	}
	group, err := c.client.CreateGroup(ctx, g, authkit.InTx(tx))
	if err != nil {
		return iam.GroupRef{}, fmt.Errorf("controlplane: create merchant group for %s: %w", mid, err)
	}
	return iam.GroupByID(group.ID), nil
}

// CreateMerchant claims name for a new merchant bound to a new AuthKit group,
// owned by ownerUserID when set. The group, prepare's AuthKit writes (done
// with authkit.InTx(tx)) and the merchant row commit together: a name that
// cannot be claimed (ErrMerchantNameTaken) leaves nothing behind.
func (c *ControlPlane) CreateMerchant(ctx context.Context, name, ownerUserID string, prepare func(context.Context, pgx.Tx, iam.GroupRef) error) (*merchants.Merchant, error) {
	if c.Core() == nil {
		return nil, ErrNoControlPlane
	}
	directory, err := c.directory()
	if err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	mid := merchant.ID(id)
	return directory.Provision(ctx, merchants.ProvisionRequest{ID: mid, Slug: name, PermissionGroupID: mid.String()}, func(ctx context.Context, tx pgx.Tx) error {
		group, err := c.createMerchantGroup(ctx, tx, mid, ownerUserID)
		if err != nil || prepare == nil {
			return err
		}
		return prepare(ctx, tx, group)
	})
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
	if errors.Is(err, billing.ErrMerchantNameTaken) {
		// A concurrent claim won the name; report it as existing.
		m, err = directory.GetBySlug(ctx, name)
		return m, false, err
	}
	return m, err == nil, err
}

// CreateOwnedMerchant creates a merchant claiming name and owned by userID,
// under the declared creation policy. For a merchant the user already owns it
// returns that merchant (created=false), whatever its name is now; a name
// held by any other merchant is ErrMerchantNameTaken.
func (c *ControlPlane) CreateOwnedMerchant(ctx context.Context, name, userID string) (*merchants.Merchant, bool, error) {
	userID = strings.TrimSpace(userID)
	if !c.MerchantCreationEnabled() || c.Core() == nil {
		return nil, false, ErrNoControlPlane
	}
	if userID == "" {
		return nil, false, iam.ErrInsufficientAuthority
	}
	name = merchant.NormalizeSlug(name)
	if err := merchant.ValidateSlug(name); err != nil {
		return nil, false, fmt.Errorf("%w: %w", merchants.ErrInvalidName, err)
	}
	if err := c.EnforceMerchantCreationPolicy(ctx, name, userID); err != nil {
		return nil, false, err
	}
	m, created, err := c.ProvisionMerchant(ctx, name, userID)
	if err != nil || created {
		return m, created, err
	}
	if m.PermissionGroupID != "" {
		roles, err := c.client.GroupRoles(ctx, iam.GroupByID(m.PermissionGroupID), []iam.Subject{iam.UserSubject(userID)})
		if err != nil || roles[iam.UserSubject(userID)] == MerchantOwner {
			return m, false, err
		}
	}
	return nil, false, fmt.Errorf("%w: %q", billing.ErrMerchantNameTaken, name)
}
