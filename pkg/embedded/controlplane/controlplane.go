// Package controlplane is the OPT-IN OpenRails AuthKit control-plane helper for
// the standalone binary and AuthKit-using embedded hosts (#284). The embedded
// CORE (pkg/embedded.New -> bootstrap.NewApp -> internal/app) deliberately does
// NOT import internal/controlplane (and through it AuthKit): control-plane /
// tenant-management is standalone-only. Importing THIS package is what pulls the
// control plane (and AuthKit) onto a host's graph.
//
// The control plane is held on app.App as `any` (App.ControlPlane); this package
// builds the concrete *controlplane.ControlPlane, attaches it via
// App.SetControlPlane, and recovers it with Get for the standalone gin server.
package controlplane

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
)

// Get recovers the concrete *controlplane.ControlPlane attached to the app, or
// nil when the control plane is disabled (verifier-only mode) or the field holds
// some other type.
func Get(a *app.App) *controlplane.ControlPlane {
	if a == nil {
		return nil
	}
	cp, _ := a.ControlPlane.(*controlplane.ControlPlane)
	return cp
}

// Attach builds the OpenRails-owned AuthKit control plane (#224) when enabled in
// config and attaches it to the app. It is a no-op (control plane left nil) when
// cfg.Auth.ControlPlaneEnabled() is false. injectedPool, when non-nil, is reused
// as the control-plane pool; otherwise Attach creates an OpenRails-owned pool
// whose lifecycle App.Close manages.
//
// This is the construction that used to live in internal/app.BootstrapWithOptions
// (#284); the standalone/opt-in path now calls it explicitly after building the
// app.
func Attach(ctx context.Context, a *app.App, cfg *config.Config, injectedPool *pgxpool.Pool) error {
	if a == nil || cfg == nil {
		return nil
	}
	if cfg.Auth == nil || !cfg.Auth.ControlPlaneEnabled() {
		return nil
	}

	// The control plane needs a pgx pool over the database holding AuthKit's
	// profiles.* schema. Reuse an injected pool when provided, else create one.
	pool := injectedPool
	ownedPool := false
	if pool == nil {
		p, err := pgxpool.New(ctx, cfg.DB.GetConnectionString())
		if err != nil {
			return fmt.Errorf("control plane: build pgx pool: %w", err)
		}
		pool = p
		ownedPool = true
	}

	cp, err := controlplane.New(ctx, cfg, pool)
	if err != nil {
		if ownedPool {
			pool.Close()
		}
		return fmt.Errorf("build control plane: %w", err)
	}

	if ownedPool {
		a.SetControlPlane(cp, pool)
	} else {
		a.SetControlPlane(cp, nil)
	}
	return nil
}

// RunBootstrap idempotently bootstraps the OpenRails-owned AuthKit control plane
// (#224): ensures the default tenant's operator org, OpenRails operator role, the
// openrails.* permission catalog, and an initial operator OAT, plus the
// managed-hosting platform superadmin org+role when configured (#226). It is a
// no-op when the control plane is disabled.
//
// Call it AFTER migrations have run (so billing.tenants and profiles.* exist) and
// at startup. Safe to re-run. This was App.RunControlPlaneBootstrap before #284.
func RunBootstrap(ctx context.Context, a *app.App, opts controlplane.BootstrapOptions) (*controlplane.BootstrapResult, error) {
	cp := Get(a)
	if cp == nil {
		return nil, nil
	}
	if !opts.MintInitialOAT {
		opts.MintInitialOAT = true
	}
	res, err := cp.Bootstrap(ctx, opts)
	if err != nil {
		return res, err
	}
	// #226: also ensure the managed-hosting platform superadmin org + role when
	// configured. No-op when no platform org is set (single-tenant / non-managed).
	if _, perr := cp.BootstrapPlatform(ctx); perr != nil {
		return res, fmt.Errorf("platform superadmin bootstrap: %w", perr)
	}
	return res, nil
}
