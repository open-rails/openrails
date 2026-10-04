package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchants"
)

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
