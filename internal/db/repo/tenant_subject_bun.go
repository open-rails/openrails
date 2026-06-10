package repo

// TRANSITION SHIMS (#334): bun-handle variants of the tenant-subject and
// entitlement-timeline helpers, for call sites that still run inside bun
// transactions or on bun handles. Each caller migrates to the pgx/sqlc
// versions as its module converts; this file is deleted in Phase 2.

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/tenant"
)

// EnsureTenantSubjectIDBun is the bun-era EnsureTenantSubjectID, kept for
// unconverted bun-tx call sites.
func EnsureTenantSubjectIDBun(ctx context.Context, db bun.IDB, tenantID uuid.UUID, userID string) (uuid.UUID, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return uuid.Nil, nil
	}
	if tenantID == uuid.Nil {
		tenantID = tenant.FromContextOrDefault(ctx).UUID()
	}
	if uid, perr := uuid.Parse(userID); perr == nil {
		var id uuid.UUID
		err := db.NewRaw(
			`INSERT INTO billing.tenant_subjects (id, tenant_id, issuer, subject)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT (id) DO UPDATE SET last_seen_at = now()
			 RETURNING id`,
			uid, tenantID, SelfIssuer, userID,
		).Scan(ctx, &id)
		return id, err
	}
	var id uuid.UUID
	err := db.NewRaw(
		`INSERT INTO billing.tenant_subjects (tenant_id, issuer, subject)
		 VALUES (?, ?, ?)
		 ON CONFLICT (tenant_id, issuer, subject) DO UPDATE SET last_seen_at = now()
		 RETURNING id`,
		tenantID, LegacyUserIssuer, userID,
	).Scan(ctx, &id)
	return id, err
}

// ResolveTenantSubjectIDBun is the bun-era ResolveTenantSubjectID.
func ResolveTenantSubjectIDBun(ctx context.Context, db bun.IDB, tenantID uuid.UUID, userID string) (uuid.UUID, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return uuid.Nil, nil
	}
	if uid, perr := uuid.Parse(userID); perr == nil {
		return uid, nil
	}
	if tenantID == uuid.Nil {
		tenantID = tenant.FromContextOrDefault(ctx).UUID()
	}
	var id uuid.UUID
	err := db.NewRaw(
		`SELECT id FROM billing.tenant_subjects
		  WHERE tenant_id = ? AND issuer = ? AND subject = ?`,
		tenantID, LegacyUserIssuer, userID,
	).Scan(ctx, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, nil
	}
	return id, err
}

// LockEntitlementTimelineBun is the bun-tx variant of LockEntitlementTimeline.
func LockEntitlementTimelineBun(ctx context.Context, tx bun.Tx, userID, entitlement string) error {
	if userID == "" || entitlement == "" {
		return errors.New("userID and entitlement are required for entitlement timeline lock")
	}
	_, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(?)", entitlementTimelineLockKey(userID, entitlement))
	return err
}

// GetEntitlementByIDTxBun is the bun-tx variant of GetEntitlementByIDTx.
func GetEntitlementByIDTxBun(ctx context.Context, tx bun.Tx, id uuid.UUID) (*models.Entitlement, error) {
	ent := new(models.Entitlement)
	if err := tx.NewSelect().
		Model(ent).
		Where("ent.id = ?", id).
		Limit(1).
		Scan(ctx); err != nil {
		return nil, err
	}
	return ent, nil
}
