package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/open-rails/openrails/internal/standalonedb"
)

// Migrate creates or upgrades OpenRails' tables in Config.Schema and River's
// in Config.RiverSchema through pool, whose role then owns them and runs
// OpenRails with no grants. It migrates River whichever fleet Start will run
// (River's migrations are idempotent and serialized with the host's), so New
// and Start run no DDL. With Config.SchemaOwner it hands Config.Schema, and
// the River schema when it is OpenRails' own (the default), to that role.
// With Config.ControlPlane it also migrates the control plane's AuthKit
// schema.
func Migrate(ctx context.Context, pool *pgxpool.Pool, cfg config.Config) error {
	if pool == nil {
		return fmt.Errorf("openrails: Migrate requires a Postgres pool")
	}
	schema := config.SchemaName(&cfg)
	if !validIdentifier(schema) {
		return fmt.Errorf("openrails: invalid database schema %q", schema)
	}
	riverSchema := config.RiverSchemaName(&cfg)
	if err := validRiverSchema(riverSchema); err != nil {
		return err
	}
	if err := migrate.ApplyPostgresMigrations(ctx, pool, migrate.Options{Schema: schema}); err != nil {
		return err
	}
	if err := migrate.ApplyRiver(ctx, pool, riverSchema); err != nil {
		return err
	}
	if owner := strings.TrimSpace(cfg.SchemaOwner); owner != "" {
		owned := []string{schema}
		// An explicit RiverSchema may be the host's fleet's: it keeps its owner.
		if riverSchema != schema && strings.TrimSpace(cfg.RiverSchema) == "" {
			owned = append(owned, riverSchema)
		}
		for _, s := range owned {
			if _, err := migrate.HandOver(ctx, pool, s, owner); err != nil {
				return fmt.Errorf("openrails: hand schema %s to %s: %w", s, owner, err)
			}
		}
	}
	if cfg.ControlPlane != nil {
		return standalonedb.ApplyAuthKit(ctx, pool)
	}
	return nil
}
