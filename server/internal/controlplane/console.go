package controlplane

import (
	"context"
	"errors"
	"slices"

	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/server/internal/adminconsole"
)

// RootConsoleSources are root's enabled trusted applications: the
// deployment's own, declared in the bootstrap manifest or by root's
// credential managers, which the console's sign-in page offers once they
// pass its probe.
func (c *ControlPlane) RootConsoleSources(ctx context.Context) ([]adminconsole.Source, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoControlPlane
	}
	var out []adminconsole.Source
	for app, err := range iam.All(func(p iam.PageRequest) (iam.ListPage[iam.RemoteApplication], error) {
		return c.client.ListRemoteApplications(ctx, iam.RootGroup(), p)
	}) {
		if errors.Is(err, iam.ErrGroupNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if app.Enabled {
			out = append(out, adminconsole.Source{Issuer: app.Issuer, Operator: app.TrustRoot != iam.ApplicationTrustRootUser})
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
