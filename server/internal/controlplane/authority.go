package controlplane

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/open-rails/authkit/iam"
	helpersauth "github.com/open-rails/helpers/auth"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/credential"
	"github.com/open-rails/openrails/internal/merchants"
)

// ErrNoControlPlane is returned by authority checks invoked on a nil control
// plane (an embedded host that never attached one), so callers fail closed
// explicitly rather than silently allowing or denying.
var ErrNoControlPlane = errors.New("controlplane: not configured")

// ErrMerchantAmbiguous requires an explicit selector when several distinct
// merchant groups are present in the user's live memberships.
var ErrMerchantAmbiguous = credential.ErrMerchantAmbiguous

// RequestIdentity is the identity r's control-plane user token acts as:
// bound to its session, so every permission check refuses it once that
// sign-in is revoked. billingauth.ErrUnauthenticated
// when r carries no user token.
func (c *ControlPlane) RequestIdentity(r *http.Request) (helpersauth.Identity, error) {
	if c == nil || c.users == nil {
		return helpersauth.Identity{}, ErrNoControlPlane
	}
	return c.users.Identity(r)
}

// ResolveAuthorizedMerchant resolves merchantRef (a current or former name)
// or, when empty, the requesting user's sole merchant to one bound merchant,
// then checks perm live on that merchant's group for the request's actor. A
// revoked session is an error joined with helpers/auth ErrRevoked.
func (c *ControlPlane) ResolveAuthorizedMerchant(ctx context.Context, r *http.Request, merchantRef, perm string) (billing.MerchantID, string, error) {
	if c == nil || c.Core() == nil {
		return billing.MerchantID{}, "", ErrNoControlPlane
	}
	who, err := c.RequestIdentity(r)
	if err != nil {
		return billing.MerchantID{}, "", err
	}
	var groupID string
	if ref := strings.TrimSpace(merchantRef); ref == "" {
		groupID, err = c.merchantGroupForUser(ctx, who.Subject)
	} else {
		groupID, err = c.merchantGroupByName(ctx, ref)
	}
	if err != nil {
		return billing.MerchantID{}, "", err
	}
	allowed, err := c.can(ctx, who, iam.GroupByID(groupID), perm)
	if err != nil {
		return billing.MerchantID{}, "", err
	}
	if !allowed {
		return billing.MerchantID{}, "", billing.ErrPermissionRequired
	}
	mid, slug, err := c.merchantForGroupID(ctx, groupID)
	if errors.Is(err, ErrServiceCredentialMerchantUnresolved) {
		return billing.MerchantID{}, "", billing.ErrMerchantUnresolved
	}
	return mid, slug, err
}

// UserMerchant is the live merchant a user acts on in the admin API: the
// one ref names (a current or former name), else the only merchant group
// they hold a role in. It checks no permission: the staff gate asks the
// session's Can in that merchant's group.
func (c *ControlPlane) UserMerchant(ctx context.Context, userID, ref string) (billing.MerchantID, string, error) {
	if c == nil || c.Core() == nil {
		return billing.MerchantID{}, "", ErrNoControlPlane
	}
	var groupID string
	var err error
	if ref = strings.TrimSpace(ref); ref == "" {
		groupID, err = c.merchantGroupForUser(ctx, userID)
	} else {
		groupID, err = c.merchantGroupByName(ctx, ref)
	}
	if err != nil {
		return billing.MerchantID{}, "", err
	}
	mid, slug, err := c.merchantForGroupID(ctx, groupID)
	if errors.Is(err, ErrServiceCredentialMerchantUnresolved) {
		return billing.MerchantID{}, "", billing.ErrMerchantUnresolved
	}
	return mid, slug, err
}

// MerchantGroup is the permission group bound to mid, a live merchant: where
// its staff hold their permissions.
func (c *ControlPlane) MerchantGroup(ctx context.Context, mid billing.MerchantID) (string, error) {
	directory, err := c.directory()
	if err != nil {
		return "", err
	}
	m, err := directory.Get(ctx, mid)
	switch {
	case errors.Is(err, merchants.ErrMerchantNotFound):
		return "", billing.ErrMerchantUnresolved
	case err != nil:
		return "", err
	case m.PermissionGroupID == "" || m.Status != merchants.StatusActive:
		return "", billing.ErrMerchantUnresolved
	}
	return m.PermissionGroupID, nil
}

// CheckRecentSignIn is AuthKit's Sensitive check for r's user token, with
// helpers/auth RecentSignInChecker's errors.
func (c *ControlPlane) CheckRecentSignIn(ctx context.Context, r *http.Request) error {
	if c == nil || c.users == nil {
		return ErrNoControlPlane
	}
	return c.users.CheckRecentSignIn(ctx, r)
}

// HasRootPermission reports whether the request's user holds perm in the root
// group, checked live with its session. The root owner holds root:*; the
// bounded operator roles hold root:merchants:*.
func (c *ControlPlane) HasRootPermission(ctx context.Context, r *http.Request, perm string) (bool, error) {
	if c == nil || c.Core() == nil {
		return false, ErrNoControlPlane
	}
	who, err := c.RequestIdentity(r)
	if err != nil {
		return false, err
	}
	return c.can(ctx, who, iam.RootGroup(), perm)
}

// can checks perm live for who in ref. A revoked session is joined with
// helpers/auth ErrRevoked, a credential failure rather than an outage.
func (c *ControlPlane) can(ctx context.Context, who helpersauth.Identity, ref iam.GroupRef, perm string) (bool, error) {
	p, err := c.client.Permission(strings.TrimSpace(perm))
	if err != nil {
		return false, err
	}
	allowed, err := c.client.Can(ctx, who, ref, p)
	if errors.Is(err, iam.ErrSessionRevoked) {
		return false, errors.Join(err, helpersauth.ErrRevoked)
	}
	return allowed, err
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
		return "", billing.ErrMerchantUnresolved
	}
	if err != nil {
		return "", err
	}
	if m.PermissionGroupID == "" || m.Status != merchants.StatusActive {
		return "", billing.ErrMerchantUnresolved
	}
	return m.PermissionGroupID, nil
}

// merchantGroupForUser is the one merchant group userID holds a role in.
func (c *ControlPlane) merchantGroupForUser(ctx context.Context, userID string) (string, error) {
	memberships, err := c.memberships(ctx, iam.UserSubject(userID))
	if err != nil {
		return "", err
	}
	var groupID string
	for _, m := range memberships {
		if m.Group.Persona != MerchantType {
			continue
		}
		if groupID != "" && groupID != m.Group.ID {
			return "", ErrMerchantAmbiguous
		}
		groupID = m.Group.ID
	}
	if groupID == "" {
		return "", billing.ErrMerchantUnresolved
	}
	return groupID, nil
}

// OwnedMerchantGroups are the merchant groups userID holds the owner role in.
func (c *ControlPlane) OwnedMerchantGroups(ctx context.Context, userID string) ([]string, error) {
	memberships, err := c.memberships(ctx, iam.UserSubject(strings.TrimSpace(userID)))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range memberships {
		if m.Group.Persona == MerchantType && m.Role == MerchantOwner {
			out = append(out, m.Group.ID)
		}
	}
	return out, nil
}

// memberships is every live group s holds a role in.
func (c *ControlPlane) memberships(ctx context.Context, s iam.Subject) ([]iam.Membership, error) {
	var out []iam.Membership
	for m, err := range iam.All(func(p iam.PageRequest) (iam.ListPage[iam.Membership], error) {
		return c.client.ListMemberships(ctx, s, p)
	}) {
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// ResolveMerchantForGroup resolves a bound merchant by a current or former
// name, with no authority check.
func (c *ControlPlane) ResolveMerchantForGroup(ctx context.Context, merchantRef string) (billing.MerchantID, string, error) {
	if c == nil || c.Core() == nil {
		return billing.MerchantID{}, "", ErrNoControlPlane
	}
	ref := strings.ToLower(strings.TrimSpace(merchantRef))
	if ref == "" {
		return billing.MerchantID{}, "", billing.ErrMerchantUnresolved
	}
	mid, mslug, err := c.MerchantScope(ctx, ref)
	if errors.Is(err, ErrServiceCredentialMerchantUnresolved) {
		return billing.MerchantID{}, "", billing.ErrMerchantUnresolved
	}
	if err != nil {
		return billing.MerchantID{}, "", err
	}
	return mid, mslug, nil
}
