// Package merchantbinding owns explicit adoption and verification of an
// existing billing identity. Authentication-specific provisioning is an
// optional adapter; the embedded engine only reads group metadata.
package merchantbinding

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

// Validate requires canonical persisted identities, never a name lookup.
func Validate(binding billing.MerchantBinding) error {
	if binding.ID.IsZero() {
		return errors.New("merchant binding requires an existing merchant ID")
	}
	tenant, err := uuid.Parse(binding.TenantID)
	if err != nil || tenant == uuid.Nil || tenant.String() != binding.TenantID {
		return errors.New("merchant binding requires a canonical tenant UUID")
	}
	return nil
}

// ValidateGroup verifies live metadata independently of its source.
func ValidateGroup(binding billing.MerchantBinding, group billing.MerchantGroup) error {
	if err := Validate(binding); err != nil {
		return err
	}
	if group.ID != binding.ID.String() || group.TenantID != binding.TenantID || group.Persona != billing.MerchantGroupPersona || group.Deleted {
		return errors.New("merchant binding does not match a live merchant group in the configured tenant")
	}
	return nil
}

// Verify checks an already adopted row and the live group before the runtime
// installs any merchant configuration. It performs no adoption or group writes.
func Verify(ctx context.Context, q *gen.Queries, reader billing.MerchantGroupReader, slug string, binding billing.MerchantBinding) error {
	if err := Validate(binding); err != nil {
		return err
	}
	if reader == nil {
		return errors.New("explicit merchant binding requires Deps.MerchantGroups")
	}
	row, err := q.GetMerchantTenantBinding(ctx, binding.ID.UUID())
	if err != nil {
		return fmt.Errorf("read merchant binding: %w", err)
	}
	if row.Slug != slug || row.Status != "active" || row.DeletedAt != nil || row.RetiredAt != nil || row.PermissionGroupID == nil || *row.PermissionGroupID != binding.ID.String() || row.TenantID == nil || row.TenantID.String() != binding.TenantID {
		return errors.New("declared merchant binding does not match the existing live billing identity")
	}
	group, err := reader.LookupMerchantGroup(ctx, binding)
	if err != nil {
		return fmt.Errorf("read merchant permission group: %w", err)
	}
	return ValidateGroup(binding, group)
}

// Bind locks an existing merchant, lets the authentication adapter ensure its
// group in the same transaction, and records the immutable one-to-one binding.
// It never creates a billing identity, changes its name or assigns a role.
func Bind(ctx context.Context, pool *pgxpool.Pool, database config.DatabaseConfig, binding billing.MerchantBinding, ensureGroup func(context.Context, pgx.Tx) error) error {
	if err := Validate(binding); err != nil {
		return err
	}
	if pool == nil || ensureGroup == nil {
		return errors.New("merchant binding requires a pool and group provisioner")
	}
	tx, err := db.WrapPool(pool, config.SchemaName(&config.Config{Database: database})).Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := gen.New(tx)
	row, err := q.LockMerchantTenantBinding(ctx, binding.ID.UUID())
	if err != nil {
		return fmt.Errorf("lock existing merchant: %w", err)
	}
	if row.Status != "active" || row.DeletedAt != nil || row.RetiredAt != nil {
		return errors.New("merchant binding requires a live, unretired billing identity")
	}
	if conflictingBinding(binding, row.PermissionGroupID, row.TenantID) {
		return errors.New("merchant already has a conflicting binding")
	}
	if err := ensureGroup(ctx, tx); err != nil {
		return fmt.Errorf("ensure merchant group: %w", err)
	}
	if row.TenantID == nil {
		changed, err := q.BindMerchantTenant(ctx, gen.BindMerchantTenantParams{ID: binding.ID.UUID(), TenantID: uuid.MustParse(binding.TenantID)})
		if err != nil {
			return fmt.Errorf("record merchant binding: %w", err)
		}
		if changed != 1 {
			return errors.New("merchant binding changed concurrently")
		}
	}
	return tx.Commit(ctx)
}

// A recorded tenant always requires its exact merchant group, even if legacy
// data was written before the database constraint existed.
func conflictingBinding(binding billing.MerchantBinding, group *string, tenant *uuid.UUID) bool {
	return group != nil && *group != binding.ID.String() ||
		tenant != nil && (group == nil || tenant.String() != binding.TenantID)
}
