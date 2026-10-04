package merchants

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchant"
)

// dbSecretStore persists per-merchant secrets in billing.merchant_secrets.
// This is the self-hosted / dev default: it builds and runs WITHOUT a live
// Vault. A managed deployment swaps in the Vault-backed store (secrets_vault.go)
// with the same (merchant, name) addressing and no schema change.
type dbSecretStore struct {
	database *db.DB
}

// NewDBSecretStore returns a database-backed MerchantSecretStore over the given
// pgx pool (the pool that holds the openrails.* schema).
func NewDBSecretStore(pool *db.Pool) (MerchantSecretStore, error) {
	if pool == nil {
		return nil, errors.New("merchants: pgx pool is required for the DB-backed secret store")
	}
	database, err := db.NewWithPGXPool(pool.Raw(), pool.Schema())
	if err != nil {
		return nil, err
	}
	return &dbSecretStore{database: database}, nil
}

func (d *dbSecretStore) Get(ctx context.Context, merchantID billing.MerchantID, name string) (Secret, error) {
	if err := validateSecretRef(merchantID, name); err != nil {
		return Secret{}, err
	}
	var s Secret
	err := d.database.RunInMerchantConn(merchant.WithID(ctx, merchantID), func(ctx context.Context) error {
		row, err := d.database.Gen(ctx).GetMerchantSecret(ctx, gen.GetMerchantSecretParams{MerchantID: merchantID.UUID(), Name: name})
		s = Secret{Name: row.Name, Value: row.Value, Version: int(row.Version)}
		return err
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

func (d *dbSecretStore) Put(ctx context.Context, merchantID billing.MerchantID, name, value string) (Secret, error) {
	if err := validateSecretRef(merchantID, name); err != nil {
		return Secret{}, err
	}
	// Idempotent rotation: insert version 1, or bump version ONLY when the value
	// actually changes (re-putting the same value is a no-op rotation).
	var s Secret
	err := d.database.DataPool().MerchantTx(ctx, merchantID, func(ctx context.Context, tx pgx.Tx) error {
		row, err := gen.New(tx).PutMerchantSecret(ctx, gen.PutMerchantSecretParams{MerchantID: merchantID.UUID(), Name: name, Value: value})
		s = Secret{Name: row.Name, Value: row.Value, Version: int(row.Version)}
		return err
	})
	if err != nil {
		return Secret{}, fmt.Errorf("merchants: put merchant secret: %w", err)
	}
	return s, nil
}

func (d *dbSecretStore) Delete(ctx context.Context, merchantID billing.MerchantID, name string) error {
	if err := validateSecretRef(merchantID, name); err != nil {
		return err
	}
	err := d.database.DataPool().MerchantTx(ctx, merchantID, func(ctx context.Context, tx pgx.Tx) error {
		return gen.New(tx).DeleteMerchantSecret(ctx, gen.DeleteMerchantSecretParams{MerchantID: merchantID.UUID(), Name: name})
	})
	if err != nil {
		return fmt.Errorf("merchants: delete merchant secret: %w", err)
	}
	return nil
}

func (d *dbSecretStore) List(ctx context.Context, merchantID billing.MerchantID) ([]string, error) {
	if merchantID.IsZero() {
		return nil, validateSecretRef(merchantID, "x")
	}
	var names []string
	err := d.database.RunInMerchantConn(merchant.WithID(ctx, merchantID), func(ctx context.Context) error {
		var err error
		names, err = d.database.Gen(ctx).ListMerchantSecretNames(ctx, merchantID.UUID())
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("merchants: list merchant secrets: %w", err)
	}
	return names, nil
}

func (d *dbSecretStore) StageSecret(ctx context.Context, id billing.MerchantID, name, value string) (Secret, error) {
	if err := validateSecretRef(id, name); err != nil {
		return Secret{}, err
	}
	var result Secret
	err := d.database.DataPool().CommittedMerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.LockLiveMerchantForSecretWrite(ctx, id.UUID()); err != nil {
			return err
		}
		if err := q.StageMerchantSecret(ctx, gen.StageMerchantSecretParams{MerchantID: id.UUID(), Name: name, Value: value}); err != nil {
			return err
		}
		row, err := q.GetMerchantSecret(ctx, gen.GetMerchantSecretParams{MerchantID: id.UUID(), Name: name})
		result = Secret{Name: row.Name, Value: row.Value, Version: int(row.Version)}
		return err
	})
	if err != nil {
		return Secret{}, ErrSecretBackendUnavailable
	}
	if result.Value != value {
		return Secret{}, ErrCredentialOperationConflict
	}
	return result, nil
}
