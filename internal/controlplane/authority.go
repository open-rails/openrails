package controlplane

import (
	"context"
	"errors"
	"strings"

	authcore "github.com/open-rails/authkit/embedded"

	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrNoControlPlane is returned by authority checks invoked on a nil control
// plane (an embedded host that never attached one), so callers fail closed
// explicitly rather than silently allowing or denying.
var ErrNoControlPlane = errors.New("controlplane: not configured")

// HasAdminPermission reports whether the given user holds perm in the named
// MERCHANT permission-group according to LIVE AuthKit group state (not stale JWT
// claims). Under the permission-group model (#567) merchant-admin authority is a
// role on the merchant group (`type=merchant`, `resourceRef=merchantRef`); the
// merchant `owner` auto-holds `merchant:*`.
//
// merchantRef is the merchant slug (the group's resource ref). An empty ref
// yields no authority (there is no operator fallback): the caller must present a
// merchant context.
func (c *ControlPlane) HasAdminPermission(ctx context.Context, merchantRef, userID, perm string) (bool, error) {
	if c == nil || c.Core() == nil {
		return false, ErrNoControlPlane
	}
	ref := strings.ToLower(strings.TrimSpace(merchantRef))
	if ref == "" {
		return false, nil
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false, nil
	}
	return c.Core().Can(ctx, userID, authcore.SubjectKindUser, MerchantType, ref, strings.TrimSpace(perm))
}

// ErrMerchantAmbiguous is returned by MerchantForUser when a user is a member of
// more than one merchant group: the active merchant cannot be inferred from
// membership alone and the caller must present an explicit merchant selector.
var ErrMerchantAmbiguous = errors.New("controlplane: user belongs to multiple merchants")

// MerchantForUser resolves the merchant a user session is acting on from the
// user's LIVE merchant-group membership (#567: a user access token carries no
// org/merchant claim, so /v1/merchant routes resolve the merchant from the
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

// ResolveMerchantForOrg resolves a merchant by its reference (the merchant slug,
// the merchant group's resource ref). Route auth uses it after a live merchant
// permission check so user-session merchant routes pin the same merchant context
// as API-key and delegated JWT principals.
//
// The parameter is named orgSlug for source compatibility with the route seam,
// but under #567 it is the merchant slug — there is no org persona.
func (c *ControlPlane) ResolveMerchantForOrg(ctx context.Context, orgSlug string) (merchant.ID, string, error) {
	if c == nil || c.Core() == nil {
		return merchant.ID{}, "", ErrNoControlPlane
	}
	ref := strings.ToLower(strings.TrimSpace(orgSlug))
	if ref == "" {
		return merchant.ID{}, "", ErrServiceCredentialMerchantUnresolved
	}
	mid, mslug, err := c.MerchantScope(ctx, ref)
	if err != nil {
		return merchant.ID{}, "", err
	}
	return mid, mslug, nil
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
