package crypto

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"

	"github.com/open-rails/openrails/pkg/merchant"
)

// dbDEKStore persists wrapped per-merchant DEKs in openrails.merchant_deks.
// This is the self-hosted / dev default. A managed deployment can swap in a
// KMS-backed DEKStore with the same interface and no caller change.
type dbDEKStore struct {
	pool *db.Pool
}

// NewDBDEKStore returns a Postgres-backed DEKStore over the given pool (the pool
// that holds the openrails.* schema, i.e. the control-plane pool).
func NewDBDEKStore(pool *db.Pool) (DEKStore, error) {
	if pool == nil {
		return nil, errors.New("crypto: pgx pool is required for the DB-backed DEK store")
	}
	return &dbDEKStore{pool: pool}, nil
}

func (s *dbDEKStore) GetWrappedDEK(ctx context.Context, merchantID merchant.ID) ([]byte, bool, error) {
	var wrapped []byte
	err := s.pool.CommittedMerchantTx(ctx, merchantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		wrapped, err = gen.New(tx).GetMerchantWrappedDEK(ctx, merchantID.UUID())
		return err
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("crypto: get wrapped DEK: %w", err)
	}
	return wrapped, true, nil
}

// PutWrappedDEK inserts the wrapped DEK, or — if a row already exists for the
// merchant (concurrent first-use) — keeps the existing wrapped DEK and returns it.
// The ON CONFLICT ... DO UPDATE with the no-op assignment guarantees RETURNING
// yields the row that actually persists, so racing creators converge.
func (s *dbDEKStore) PutWrappedDEK(ctx context.Context, merchantID merchant.ID, wrapped []byte) ([]byte, error) {
	var stored []byte
	err := s.pool.CommittedMerchantTx(ctx, merchantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		stored, err = gen.New(tx).PutMerchantWrappedDEK(ctx, gen.PutMerchantWrappedDEKParams{MerchantID: merchantID.UUID(), WrappedDek: wrapped})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("crypto: put wrapped DEK: %w", err)
	}
	return stored, nil
}
