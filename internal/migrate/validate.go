package migrate

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
	"github.com/open-rails/openrails/internal/sqlschema"
)

// Validate checks existing OpenRails and River ledgers and writes nothing:
// every migration of the chain is recorded, by the same file and with the
// content this build carries. Apply settles a difference; Validate only
// refuses it. Startup does not prepare partitions here; normal runtime
// maintenance keeps its existing behavior, and explicit Apply prepares
// initial partitions.
func Validate(ctx context.Context, pool *pgxpool.Pool, opts Options) error {
	if pool == nil {
		return errors.New("openrails: validating migrations needs a Postgres pool")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	schema := opts.Schema
	if schema == "" {
		schema = sqlschema.Authored
	}
	chainFS := opts.Chain
	if chainFS == nil {
		chainFS = postgresmigrations.FS
	}
	chain, err := loadChain(chainFS, schema)
	if err != nil {
		return err
	}
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer sqlDB.Close()
	m := migratekit.NewPostgres(sqlDB, config.MigratekitApp).WithSchema(schema)
	if err := m.ValidateAllApplied(ctx, chain); err != nil {
		return validationError(err)
	}
	applied, err := m.AppliedRecords(ctx)
	if err != nil {
		return validationError(err)
	}
	for _, migration := range chain {
		record := applied[migratekit.Prefix(migration.Name)]
		if record.Filename != "" && record.Filename != migration.Name {
			return validationError(fmt.Errorf("schema %q has migration %q where %q is required", schema, record.Filename, migration.Name))
		}
		if edited(record, migration) {
			return validationError(fmt.Errorf("schema %q applied %s with content %.12s where this build carries %.12s",
				schema, migration.Name, record.Digest, migratekit.ContentDigest(migration.Content)))
		}
	}
	if opts.RiverSchema != "" {
		if err := riverhelpers.ValidateMigrations(ctx, pool, riverhelpers.MigrationOptions{Schema: opts.RiverSchema}); err != nil {
			return validationError(err)
		}
	}
	return nil
}

// edited reports a record whose content is not the migration's, by the rule
// migratekit's strict integrity settles: an edit that leaves the SQL tokens
// alone is not one, and a record without a digest cannot be compared.
func edited(record migratekit.AppliedRecord, migration migratekit.Migration) bool {
	return record.Digest != "" && record.Digest != migratekit.ContentDigest(migration.Content) &&
		(record.SemanticDigest == "" || record.SemanticDigest != migratekit.SemanticContentDigest(migration.Content))
}

func validationError(err error) error {
	var network net.Error
	var connect *pgconn.ConnectError
	var postgres *pgconn.PgError
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network) || errors.As(err, &connect) ||
		errors.As(err, &postgres) && (strings.HasPrefix(postgres.Code, "08") || strings.HasPrefix(postgres.Code, "53") || strings.HasPrefix(postgres.Code, "57")) {
		return fmt.Errorf("openrails: validate database migrations: %w", err)
	}
	return fmt.Errorf("%w; run openrails.Migrate before application startup: %w", billing.ErrMigrationsRequired, err)
}
