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

// Migrate creates or upgrades OpenRails' database objects through pool, whose
// role then owns them and runs OpenRails with no grants. The schema and River
// ownership come from cfg; a host-owned fleet migrates River itself. With
// Config.ControlPlane it also migrates the control plane's AuthKit schema.
func Migrate(ctx context.Context, pool *pgxpool.Pool, cfg config.Config) error {
	if pool == nil {
		return fmt.Errorf("openrails: Migrate requires a Postgres pool")
	}
	cfg.RiverSchema = strings.ToLower(strings.TrimSpace(cfg.RiverSchema))
	schema := cfg.SchemaName()
	if !validIdentifier(schema) {
		return fmt.Errorf("openrails: invalid database schema %q", schema)
	}
	opts := migrate.Options{Schema: schema, HostRiver: cfg.River.HostOwned()}
	switch cfg.River {
	case "", config.RiverManaged:
		opts.RiverSchema = cfg.RiverSchema
		if opts.RiverSchema == "" {
			opts.RiverSchema = config.DefaultRiverSchema
		}
		if !validIdentifier(opts.RiverSchema) {
			return fmt.Errorf("openrails: Config.RiverSchema %q is not a valid schema name", cfg.RiverSchema)
		}
	case config.RiverHostOwned:
	default:
		return fmt.Errorf("openrails: Config.River %q is invalid; use RiverManaged or RiverHostOwned", cfg.River)
	}
	if err := migrate.ApplyPostgresMigrations(ctx, pool, opts); err != nil {
		return err
	}
	if cfg.ControlPlane != nil {
		return standalonedb.ApplyAuthKit(ctx, pool)
	}
	return nil
}
