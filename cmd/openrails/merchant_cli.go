package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/pkg/merchant"
)

func resolveCLIMerchant(ctx context.Context, database *db.DB, slug string) (merchant.ID, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return merchant.ID{}, fmt.Errorf("--merchant is required (slug or id)")
	}
	if tid, err := merchant.ParseID(slug); err == nil {
		return tid, nil
	}
	var id string
	if err := database.DataPool().
		QueryRow(ctx, `SELECT id::text FROM openrails.merchants WHERE lower(slug) = lower($1) AND deleted_at IS NULL`, slug).
		Scan(&id); err != nil {
		return merchant.ID{}, fmt.Errorf("resolve merchant %q: %w", slug, err)
	}
	return merchant.ParseID(id)
}
