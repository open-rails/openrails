// Package controlplane wires the OpenRails AuthKit control plane onto an app
// graph (#284). The embedded CORE (pkg/embedded.New -> bootstrap.NewApp ->
// internal/app) deliberately does NOT import internal/controlplane (and through
// it AuthKit): embedded hosts are host-authenticated and opt in by importing
// THIS package, which is what pulls the control plane (and AuthKit) onto a
// host's graph. The STANDALONE binary, by contrast, always attaches the control
// plane (#469): there is no verifier-only standalone mode, and an Attach
// failure is fatal at boot.
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
// nil when no control plane has been attached (an embedded host that never
// called Attach) or the field holds some other type.
func Get(a *app.App) *controlplane.ControlPlane {
	if a == nil {
		return nil
	}
	cp, _ := a.ControlPlane.(*controlplane.ControlPlane)
	return cp
}

// Attach builds the OpenRails-owned AuthKit control plane (#224) and attaches it
// to the app. Construction always happens (#469: there is no disabled state);
// any failure — missing issuer, bad keys, unreachable DB — is returned so the
// standalone boot path can exit non-zero. injectedPool, when non-nil, is reused
// as the control-plane pool; otherwise Attach creates an OpenRails-owned pool
// whose lifecycle App.Close manages.
func Attach(ctx context.Context, a *app.App, cfg *config.Config, injectedPool *pgxpool.Pool) error {
	if a == nil || cfg == nil {
		return fmt.Errorf("control plane: app and config are required")
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
// (#224): ensures the bootstrap authority, OpenRails operator role, the
// openrails.* permission catalog, and an initial operator service token.
// Calling it without an attached control plane is a wiring error (#469: the
// standalone always attaches one first).
//
// Call it AFTER migrations have run (so openrails.merchants and profiles.* exist) and
// at startup. Safe to re-run. This was App.RunControlPlaneBootstrap before #284.
func RunBootstrap(ctx context.Context, a *app.App, opts controlplane.BootstrapOptions) (*controlplane.BootstrapResult, error) {
	cp := Get(a)
	if cp == nil {
		return nil, fmt.Errorf("control plane bootstrap: no control plane attached (call Attach first)")
	}
	if !opts.MintInitialServiceToken {
		opts.MintInitialServiceToken = true
	}
	res, err := cp.Bootstrap(ctx, opts)
	if err != nil {
		return res, err
	}
	// #226 compatibility: also ensure the legacy managed-hosting superadmin role
	// when configured. No-op in single-tenant / non-managed deployments.
	if _, perr := cp.BootstrapPlatform(ctx); perr != nil {
		return res, fmt.Errorf("platform superadmin bootstrap: %w", perr)
	}
	return res, nil
}
