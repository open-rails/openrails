package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/open-rails/openrails/internal/db/models"
)

// AG-1: Admin grant without corresponding entitlement
type CheckAdminGrantMissingEntitlements struct{}

func (c *CheckAdminGrantMissingEntitlements) ID() string         { return "AG-1" }
func (c *CheckAdminGrantMissingEntitlements) Name() string       { return "admin_grant_missing_entitlements" }
func (c *CheckAdminGrantMissingEntitlements) Category() string   { return "admin_grant" }
func (c *CheckAdminGrantMissingEntitlements) Severity() Severity { return SeverityHigh }

func (c *CheckAdminGrantMissingEntitlements) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	type result struct {
		GrantID     uuid.UUID  `bun:"grant_id"`
		UserID      string     `bun:"user_id"`
		Entitlement string     `bun:"entitlement"`
		GrantedAt   time.Time  `bun:"granted_at"`
		ExpiresAt   *time.Time `bun:"expires_at"`
		RevokedAt   *time.Time `bun:"revoked_at"`
	}

	var results []result
	err := db.NewRaw(`
		SELECT
			ag.id as grant_id,
			ag.tenant_subject_id::text AS user_id,
			ag.entitlement,
			ag.granted_at,
			ag.expires_at,
			ag.revoked_at
		FROM billing.admin_grants ag
		LEFT JOIN billing.entitlements ent ON
			ag.tenant_subject_id = ent.tenant_subject_id
			AND ag.entitlement = ent.entitlement
			AND ent.source_type = ?
			AND ent.source_id = ag.id
			AND ent.deleted_at IS NULL
		WHERE ag.revoked_at IS NULL
		  AND (ag.expires_at IS NULL OR ag.expires_at > NOW())
		  AND ent.id IS NULL
	`, models.EntitlementSourceAdmin).Scan(ctx, &results)

	if err != nil {
		return nil, fmt.Errorf("query admin grant missing entitlements: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntityAdminGrant,
			EntityID:       r.GrantID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Admin grant for '%s' has no corresponding entitlement", r.Entitlement),
			Recommendation: "Create entitlement record for this admin grant",
			AutoFixable:    true,
			Details: map[string]any{
				"entitlement": r.Entitlement,
				"granted_at":  r.GrantedAt,
				"expires_at":  r.ExpiresAt,
			},
		})
	}

	return findings, nil
}

// AG-2: Orphan admin entitlement (entitlement exists but admin grant doesn't)
type CheckOrphanAdminEntitlements struct{}

func (c *CheckOrphanAdminEntitlements) ID() string         { return "AG-2" }
func (c *CheckOrphanAdminEntitlements) Name() string       { return "orphan_admin_entitlements" }
func (c *CheckOrphanAdminEntitlements) Category() string   { return "admin_grant" }
func (c *CheckOrphanAdminEntitlements) Severity() Severity { return SeverityMedium }

func (c *CheckOrphanAdminEntitlements) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	type result struct {
		EntID       uuid.UUID  `bun:"ent_id"`
		UserID      string     `bun:"user_id"`
		Entitlement string     `bun:"entitlement"`
		SourceID    *uuid.UUID `bun:"source_id"`
	}

	var results []result
	err := db.NewRaw(`
		SELECT
			ent.id as ent_id,
			ent.tenant_subject_id::text AS user_id,
			ent.entitlement,
			ent.source_id
		FROM billing.entitlements ent
		LEFT JOIN billing.admin_grants ag ON ent.source_id = ag.id
		WHERE ent.source_type = ?
		  AND ent.revoked_at IS NULL
		  AND ent.deleted_at IS NULL
		  AND ag.id IS NULL
	`, models.EntitlementSourceAdmin).Scan(ctx, &results)

	if err != nil {
		return nil, fmt.Errorf("query orphan admin entitlements: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntityEntitlement,
			EntityID:       r.EntID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Admin entitlement '%s' references non-existent admin_grant %v", r.Entitlement, r.SourceID),
			Recommendation: "Revoke orphan entitlement or recreate admin grant",
			AutoFixable:    false,
			Details: map[string]any{
				"entitlement":            r.Entitlement,
				"missing_admin_grant_id": r.SourceID,
			},
		})
	}

	return findings, nil
}

// AG-3: Admin grant revoked but entitlement still active
type CheckRevokedAdminGrantActiveEntitlement struct{}

func (c *CheckRevokedAdminGrantActiveEntitlement) ID() string { return "AG-3" }
func (c *CheckRevokedAdminGrantActiveEntitlement) Name() string {
	return "revoked_admin_grant_active_entitlement"
}
func (c *CheckRevokedAdminGrantActiveEntitlement) Category() string   { return "admin_grant" }
func (c *CheckRevokedAdminGrantActiveEntitlement) Severity() Severity { return SeverityHigh }

func (c *CheckRevokedAdminGrantActiveEntitlement) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	type result struct {
		EntID       uuid.UUID `bun:"ent_id"`
		UserID      string    `bun:"user_id"`
		Entitlement string    `bun:"entitlement"`
		GrantID     uuid.UUID `bun:"grant_id"`
		RevokedAt   time.Time `bun:"revoked_at"`
	}

	var results []result
	err := db.NewRaw(`
		SELECT
			ent.id as ent_id,
			ent.tenant_subject_id::text AS user_id,
			ent.entitlement,
			ag.id as grant_id,
			ag.revoked_at
		FROM billing.entitlements ent
		JOIN billing.admin_grants ag ON ent.source_id = ag.id
		WHERE ent.source_type = ?
		  AND ent.revoked_at IS NULL
		  AND ent.deleted_at IS NULL
		  AND ag.revoked_at IS NOT NULL
	`, models.EntitlementSourceAdmin).Scan(ctx, &results)

	if err != nil {
		return nil, fmt.Errorf("query revoked admin grant active entitlement: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntityEntitlement,
			EntityID:       r.EntID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Entitlement '%s' is still active but admin grant was revoked at %s", r.Entitlement, r.RevokedAt.Format(time.RFC3339)),
			Recommendation: "Revoke entitlement to match admin grant state",
			AutoFixable:    true,
			Details: map[string]any{
				"entitlement":      r.Entitlement,
				"admin_grant_id":   r.GrantID,
				"grant_revoked_at": r.RevokedAt,
			},
		})
	}

	return findings, nil
}

// AG-4: Expired admin grant with active entitlement
type CheckExpiredAdminGrantActiveEntitlement struct{}

func (c *CheckExpiredAdminGrantActiveEntitlement) ID() string { return "AG-4" }
func (c *CheckExpiredAdminGrantActiveEntitlement) Name() string {
	return "expired_admin_grant_active_entitlement"
}
func (c *CheckExpiredAdminGrantActiveEntitlement) Category() string   { return "admin_grant" }
func (c *CheckExpiredAdminGrantActiveEntitlement) Severity() Severity { return SeverityHigh }

func (c *CheckExpiredAdminGrantActiveEntitlement) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	type result struct {
		EntID       uuid.UUID `bun:"ent_id"`
		UserID      string    `bun:"user_id"`
		Entitlement string    `bun:"entitlement"`
		GrantID     uuid.UUID `bun:"grant_id"`
		ExpiresAt   time.Time `bun:"expires_at"`
	}

	var results []result
	err := db.NewRaw(`
		SELECT
			ent.id as ent_id,
			ent.tenant_subject_id::text AS user_id,
			ent.entitlement,
			ag.id as grant_id,
			ag.expires_at
		FROM billing.entitlements ent
		JOIN billing.admin_grants ag ON ent.source_id = ag.id
		WHERE ent.source_type = ?
		  AND ent.revoked_at IS NULL
		  AND ent.deleted_at IS NULL
		  AND ag.revoked_at IS NULL
		  AND ag.expires_at IS NOT NULL
		  AND ag.expires_at < NOW()
	`, models.EntitlementSourceAdmin).Scan(ctx, &results)

	if err != nil {
		return nil, fmt.Errorf("query expired admin grant active entitlement: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntityEntitlement,
			EntityID:       r.EntID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Entitlement '%s' is still active but admin grant expired at %s", r.Entitlement, r.ExpiresAt.Format(time.RFC3339)),
			Recommendation: "Revoke entitlement as admin grant has expired",
			AutoFixable:    true,
			Details: map[string]any{
				"entitlement":      r.Entitlement,
				"admin_grant_id":   r.GrantID,
				"grant_expires_at": r.ExpiresAt,
			},
		})
	}

	return findings, nil
}
