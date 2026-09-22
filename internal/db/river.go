package db

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/pgidentity"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// RiverJobInserter is the host's enqueue capability, without lifecycle ownership.
// InsertTx makes the financial admission and its wakeup one PostgreSQL commit.
type RiverJobInserter interface {
	InsertTx(context.Context, pgx.Tx, river.JobArgs, *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

type riverBinding struct {
	mu       sync.RWMutex
	inserter RiverJobInserter
}

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
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT pg_catalog.to_regclass($1) IS NOT NULL`, pgx.Identifier{schema, "river_job"}.Sanitize()).Scan(&exists); err != nil {
		return fmt.Errorf("River queue table: %w", err)
	}
	if !exists {
		return fmt.Errorf("River queue table is missing from schema %q", schema)
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
		d.river = &riverBinding{}
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
