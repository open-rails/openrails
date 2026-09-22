// Package pgidentity qualifies physical PostgreSQL database identity without
// relying on connection strings, privileged server identifiers, or stored data.
package pgidentity

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrDifferentDatabase = errors.New("PostgreSQL pools do not address the same physical database")

// RequireSameDatabase compares the actual database reached by two pools. The
// identical-pool path acquires no connection, including when its only connection
// is held by an ambient transaction. Distinct pools are checked before binding,
// never while admitting work through an already borrowed transaction.
//
// A random pair of transaction-level advisory locks is visible only in the
// actual cluster and database that holds them. Observing their exact backend and
// keys from the other pool proves identity across roles and connection aliases.
// Every borrowed connection is released and the proof transaction is rolled
// back, including on cancellation. Neither pool is closed.
func RequireSameDatabase(ctx context.Context, first, second *pgxpool.Pool) (err error) {
	if ctx == nil {
		return errors.New("database identity requires a context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if first == nil || second == nil {
		return errors.New("database identity requires both pools")
	}
	if first == second {
		return nil
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("database identity nonce: %w", err)
	}
	keys := [4]int32{}
	for i := range keys {
		keys[i] = int32(binary.BigEndian.Uint32(nonce[i*4:]) & 0x7fffffff)
	}
	conn, err := first.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("database identity holder: %w", err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("database identity transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(context.WithoutCancel(ctx)); rollbackErr != nil {
			err = errors.Join(err, fmt.Errorf("release database identity proof: %w", rollbackErr))
		}
	}()
	var pid int32
	var one, two bool
	if err := tx.QueryRow(ctx, `SELECT pg_catalog.pg_backend_pid(), pg_catalog.pg_try_advisory_xact_lock($1::integer,$2::integer), pg_catalog.pg_try_advisory_xact_lock($3::integer,$4::integer)`, keys[0], keys[1], keys[2], keys[3]).Scan(&pid, &one, &two); err != nil {
		return fmt.Errorf("database identity proof: %w", err)
	}
	if !one || !two {
		return errors.New("database identity proof keys are already held")
	}
	var same bool
	err = second.QueryRow(ctx, `SELECT pg_catalog.count(*) = 2 FROM pg_catalog.pg_locks
 WHERE locktype='advisory' AND pid=$1 AND granted AND mode='ExclusiveLock' AND objsubid=2
 AND database=(SELECT oid FROM pg_catalog.pg_database WHERE datname=pg_catalog.current_database())
 AND ((classid=$2::bigint::pg_catalog.oid AND objid=$3::bigint::pg_catalog.oid) OR (classid=$4::bigint::pg_catalog.oid AND objid=$5::bigint::pg_catalog.oid))`, pid, int64(keys[0]), int64(keys[1]), int64(keys[2]), int64(keys[3])).Scan(&same)
	if err != nil {
		return fmt.Errorf("observe database identity proof: %w", err)
	}
	if !same {
		return ErrDifferentDatabase
	}
	return ctx.Err()
}
