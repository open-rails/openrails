package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	authcore "github.com/open-rails/authkit/embedded"
	"github.com/open-rails/openrails/internal/controlplane"
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
	// The authority owns a separate pool: callers may already hold a billing pin.
	authorityPool, err := pgxpool.NewWithConfig(ctx, database.Pool().Config())
	if err != nil {
		return merchant.ID{}, err
	}
	defer authorityPool.Close()
	groups, err := authcore.NewGroupDirectory(authorityPool, "")
	if err != nil {
		return merchant.ID{}, err
	}
	directory, err := merchants.NewDirectoryService(database.DataPool())
	if err != nil {
		return merchant.ID{}, err
	}
	directory.WithNameAuthority(controlplane.MerchantNameAuthority(groups))
	selected, err := directory.GetBySlug(ctx, slug)
	if err != nil {
		return merchant.ID{}, fmt.Errorf("resolve merchant %q through AuthKit: %w", slug, err)
	}
	return selected.ID, nil
}
