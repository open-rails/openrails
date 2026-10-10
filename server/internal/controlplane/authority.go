package controlplane

import (
	"context"
	"errors"
	"strings"

	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/helpers/auth"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchants"
)

// ErrNoControlPlane is returned by authority checks invoked on a nil control
// plane (an embedded host that never attached one), so callers fail closed
// explicitly rather than silently allowing or denying.
var ErrNoControlPlane = errors.New("controlplane: not configured")

// MerchantScope is where mid's staff and credentials hold their permissions:
// the AuthKit group of a live merchant, whose id is the merchant's.
// billing.ErrMerchantUnresolved for any other merchant.
func (c *ControlPlane) MerchantScope(ctx context.Context, mid billing.MerchantID) (auth.Scope, error) {
	if c == nil || c.Core() == nil {
		return auth.Scope{}, ErrNoControlPlane
	}
	if _, err := c.liveMerchant(ctx, mid); err != nil {
		return auth.Scope{}, err
	}
	scope, err := c.client.Scope(ctx, iam.GroupByID(mid.String()))
	if errors.Is(err, iam.ErrGroupNotFound) {
		return auth.Scope{}, billing.ErrMerchantUnresolved
	}
	return scope, err
}

// MerchantOfScope is the live merchant a scope of this AuthKit is, the
// inverse of MerchantScope; false for any other scope.
func (c *ControlPlane) MerchantOfScope(ctx context.Context, scope auth.Scope) (*merchants.Merchant, bool, error) {
	if c == nil || c.Core() == nil || scope.Authority != c.issuer {
		return nil, false, nil
	}
	mid, err := billing.ParseMerchantID(scope.ID)
	if err != nil {
		return nil, false, nil
	}
	m, err := c.liveMerchant(ctx, mid)
	if errors.Is(err, billing.ErrMerchantUnresolved) {
		return nil, false, nil
	}
	return m, err == nil, err
}

// liveMerchant is mid when it is an active merchant.
func (c *ControlPlane) liveMerchant(ctx context.Context, mid billing.MerchantID) (*merchants.Merchant, error) {
	directory, err := c.directory()
	if err != nil {
		return nil, err
	}
	m, err := directory.Get(ctx, mid)
	switch {
	case errors.Is(err, merchants.ErrMerchantNotFound):
		return nil, billing.ErrMerchantUnresolved
	case err != nil:
		return nil, err
	case m.Status != merchants.StatusActive:
		return nil, billing.ErrMerchantUnresolved
	}
	return m, nil
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

// MerchantByName resolves a live merchant by a current or former name, with
// no authority check: billing.ErrMerchantUnresolved for none.
func (c *ControlPlane) MerchantByName(ctx context.Context, name string) (billing.MerchantID, string, error) {
	if c == nil || c.Core() == nil {
		return billing.MerchantID{}, "", ErrNoControlPlane
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return billing.MerchantID{}, "", billing.ErrMerchantUnresolved
	}
	directory, err := c.directory()
	if err != nil {
		return billing.MerchantID{}, "", err
	}
	m, err := directory.GetBySlug(ctx, name)
	switch {
	case errors.Is(err, merchants.ErrMerchantNotFound):
		return billing.MerchantID{}, "", billing.ErrMerchantUnresolved
	case err != nil:
		return billing.MerchantID{}, "", err
	case m.Status != merchants.StatusActive:
		return billing.MerchantID{}, "", billing.ErrMerchantUnresolved
	}
	return m.ID, m.Slug, nil
}

// directory is the runtime's merchants service once bound (BindMerchants),
// else a directory over the control plane's pool, which reads no
// configuration.
func (c *ControlPlane) directory() (*merchants.Service, error) {
	if c == nil || c.pool == nil {
		return nil, ErrNoControlPlane
	}
	if c.merchants != nil {
		return c.merchants, nil
	}
	return merchants.NewDirectoryService(c.pool)
}

// BindMerchants gives the control plane the runtime's merchants service: the
// merchant configuration display names are read from and written to.
func (c *ControlPlane) BindMerchants(svc *merchants.Service) {
	if c != nil {
		c.merchants = svc
	}
}
