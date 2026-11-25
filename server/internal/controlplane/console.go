package controlplane

import (
	"cmp"
	"context"
	"errors"
	"slices"

	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/server/internal/adminconsole"
)

// RootConsoleSources are root's enabled trusted applications, declared in
// the bootstrap manifest: the deployment's own, which the console's sign-in
// page may show to anyone.
func (c *ControlPlane) RootConsoleSources(ctx context.Context) ([]adminconsole.Source, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoControlPlane
	}
	return c.groupSources(ctx, iam.RootGroup(), "root", "")
}

// MerchantConsoleSources are the enabled trusted applications of the live
// merchant named name (its current or a former name); none for an unknown
// or inactive one, alike.
func (c *ControlPlane) MerchantConsoleSources(ctx context.Context, name string) ([]adminconsole.Source, error) {
	dir, err := c.directory()
	if err != nil {
		return nil, err
	}
	m, err := dir.GetBySlug(ctx, name)
	switch {
	case errors.Is(err, billing.ErrMerchantNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	case m.Status != merchants.StatusActive:
		return nil, nil
	}
	return c.groupSources(ctx, iam.GroupByID(m.ID.String()), m.ID.String(), cmp.Or(dir.DisplayName(ctx, m.ID), m.Slug))
}

func (c *ControlPlane) groupSources(ctx context.Context, group iam.GroupRef, key, name string) ([]adminconsole.Source, error) {
	var out []adminconsole.Source
	for app, err := range iam.All(func(p iam.PageRequest) (iam.ListPage[iam.RemoteApplication], error) {
		return c.client.ListRemoteApplications(ctx, group, p)
	}) {
		if errors.Is(err, iam.ErrGroupNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if app.Enabled {
			out = append(out, adminconsole.Source{Key: key, Name: name, Issuer: app.Issuer, Operator: app.TrustRoot != iam.ApplicationTrustRootUser})
		}
	}
	return out, nil
}

// SecondFactor fails while the server's accounts need a second factor staff
// cannot enroll: AuthKit's authkit_two_factor probe, or authenticator apps
// (TOTP) offered without their key.
func (c *ControlPlane) SecondFactor(ctx context.Context) error {
	if c == nil || c.client == nil || !c.secondFactor {
		return nil
	}
	for _, p := range c.client.Probes() {
		if p.Name == "authkit_two_factor" {
			if err := p.Check(ctx); err != nil {
				return err
			}
		}
	}
	if c.totp && !slices.Contains(c.client.TwoFactorMethods(), iam.TwoFactorTOTP) {
		return errors.New("controlplane: staff need a second factor, but authenticator apps are unavailable: put a 16, 24 or 32-byte key in totp.key beside keys.json (AUTH_KEYS_PATH)")
	}
	return nil
}
