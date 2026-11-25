package controlplane

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/server/internal/adminconsole"
)

// ConsoleSources are the trusted issuers staff may sign in to the console
// at: root's applications (declared in the bootstrap manifest) and each live
// merchant's, enabled ones only. Which of them sign people in, the console's
// directory decides from their metadata.
func (c *ControlPlane) ConsoleSources(ctx context.Context) ([]adminconsole.Source, error) {
	if c == nil || c.client == nil || c.pool == nil {
		return nil, ErrNoControlPlane
	}
	out, err := c.groupSources(ctx, iam.RootGroup(), "root", "")
	if err != nil {
		return nil, err
	}
	status := "active"
	var afterAt *time.Time
	var afterID *uuid.UUID
	for {
		rows, err := gen.New(c.pool).ListPlatformMerchants(ctx, gen.ListPlatformMerchantsParams{Status: &status, AfterAt: afterAt, AfterID: afterID, RowLimit: 200})
		if err != nil {
			return nil, fmt.Errorf("controlplane: console sign-in sources: %w", err)
		}
		for _, row := range rows {
			mid := billing.MerchantID(row.ID)
			name := row.Slug
			if c.merchants != nil {
				name = cmp.Or(c.merchants.DisplayName(ctx, mid), row.Slug)
			}
			sources, err := c.groupSources(ctx, iam.GroupByID(mid.String()), mid.String(), name)
			if err != nil {
				return nil, err
			}
			out = append(out, sources...)
		}
		if len(rows) < 200 {
			return out, nil
		}
		last := rows[len(rows)-1]
		afterAt, afterID = &last.CreatedAt, &last.ID
	}
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
