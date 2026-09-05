package main

import (
	"context"
	"fmt"

	authcore "github.com/open-rails/authkit/embedded"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// openCLINameDirectory owns a separate read-only authority pool. Catalog work
// may pin the billing pool's only connection while resolving current names.
func openCLINameDirectory(ctx context.Context, cfg *config.Config) (*merchants.Service, merchant.NameAuthority, func(), error) {
	if cfg == nil || cfg.DB == nil {
		return nil, nil, nil, fmt.Errorf("config not loaded")
	}
	database, err := db.NewDB(ctx, cfg.DB)
	if err != nil {
		return nil, nil, nil, err
	}
	close := func() { _ = database.Close() }
	groups, err := authcore.NewGroupDirectory(database.Pool(), "")
	if err != nil {
		close()
		return nil, nil, nil, err
	}
	authority := controlplane.MerchantNameAuthority(groups)
	directory, err := merchants.NewDirectoryService(database.DataPool())
	if err != nil {
		close()
		return nil, nil, nil, err
	}
	return directory.WithNameAuthority(authority), authority, close, nil
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
