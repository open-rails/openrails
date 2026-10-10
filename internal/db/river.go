package db

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/pgidentity"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// RiverJobInserter is the host's enqueue capability, without lifecycle ownership.
// InsertTx makes the financial admission and its wakeup one PostgreSQL commit.
type RiverJobInserter interface {
	InsertTx(context.Context, pgx.Tx, river.JobArgs, *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// runtimeBinding is what the runtime composer binds after the handle exists,
// shared by every handle derived from it: the River producer and the
// merchant configuration.
type runtimeBinding struct {
	mu       sync.RWMutex
	inserter RiverJobInserter
	config   MerchantConfig
}

// MerchantConfig is the merchants' configuration, which lives in a file or
// Vault, never in Postgres. The runtime binds the merchants service, which
// code holding only a database handle reads it through.
type MerchantConfig interface {
	MerchantSettings(ctx context.Context, id billing.MerchantID) (displayName string, settings billing.MerchantSettings, err error)
}

// SetMerchantConfig binds the merchant configuration every handle derived
// from d reads through.
func (d *DB) SetMerchantConfig(config MerchantConfig) {
	if d == nil {
		return
	}
	if d.river == nil {
		d.river = &runtimeBinding{}
	}
	d.river.mu.Lock()
	defer d.river.mu.Unlock()
	d.river.config = config
}

// MerchantConfig is the bound merchant configuration; nil when none is bound.
func (d *DB) MerchantConfig() MerchantConfig {
	if d == nil || d.river == nil {
		return nil
	}
	d.river.mu.RLock()
	defer d.river.mu.RUnlock()
	return d.river.config
}

// ErrRiverTablesMissing reports a River schema that was never migrated.
var ErrRiverTablesMissing = errors.New("River's tables are missing")

// ValidateRiverJobBinding must succeed before the runtime enables a producer.
// Validate during composition, before requests hold any transaction or pool pin.
func (d *DB) ValidateRiverJobBinding(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	if d == nil || ctx == nil {
		return fmt.Errorf("River binding requires the billing database and context")
	}
	if marked, _ := ctx.Value(transactionContextKey{}).(bool); marked || d.pgtx != nil {
		return fmt.Errorf("River binding must precede caller transactions: %w", ErrCallerTransaction)
	}
	if pin, _ := ctx.Value(merchantPgxConnKey{}).(*lazyMerchantPgxConn); pin != nil {
		return fmt.Errorf("River binding must precede merchant connection pins: %w", ErrCallerTransaction)
	}
	if err := pgidentity.RequireSameDatabase(ctx, d.pool, pool); err != nil {
		return fmt.Errorf("River queue database: %w", err)
	}
	if schema == "" {
		return fmt.Errorf("River binding requires its actual schema")
	}
	exists, err := gen.New(pool).RiverQueueTableExists(ctx, pgx.Identifier{schema, "river_job"}.Sanitize())
	if err != nil {
		return fmt.Errorf("River queue table: %w", err)
	}
	if exists == nil || !*exists {
		return fmt.Errorf("%w from schema %q", ErrRiverTablesMissing, schema)
	}
	return nil
}

// SetRiverJobInserter is called by the runtime composer after binding validation,
// never by a producer. Nil invalidates a previously composed producer.
// Tx-scoped DBs retain this binding so composition/abort cannot leave a stale
// separately constructed producer behind.
func (d *DB) SetRiverJobInserter(inserter RiverJobInserter) {
	if d == nil {
		return
	}
	if d.river == nil {
		d.river = &runtimeBinding{}
	}
	d.river.mu.Lock()
	defer d.river.mu.Unlock()
	d.river.inserter = inserter
}

func (d *DB) InsertRiverJobTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) error {
	if d == nil || d.river == nil || tx == nil {
		return fmt.Errorf("financial operation requires a bound River producer and transaction")
	}
	d.river.mu.RLock()
	inserter := d.river.inserter
	d.river.mu.RUnlock()
	if inserter == nil {
		return fmt.Errorf("financial operation requires a bound River producer")
	}
	// River owns its SQL namespace. Unwrap only our schema adapter while
	// retaining the exact host transaction/savepoint and connection.
	for {
		scoped, ok := tx.(schemaTx)
		if !ok {
			break
		}
		tx = scoped.Tx
	}
	_, err := inserter.InsertTx(ctx, tx, args, opts)
	return err
}

// InsertRiverJob enqueues args in d's own transaction, so the job commits or
// rolls back with the work that queued it.
func (d *DB) InsertRiverJob(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) error {
	if d == nil || d.pgtx == nil {
		return fmt.Errorf("a job queued with its work requires that work's transaction")
	}
	return d.InsertRiverJobTx(ctx, d.pgtx, args, opts)
}
