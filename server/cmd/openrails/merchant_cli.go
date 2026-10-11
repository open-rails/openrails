package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchantbootstrap"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/server/internal/bootstrap"
	"github.com/open-rails/openrails/server/internal/hostconfig"
)

// cliMerchantManifest is the manifest run-server boots with, for a one-off
// tool: path, else the conventional path when it exists (nil when neither
// does), with the configured overlays. Keys only the server reads
// (remote_application) are read and left out, so every tool takes the
// server's own file.
func cliMerchantManifest(ctx context.Context, path string) (*merchantbootstrap.BillingConfig, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		if _, err := os.Stat(bootstrap.DefaultMerchantConfigManifestPath); errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		path = bootstrap.DefaultMerchantConfigManifestPath
	}
	manifest, err := bootstrap.LoadMerchantConfigManifestFiles(path, hostconfig.FromContext(ctx).MerchantManifestOverlays...)
	if err != nil {
		return nil, fmt.Errorf("merchant manifest %s: %w", path, err)
	}
	return manifest.Declarations(), nil
}

func resolveCLIMerchant(ctx context.Context, database *db.DB, slug string) (billing.MerchantID, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return billing.MerchantID{}, fmt.Errorf("--merchant is required (public name or id:<uuid>)")
	}
	if rawID, explicit := strings.CutPrefix(slug, "id:"); explicit {
		tid, err := billing.ParseMerchantID(rawID)
		if err != nil {
			return billing.MerchantID{}, err
		}
		return tid, database.RequireMerchantID(ctx, tid)
	}
	directory, err := merchants.NewDirectoryService(database.DataPool())
	if err != nil {
		return billing.MerchantID{}, err
	}
	selected, err := directory.GetBySlug(ctx, slug)
	if err != nil {
		return billing.MerchantID{}, fmt.Errorf("resolve merchant %q: %w", slug, err)
	}
	return selected.ID, nil
}

func resolveConfiguredCLIMerchant(ctx context.Context, cfg *config.Config, name string) (billing.MerchantID, error) {
	if cfg == nil || cfg.DB == nil {
		return billing.MerchantID{}, fmt.Errorf("config not loaded")
	}
	database, err := db.NewDB(ctx, cfg)
	if err != nil {
		return billing.MerchantID{}, err
	}
	defer database.Close()
	return resolveCLIMerchant(ctx, database, name)
}
