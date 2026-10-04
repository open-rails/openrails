package operator

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/merchants"
)

// ProvisionMerchantForRestoreRequest selects billing identity from an archive
// and authority from the destination. The host supplies the authenticated local
// owner; group identity and permissions must never come from the archive.
type ProvisionMerchantForRestoreRequest struct {
	MerchantID billing.MerchantID
	// Slug is the name the restored merchant claims at the destination.
	Slug            string
	ExistingGroupID string
	OwnerUserID     string
}

// ProvisionMerchantForRestore preserves a merchant UUID under an existing
// destination merchant group whose live owner is req.OwnerUserID, claiming
// req.Slug. It creates no groups, roles, credentials or host routes. The
// importer separately requires an empty billing book.
func ProvisionMerchantForRestore(ctx context.Context, a *app.App, req ProvisionMerchantForRestoreRequest) (*billing.ProvisionMerchantResult, error) {
	if req.MerchantID.IsZero() {
		return nil, fmt.Errorf("control plane restore provision: merchant_id is required")
	}
	cp := Get(a)
	if cp == nil || cp.Core() == nil {
		return nil, fmt.Errorf("control plane restore provision: no control plane attached (call Attach first)")
	}
	groupID, owner := strings.TrimSpace(req.ExistingGroupID), strings.TrimSpace(req.OwnerUserID)
	if groupID == "" || owner == "" {
		return nil, iam.ErrInsufficientAuthority
	}
	core := cp.Core()
	group, err := core.Group(ctx, iam.GroupByID(groupID))
	if err != nil {
		return nil, err
	}
	if group.Persona != controlplane.MerchantType || group.DeletedAt != nil {
		return nil, iam.ErrInsufficientAuthority
	}
	roles, err := core.GroupRoles(ctx, iam.GroupByID(group.ID), []iam.Subject{iam.UserSubject(owner)})
	if err != nil {
		return nil, err
	}
	if roles[iam.UserSubject(owner)] != controlplane.MerchantOwner {
		return nil, iam.ErrInsufficientAuthority
	}
	if u, err := core.User(ctx, iam.UserByID(owner)); errors.Is(err, iam.ErrUserNotFound) || err == nil && u.Ban != nil {
		return nil, iam.ErrInsufficientAuthority
	} else if err != nil {
		return nil, err
	}
	directory, err := merchants.NewDirectoryService(cp.Pool())
	if err != nil {
		return nil, err
	}
	m, created, err := directory.ProvisionForRestore(ctx, req.MerchantID, merchants.ProvisionRequest{
		Slug: req.Slug, PermissionGroupID: group.ID,
	})
	if err != nil {
		return nil, err
	}
	return &billing.ProvisionMerchantResult{MerchantID: m.ID, GroupID: group.ID, Created: created}, nil
}
