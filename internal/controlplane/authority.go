package controlplane

import (
	"context"
	"errors"
	"strings"

	"github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"

	"github.com/open-rails/openrails/internal/auth/policy"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrNoControlPlane is returned by authority checks invoked on a nil control
// plane (an embedded host that never attached one), so callers fail closed
// explicitly rather than silently allowing or denying.
var ErrNoControlPlane = errors.New("controlplane: not configured")

// ResolveAuthorizedMerchant resolves a name exactly once, checks live authority
// on that group UUID and selects the billing row bound to that same UUID.
func (c *ControlPlane) ResolveAuthorizedMerchant(ctx context.Context, merchantRef, userID, perm string) (merchant.ID, string, error) {
	if c == nil || c.Core() == nil {
		return merchant.ID{}, "", ErrNoControlPlane
	}
	if strings.TrimSpace(merchantRef) == "" || strings.TrimSpace(userID) == "" {
		return merchant.ID{}, "", policy.ErrPermissionRequired
	}
	group, err := c.Core().GroupInstanceForSlug(ctx, MerchantType, strings.ToLower(strings.TrimSpace(merchantRef)))
	if errors.Is(err, authkit.ErrGroupNotFound) {
		return merchant.ID{}, "", policy.ErrMerchantUnresolved
	}
	if err != nil {
		return merchant.ID{}, "", err
	}
	allowed, err := c.Core().CanOnGroup(ctx, strings.TrimSpace(userID), authcore.SubjectKindUser, group.ID, strings.TrimSpace(perm))
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
	return c.Core().Can(ctx, userID, authcore.SubjectKindUser, authcore.RootPersona, "", strings.TrimSpace(perm))
}

// ErrMerchantAmbiguous is returned by MerchantForUser when a user is a member of
// more than one merchant group: the active merchant cannot be inferred from
// membership alone and the caller must present an explicit merchant selector.
var ErrMerchantAmbiguous = errors.New("controlplane: user belongs to multiple merchants")

// MerchantForUser resolves the merchant a user session is acting on from the
// user's LIVE merchant-group membership (#567: a user access token carries no
// merchant claim, so /v1/merchant routes resolve the merchant from the
// permission-group the user belongs to). Returns the merchant slug (the group's
// resource ref) when the user is a member of exactly one merchant group, "" when
// the user belongs to none, or ErrMerchantAmbiguous when more than one.
func (c *ControlPlane) MerchantForUser(ctx context.Context, userID string) (string, error) {
	if c == nil || c.Core() == nil {
		return "", ErrNoControlPlane
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return "", nil
	}
	memberships, err := c.Core().ListSubjectGroups(ctx, userID, authcore.SubjectKindUser)
	if err != nil {
		return "", err
	}
	var ref string
	for _, m := range memberships {
		if m.Persona != MerchantType || strings.TrimSpace(m.InstanceSlug) == "" {
			continue
		}
		if ref != "" && !strings.EqualFold(ref, m.InstanceSlug) {
			return "", ErrMerchantAmbiguous
		}
		ref = m.InstanceSlug
	}
	return ref, nil
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
		return merchant.ID{}, "", ErrServiceCredentialMerchantUnresolved
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
// CURRENT slug, following ak#264 tombstones so renamed-away slugs resolve to
// the same group forever. Wire it with merchants.Service.WithGroupSlugResolver
// wherever both the control plane and a directory service exist.
func (c *ControlPlane) MerchantGroupSlugResolver() merchants.GroupSlugResolver {
	return func(ctx context.Context, slug string) (string, string, error) {
		core := c.Core()
		if core == nil {
			return "", "", ErrNoControlPlane
		}
		gi, err := core.GroupInstanceForSlug(ctx, MerchantType, strings.ToLower(strings.TrimSpace(slug)))
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
	return c.Core().Can(ctx, strings.TrimSpace(userID), authcore.SubjectKindUser, MerchantType, ref, PermMerchantSettingsRead)
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
