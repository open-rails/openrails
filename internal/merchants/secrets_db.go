package merchants

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"

	"github.com/open-rails/openrails/pkg/merchant"
)

// dbSecretStore persists per-merchant secrets in openrails.merchant_secrets.
// This is the self-hosted / dev default: it builds and runs WITHOUT a live
// Vault. A managed deployment swaps in the Vault-backed store (secrets_vault.go)
// with the same (merchant, name) addressing and no schema change.
type dbSecretStore struct {
	pool *db.Pool
}

// NewDBSecretStore returns a database-backed MerchantSecretStore over the given
// pgx pool (the pool that holds the openrails.* schema).
func NewDBSecretStore(pool *db.Pool) (MerchantSecretStore, error) {
	if pool == nil {
		return nil, errors.New("tenancy: pgx pool is required for the DB-backed secret store")
	}
	return &dbSecretStore{pool: pool}, nil
}

func (d *dbSecretStore) Get(ctx context.Context, merchantID merchant.ID, name string) (Secret, error) {
	if err := validateSecretRef(merchantID, name); err != nil {
		return Secret{}, err
	}
	var s Secret
	err := d.pool.MerchantTx(ctx, merchantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT name, value, version
			  FROM openrails.merchant_secrets
			 WHERE merchant_id = $1::uuid AND name = $2
		`, merchantID.String(), name).Scan(&s.Name, &s.Value, &s.Version)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Secret{}, ErrSecretNotFound
		}
		// A query/transport failure is operational, not "secret absent" — callers
		// must retry, not treat as missing.
		return Secret{}, fmt.Errorf("merchants: get merchant secret: %w", errors.Join(ErrSecretBackendUnavailable, err))
	}
	return s, nil
}

func (d *dbSecretStore) Put(ctx context.Context, merchantID merchant.ID, name, value string) (Secret, error) {
	if err := validateSecretRef(merchantID, name); err != nil {
		return Secret{}, err
	}
	// Idempotent rotation: insert version 1, or bump version ONLY when the value
	// actually changes (re-putting the same value is a no-op rotation).
	var s Secret
	err := d.pool.MerchantTx(ctx, merchantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO openrails.merchant_secrets (merchant_id, name, value, version)
			VALUES ($1::uuid, $2, $3, 1)
			ON CONFLICT (merchant_id, name) DO UPDATE
			   SET value      = EXCLUDED.value,
			       version    = CASE WHEN openrails.merchant_secrets.value = EXCLUDED.value
			                         THEN openrails.merchant_secrets.version
			                         ELSE openrails.merchant_secrets.version + 1 END,
			       updated_at = current_timestamp
			RETURNING name, value, version
		`, merchantID.String(), name, value).Scan(&s.Name, &s.Value, &s.Version)
	})
	if err != nil {
		return Secret{}, fmt.Errorf("merchants: put merchant secret: %w", err)
	}
	return s, nil
}

func (d *dbSecretStore) Delete(ctx context.Context, merchantID merchant.ID, name string) error {
	if err := validateSecretRef(merchantID, name); err != nil {
		return err
	}
	err := d.pool.MerchantTx(ctx, merchantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			DELETE FROM openrails.merchant_secrets WHERE merchant_id = $1::uuid AND name = $2
		`, merchantID.String(), name)
		return err
	})
	if err != nil {
		return fmt.Errorf("merchants: delete merchant secret: %w", err)
	}
	return nil
}

func (d *dbSecretStore) List(ctx context.Context, merchantID merchant.ID) ([]string, error) {
	if merchantID.IsZero() {
		return nil, validateSecretRef(merchantID, "x")
	}
	var names []string
	err := d.pool.MerchantTx(ctx, merchantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT name FROM openrails.merchant_secrets WHERE merchant_id = $1::uuid ORDER BY name
		`, merchantID.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return fmt.Errorf("merchants: scan merchant secret name: %w", err)
			}
			names = append(names, n)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("merchants: list merchant secrets: %w", err)
	}
	return names, nil
}
