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
	"github.com/open-rails/openrails/internal/db/gen"
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
	// Keep SQLC on the actual witness transaction and observer pool. A
	// schema-aware application DB wrapper would change this physical proof.
	witness, err := gen.New(tx).AcquireDatabaseIdentityWitness(ctx, gen.AcquireDatabaseIdentityWitnessParams{
		FirstClass: keys[0], FirstObject: keys[1], SecondClass: keys[2], SecondObject: keys[3],
	})
	if err != nil {
		return fmt.Errorf("database identity proof: %w", err)
	}
	if !witness.FirstLocked || !witness.SecondLocked {
		return errors.New("database identity proof keys are already held")
	}
	same, err := gen.New(second).ObserveDatabaseIdentityWitness(ctx, gen.ObserveDatabaseIdentityWitnessParams{
		BackendPid: witness.BackendPid, FirstClass: int64(keys[0]), FirstObject: int64(keys[1]), SecondClass: int64(keys[2]), SecondObject: int64(keys[3]),
	})
	if err != nil {
		return fmt.Errorf("observe database identity proof: %w", err)
	}
	if !same {
		return ErrDifferentDatabase
	}
	return ctx.Err()
}
