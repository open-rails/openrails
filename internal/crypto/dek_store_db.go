package crypto

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"

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
	err := s.pool.MerchantTx(ctx, merchantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT wrapped_dek FROM openrails.merchant_deks WHERE merchant_id = $1::uuid
		`, merchantID.String()).Scan(&wrapped)
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
	err := s.pool.MerchantTx(ctx, merchantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO openrails.merchant_deks (merchant_id, wrapped_dek)
			VALUES ($1::uuid, $2)
			ON CONFLICT (merchant_id) DO UPDATE
			   SET merchant_id = openrails.merchant_deks.merchant_id
			RETURNING wrapped_dek
		`, merchantID.String(), wrapped).Scan(&stored)
	})
	if err != nil {
		return nil, fmt.Errorf("crypto: put wrapped DEK: %w", err)
	}
	return stored, nil
}
