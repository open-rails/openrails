package controlplane

// or#914 item 5: the host entrypoint for the dormant-merchant sweep. Policy
// and queries live in internal/merchants (SweepDormant); this wires the one
// authkit primitive it consumes — DeletePermissionGroup with an EXPLICIT
// ReleaseSlug, pinned to the merchant persona. A host runs it on its own
// cadence (openrails-saas: a River worker calling SweepDormantMerchants once
// per tick, armed via its own destructive setting; th#1774 is the shape).

import (
	"context"
	"errors"

	"github.com/open-rails/authkit"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/merchants"
)

// DormancySweepConfig / DormancySweepResult are the nameable aliases for the
// internal/merchants sweep types (the BootstrapOptions alias pattern).
type (
	DormancySweepConfig = merchants.DormancySweepConfig
	DormancySweepResult = merchants.DormancySweepResult
)

// SweepDormantMerchants runs one dormant-merchant sweep pass through the
// attached control plane: never-used merchants (no provider, no money, no
// catalog, no customers — probed per merchant under MerchantTx) past cfg.TTL
// are warned via openrails.merchant_dormancy_notices, and on an ARMED pass,
// once past cfg.WarningLead, deleted — the merchant group with ReleaseSlug
// (the camped name becomes claimable again) plus a directory-row soft-delete.
// Unarmed (the default) is a dry run. Safe to re-run; a crash between the
// group half and the row half converges on the next pass.
func SweepDormantMerchants(ctx context.Context, a *app.App, cfg DormancySweepConfig) (DormancySweepResult, error) {
	cp := Get(a)
	if cp == nil || cp.Core() == nil {
		return DormancySweepResult{}, errors.New("dormancy sweep: no control plane attached (call Attach first)")
	}
	dir, err := merchants.NewDirectoryService(cp.Pool())
	if err != nil {
		return DormancySweepResult{}, err
	}
	core := cp.Core()
	release := func(ctx context.Context, slug string) error {
		err := core.DeletePermissionGroup(ctx, controlplane.MerchantType, slug,
			authkit.DeletePermissionGroupOptions{ReleaseSlug: true})
		if errors.Is(err, authkit.ErrGroupNotFound) {
			// Already gone (a previous pass crashed between halves, or the
			// group died some other way): the row half still needs finishing.
			return nil
		}
		return err
	}
	return dir.SweepDormant(ctx, cfg, release)
}
