package db

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
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

// SetRiverJobInserter is called by the runtime composer, never by a producer.
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
	_, err := inserter.InsertTx(ctx, tx, args, opts)
	return err
}
