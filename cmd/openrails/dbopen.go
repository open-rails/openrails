package main

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
)

// openCLIDB is the ONE door a standalone CLI command opens a merchant-scoped
// database handle through. It opens the pool from config and runs the SAME
// RLS-posture gate the server boot path runs (internal/app.buildRuntime), so an
// operator command can never do merchant-scoped work on a role that skips every
// merchant_isolation policy (or#888; same class as or#885 for embedded). The
// gate is unconditional — development included (or#782).
//
// DELIBERATE EXEMPTION: `openrails migrate` does NOT come through here. DDL
// requires the privileged owner role by definition — it creates the very
// policies and the unprivileged openrails_app role this gate demands. It opens
// its own handle inside internal/migrate; see the note at its wiring in main.go.
// Every OTHER command that touches merchant rows must use this door.
func openCLIDB(ctx context.Context, cfg *config.Config) (*db.DB, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg == nil || cfg.DB == nil {
		return nil, fmt.Errorf("config not loaded")
	}
	database, err := db.NewDB(cfg.DB)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := database.EnforceRLSPosture(ctx); err != nil {
		_ = database.Close()
		return nil, err
	}
	return database, nil
}
