package controlplane

import (
	"context"
	"errors"
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

// ResolveAuthorizedMerchant captures one group UUID from the explicit name or,
// when empty, the user's sole merchant membership. Live authorization and billing
// selection use that same UUID; inference never resolves a mutable name again.
func (c *ControlPlane) ResolveAuthorizedMerchant(ctx context.Context, merchantRef, userID, perm string) (merchant.ID, string, error) {
	if c == nil || c.Core() == nil {
		return merchant.ID{}, "", ErrNoControlPlane
	}
	if strings.TrimSpace(userID) == "" {
		return merchant.ID{}, "", policy.ErrPermissionRequired
	}
	ref := strings.ToLower(strings.TrimSpace(merchantRef))
	var group authkit.GroupInstance
	var err error
	if ref == "" {
		group, err = c.merchantGroupForUser(ctx, strings.TrimSpace(userID))
	} else {
		group, err = c.Core().GroupInstanceForSlug(ctx, MerchantGroup(ref))
	}
	if errors.Is(err, authkit.ErrGroupNotFound) {
		return merchant.ID{}, "", policy.ErrMerchantUnresolved
	}
	if err != nil {
		return merchant.ID{}, "", err
	}
	allowed, err := c.Core().CanOnGroup(ctx, authkit.UserSubject(strings.TrimSpace(userID)), group.ID, authkit.Perm(strings.TrimSpace(perm)))
	if err != nil {
		return merchant.ID{}, "", err
	}
	if !allowed {
		return merchant.ID{}, "", policy.ErrPermissionRequired
	}
	mid, _, err := c.merchantForGroupID(ctx, group.ID)
	if errors.Is(err, ErrServiceCredentialMerchantUnresolved) {
		return merchant.ID{}, "", policy.ErrMerchantUnresolved
	}
	if err != nil {
		return merchant.ID{}, "", err
	}
	return mid, group.InstanceSlug, nil
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
var ErrMerchantAmbiguous = errors.New("controlplane: user belongs to multiple merchants")

func (c *ControlPlane) merchantGroupForUser(ctx context.Context, userID string) (authkit.GroupInstance, error) {
	memberships, err := c.Core().ListSubjectGroups(ctx, authkit.UserSubject(userID))
	if err != nil {
		return authkit.GroupInstance{}, err
	}
	var groupID string
	for _, membership := range memberships {
		if membership.Persona != MerchantType {
			continue
		}
		if groupID != "" && groupID != membership.GroupID {
			return authkit.GroupInstance{}, ErrMerchantAmbiguous
		}
		groupID = membership.GroupID
	}
	if groupID == "" {
		return authkit.GroupInstance{}, policy.ErrMerchantUnresolved
	}
	return c.Core().GroupInstanceByID(ctx, groupID)
}

// ResolveMerchantForGroup resolves a merchant by its reference (the merchant slug,
// the merchant group's resource ref). Route auth uses it after a live merchant
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

// MerchantGroupSlugResolver returns the or#914 rename-forwarding seam for the
// merchants directory service: slug -> the bound merchant group's id and
// CURRENT slug, following only aliases still reserved by AuthKit's naming policy. Wire it with merchants.Service.WithGroupSlugResolver
// wherever both the control plane and a directory service exist.
func (c *ControlPlane) MerchantGroupSlugResolver() merchants.GroupSlugResolver {
	return func(ctx context.Context, slug string) (string, string, error) {
		core := c.Core()
		if core == nil {
			return "", "", ErrNoControlPlane
		}
		gi, err := core.GroupInstanceForSlug(ctx, MerchantGroup(strings.ToLower(strings.TrimSpace(slug))))
		if errors.Is(err, authkit.ErrGroupNotFound) {
			return "", "", merchants.ErrMerchantNotFound
		}
		if err != nil {
			return "", "", err
		}
		return gi.ID, gi.InstanceSlug, nil
	}
}

// IsAdmin reports whether the user holds any merchant-staff grant in the named
// merchant group via live AuthKit state (a proxy: it tests the broadest
// merchant read perm the owner/viewer/support all hold).
func (c *ControlPlane) IsAdmin(ctx context.Context, merchantRef, userID string) (bool, error) {
	if c == nil || c.Core() == nil {
		return false, ErrNoControlPlane
	}
	ref := strings.ToLower(strings.TrimSpace(merchantRef))
	if ref == "" {
		return false, nil
	}
	return c.Core().Can(ctx, authkit.UserSubject(strings.TrimSpace(userID)), MerchantGroup(ref), PermMerchantSettingsRead)
}

func (c *ControlPlane) MerchantGroupIDResolver() merchants.GroupIDResolver {
	return func(ctx context.Context, groupID string) (string, error) {
		if c == nil || c.Core() == nil {
			return "", ErrNoControlPlane
		}
		group, err := c.Core().GroupInstanceByID(ctx, groupID)
		if errors.Is(err, authkit.ErrGroupNotFound) {
			return "", merchants.ErrMerchantNotFound
		}
		if err != nil {
			return "", err
		}
		if group.Persona != MerchantType {
			return "", merchants.ErrMerchantNotFound
		}
		return group.InstanceSlug, nil
	}
}
