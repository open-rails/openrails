package operator

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/authkit"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ProvisionMerchantForRestoreRequest selects billing identity from an archive
// and authority from the destination. The host supplies the authenticated local
// owner; group identity and permissions must never come from the archive.
type ProvisionMerchantForRestoreRequest struct {
	MerchantID      merchant.ID
	ExistingGroupID string
	OwnerUserID     string
}

// ProvisionMerchantForRestore preserves a merchant UUID under an existing
// destination merchant group. It checks live owner authority and uses that
// group's canonical name. It creates no groups, roles, credentials or host
// routes. The importer separately requires an empty billing book.
func ProvisionMerchantForRestore(ctx context.Context, a *app.App, req ProvisionMerchantForRestoreRequest) (*ProvisionMerchantResult, error) {
	if req.MerchantID.IsZero() {
		return nil, fmt.Errorf("control plane restore provision: merchant_id is required")
	}
	cp := Get(a)
	if cp == nil || cp.Core() == nil {
		return nil, fmt.Errorf("control plane restore provision: no control plane attached (call Attach first)")
	}
	groupID, owner := strings.TrimSpace(req.ExistingGroupID), strings.TrimSpace(req.OwnerUserID)
	if groupID == "" || owner == "" {
		return nil, authkit.ErrInsufficientRoleAuthority
	}
	core := cp.Core()
	group, err := core.GroupInstanceByID(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if group.Persona != controlplane.MerchantType {
		return nil, authkit.ErrInsufficientRoleAuthority
	}
	allowed, err := core.CanOnGroup(ctx, authkit.UserSubject(owner), groupID, controlplane.MerchantType.OwnerGrant())
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, authkit.ErrInsufficientRoleAuthority
	}
	directory, err := merchants.NewDirectoryService(cp.Pool())
	if err != nil {
		return nil, err
	}
	m, created, err := directory.ProvisionForRestore(ctx, req.MerchantID, merchants.ProvisionRequest{
		Slug: group.InstanceSlug, PermissionGroupID: group.ID,
	})
	if err != nil {
		return nil, err
	}
	return &ProvisionMerchantResult{MerchantID: m.ID, GroupID: group.ID, Created: created}, nil
}
