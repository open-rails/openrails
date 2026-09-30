package controlplane

import (
	"context"
	"errors"
	"github.com/open-rails/openrails/internal/credential"
	"strings"

	"github.com/open-rails/authkit"

	"github.com/open-rails/openrails/internal/auth/policy"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrNoControlPlane is returned by authority checks invoked on a nil control
// plane (an embedded host that never attached one), so callers fail closed
// explicitly rather than silently allowing or denying.
var ErrNoControlPlane = errors.New("controlplane: not configured")

// ResolveAuthorizedMerchant resolves merchantRef (a current or former name)
// or, when empty, the user's sole merchant membership to one bound merchant,
// then checks perm live on that merchant's group.
func (c *ControlPlane) ResolveAuthorizedMerchant(ctx context.Context, merchantRef, userID, perm string) (merchant.ID, string, error) {
	if c == nil || c.Core() == nil {
		return merchant.ID{}, "", ErrNoControlPlane
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return merchant.ID{}, "", policy.ErrPermissionRequired
	}
	var groupID string
	var err error
	if ref := strings.TrimSpace(merchantRef); ref == "" {
		groupID, err = c.merchantGroupForUser(ctx, userID)
	} else {
		groupID, err = c.merchantGroupByName(ctx, ref)
	}
	if err != nil {
		return merchant.ID{}, "", err
	}
	allowed, err := c.Core().CanOnGroup(ctx, authkit.UserSubject(userID), groupID, authkit.Perm(strings.TrimSpace(perm)))
	if err != nil {
		return merchant.ID{}, "", err
	}
	if !allowed {
		return merchant.ID{}, "", policy.ErrPermissionRequired
	}
	mid, slug, err := c.merchantForGroupID(ctx, groupID)
	if errors.Is(err, ErrServiceCredentialMerchantUnresolved) {
		return merchant.ID{}, "", policy.ErrMerchantUnresolved
	}
	return mid, slug, err
}

// merchantGroupByName resolves a current or former merchant name to the group
// of a live, bound merchant.
func (c *ControlPlane) merchantGroupByName(ctx context.Context, name string) (string, error) {
	directory, err := c.directory()
	if err != nil {
		return "", err
	}
	m, err := directory.GetBySlug(ctx, name)
	if errors.Is(err, merchants.ErrMerchantNotFound) {
		return "", policy.ErrMerchantUnresolved
	}
	if err != nil {
		return "", err
	}
	if m.PermissionGroupID == "" || m.Status != merchants.StatusActive {
		return "", policy.ErrMerchantUnresolved
	}
	return m.PermissionGroupID, nil
}

// HasRootPermission reports whether the user holds perm in the singleton ROOT
// permission-group (#721): live AuthKit state, no merchant context. The root
// `owner` auto-holds root:*; bounded operator roles (merchant-directory-*)
// carry concrete root:merchants:* grants. Gates the /v1/platform/* tier.
func (c *ControlPlane) HasRootPermission(ctx context.Context, userID, perm string) (bool, error) {
	if c == nil || c.Core() == nil {
		return false, ErrNoControlPlane
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false, nil
	}
	// The root group is the singleton parentless group: persona=root, no slug.
	return c.Core().Can(ctx, authkit.UserSubject(userID), authkit.RootGroup(), authkit.Perm(strings.TrimSpace(perm)))
}

// ErrMerchantAmbiguous requires an explicit selector when several distinct
// merchant groups are present in the user's live memberships.
var ErrMerchantAmbiguous = credential.ErrMerchantAmbiguous

func (c *ControlPlane) merchantGroupForUser(ctx context.Context, userID string) (string, error) {
	memberships, err := c.Core().ListSubjectGroups(ctx, authkit.UserSubject(userID))
	if err != nil {
		return "", err
	}
	var groupID string
	for _, membership := range memberships {
		if membership.Persona != MerchantType {
			continue
		}
		if groupID != "" && groupID != membership.GroupID {
			return "", ErrMerchantAmbiguous
		}
		groupID = membership.GroupID
	}
	if groupID == "" {
		return "", policy.ErrMerchantUnresolved
	}
	return groupID, nil
}

// ResolveMerchantForGroup resolves a bound merchant by a current or former
// name. Route auth uses it after a live merchant
// permission check so user-session merchant routes pin the same merchant context
// as API-key and delegated JWT principals.
func (c *ControlPlane) ResolveMerchantForGroup(ctx context.Context, merchantRef string) (merchant.ID, string, error) {
	if c == nil || c.Core() == nil {
		return merchant.ID{}, "", ErrNoControlPlane
	}
	ref := strings.ToLower(strings.TrimSpace(merchantRef))
	if ref == "" {
		return merchant.ID{}, "", policy.ErrMerchantUnresolved
	}
	mid, mslug, err := c.MerchantScope(ctx, ref)
	if errors.Is(err, ErrServiceCredentialMerchantUnresolved) {
		return merchant.ID{}, "", policy.ErrMerchantUnresolved
	}
	if err != nil {
		return merchant.ID{}, "", err
	}
	return mid, mslug, nil
}

// IsAdmin reports whether the user holds any merchant-staff grant in the named
// merchant via live AuthKit state (a proxy: it tests the broadest merchant read
// perm the owner/viewer/support all hold).
func (c *ControlPlane) IsAdmin(ctx context.Context, merchantRef, userID string) (bool, error) {
	if c == nil || c.Core() == nil {
		return false, ErrNoControlPlane
	}
	if strings.TrimSpace(merchantRef) == "" {
		return false, nil
	}
	groupID, err := c.merchantGroupByName(ctx, merchantRef)
	if errors.Is(err, policy.ErrMerchantUnresolved) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return c.Core().CanOnGroup(ctx, authkit.UserSubject(strings.TrimSpace(userID)), groupID, PermMerchantSettingsRead)
}
