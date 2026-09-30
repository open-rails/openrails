package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

func resolveCLIMerchant(ctx context.Context, database *db.DB, slug string) (merchant.ID, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return merchant.ID{}, fmt.Errorf("--merchant is required (public name or id:<uuid>)")
	}
	if rawID, explicit := strings.CutPrefix(slug, "id:"); explicit {
		tid, err := merchant.ParseID(rawID)
		if err != nil {
			return merchant.ID{}, err
		}
		return tid, database.RequireMerchantID(ctx, tid)
	}
	directory, err := merchants.NewDirectoryService(database.DataPool())
	if err != nil {
		return merchant.ID{}, err
	}
	selected, err := directory.GetBySlug(ctx, slug)
	if err != nil {
		return merchant.ID{}, fmt.Errorf("resolve merchant %q: %w", slug, err)
	}
	return selected.ID, nil
}

func resolveConfiguredCLIMerchant(ctx context.Context, cfg *config.Config, name string) (merchant.ID, error) {
	if cfg == nil || cfg.DB == nil {
		return merchant.ID{}, fmt.Errorf("config not loaded")
	}
	database, err := db.NewDB(ctx, cfg.DB)
	if err != nil {
		return merchant.ID{}, err
	}
	defer database.Close()
	return resolveCLIMerchant(ctx, database, name)
}
