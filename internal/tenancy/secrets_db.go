package tenancy

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/pkg/tenant"
)

// dbSecretStore persists per-tenant secrets in billing.tenant_secrets (migration
// 041). This is the self-hosted / dev default: it builds and runs WITHOUT a live
// Vault. A managed deployment swaps in the Vault-backed store (secrets_vault.go)
// with the same (tenant, name) addressing and no schema change.
type dbSecretStore struct {
	pool *pgxpool.Pool
}

// NewDBSecretStore returns a database-backed TenantSecretStore over the given
// pgx pool (the pool that holds the billing.* schema).
func NewDBSecretStore(pool *pgxpool.Pool) (TenantSecretStore, error) {
	if pool == nil {
		return nil, errors.New("tenancy: pgx pool is required for the DB-backed secret store")
	}
	return &dbSecretStore{pool: pool}, nil
}

func (d *dbSecretStore) Get(ctx context.Context, tenantID tenant.ID, name string) (Secret, error) {
	if err := validateSecretRef(tenantID, name); err != nil {
		return Secret{}, err
	}
	var s Secret
	err := d.pool.QueryRow(ctx, `
		SELECT name, value, version
		  FROM billing.tenant_secrets
		 WHERE tenant_id = $1::uuid AND name = $2
	`, tenantID.String(), name).Scan(&s.Name, &s.Value, &s.Version)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Secret{}, ErrSecretNotFound
		}
		// A query/transport failure is operational, not "secret absent" — callers
		// must retry, not treat as missing.
		return Secret{}, fmt.Errorf("tenancy: get tenant secret: %w", errors.Join(ErrSecretBackendUnavailable, err))
	}
	return s, nil
}

func (d *dbSecretStore) Put(ctx context.Context, tenantID tenant.ID, name, value string) (Secret, error) {
	if err := validateSecretRef(tenantID, name); err != nil {
		return Secret{}, err
	}
	// Idempotent rotation: insert version 1, or bump version ONLY when the value
	// actually changes (re-putting the same value is a no-op rotation).
	var s Secret
	err := d.pool.QueryRow(ctx, `
		INSERT INTO billing.tenant_secrets (tenant_id, name, value, version)
		VALUES ($1::uuid, $2, $3, 1)
		ON CONFLICT (tenant_id, name) DO UPDATE
		   SET value      = EXCLUDED.value,
		       version    = CASE WHEN billing.tenant_secrets.value = EXCLUDED.value
		                         THEN billing.tenant_secrets.version
		                         ELSE billing.tenant_secrets.version + 1 END,
		       updated_at = current_timestamp
		RETURNING name, value, version
	`, tenantID.String(), name, value).Scan(&s.Name, &s.Value, &s.Version)
	if err != nil {
		return Secret{}, fmt.Errorf("tenancy: put tenant secret: %w", err)
	}
	return s, nil
}

func (d *dbSecretStore) Delete(ctx context.Context, tenantID tenant.ID, name string) error {
	if err := validateSecretRef(tenantID, name); err != nil {
		return err
	}
	_, err := d.pool.Exec(ctx, `
		DELETE FROM billing.tenant_secrets WHERE tenant_id = $1::uuid AND name = $2
	`, tenantID.String(), name)
	if err != nil {
		return fmt.Errorf("tenancy: delete tenant secret: %w", err)
	}
	return nil
}

func (d *dbSecretStore) List(ctx context.Context, tenantID tenant.ID) ([]string, error) {
	if tenantID.IsZero() {
		return nil, validateSecretRef(tenantID, "x")
	}
	rows, err := d.pool.Query(ctx, `
		SELECT name FROM billing.tenant_secrets WHERE tenant_id = $1::uuid ORDER BY name
	`, tenantID.String())
	if err != nil {
		return nil, fmt.Errorf("tenancy: list tenant secrets: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("tenancy: scan tenant secret name: %w", err)
		}
		names = append(names, n)
	}
	return names, rows.Err()
}
