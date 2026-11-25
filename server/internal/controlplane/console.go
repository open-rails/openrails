package controlplane

import (
	"context"
	"errors"
	"slices"

	"github.com/open-rails/authkit/iam"
)

// SecondFactor fails while the server's accounts need a second factor staff
// cannot enroll: AuthKit's authkit_two_factor probe, or authenticator apps
// (TOTP) unavailable for want of their key.
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
	if !slices.Contains(c.client.TwoFactorMethods(), iam.TwoFactorTOTP) {
		return errors.New("controlplane: staff need a second factor, but authenticator apps are unavailable: put a 16, 24 or 32-byte key in totp.key beside keys.json")
	}
	return nil
}
