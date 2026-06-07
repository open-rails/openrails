package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// ES-1: Revoked without reason
type CheckRevokedWithoutReason struct{}

func (c *CheckRevokedWithoutReason) ID() string         { return "ES-1" }
func (c *CheckRevokedWithoutReason) Name() string       { return "revoked_without_reason" }
func (c *CheckRevokedWithoutReason) Category() string   { return "entitlement_state" }
func (c *CheckRevokedWithoutReason) Severity() Severity { return SeverityLow }

func (c *CheckRevokedWithoutReason) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	type result struct {
		ID          uuid.UUID `bun:"id"`
		UserID      string    `bun:"user_id"`
		Entitlement string    `bun:"entitlement"`
		RevokedAt   time.Time `bun:"revoked_at"`
	}

	var results []result
	err := db.NewRaw(`
		SELECT id, tenant_subject_id::text AS user_id, entitlement, revoked_at
		FROM billing.entitlements
		WHERE revoked_at IS NOT NULL
		  AND revoke_reason IS NULL
		  AND deleted_at IS NULL
	`).Scan(ctx, &results)

	if err != nil {
		return nil, fmt.Errorf("query revoked without reason: %w", err)
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
			EntityID:       r.ID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Entitlement '%s' was revoked at %s but has no revoke_reason", r.Entitlement, r.RevokedAt.Format(time.RFC3339)),
			Recommendation: "Set revoke_reason='unknown'",
			AutoFixable:    true,
			Details: map[string]any{
				"revoked_at": r.RevokedAt,
			},
		})
	}

	return findings, nil
}

// ES-2: Reason without revocation
type CheckReasonWithoutRevocation struct{}

func (c *CheckReasonWithoutRevocation) ID() string         { return "ES-2" }
func (c *CheckReasonWithoutRevocation) Name() string       { return "reason_without_revocation" }
func (c *CheckReasonWithoutRevocation) Category() string   { return "entitlement_state" }
func (c *CheckReasonWithoutRevocation) Severity() Severity { return SeverityLow }

func (c *CheckReasonWithoutRevocation) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	type result struct {
		ID           uuid.UUID `bun:"id"`
		UserID       string    `bun:"user_id"`
		Entitlement  string    `bun:"entitlement"`
		RevokeReason string    `bun:"revoke_reason"`
	}

	var results []result
	err := db.NewRaw(`
		SELECT id, tenant_subject_id::text AS user_id, entitlement, revoke_reason
		FROM billing.entitlements
		WHERE revoke_reason IS NOT NULL
		  AND revoked_at IS NULL
		  AND deleted_at IS NULL
	`).Scan(ctx, &results)

	if err != nil {
		return nil, fmt.Errorf("query reason without revocation: %w", err)
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
			EntityID:       r.ID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Entitlement '%s' has revoke_reason='%s' but revoked_at IS NULL", r.Entitlement, r.RevokeReason),
			Recommendation: "Set revoked_at=NOW() or clear revoke_reason",
			AutoFixable:    true,
			Details: map[string]any{
				"revoke_reason": r.RevokeReason,
			},
		})
	}

	return findings, nil
}

// ES-3: Invalid time window
type CheckInvalidTimeWindow struct{}

func (c *CheckInvalidTimeWindow) ID() string         { return "ES-3" }
func (c *CheckInvalidTimeWindow) Name() string       { return "invalid_time_window" }
func (c *CheckInvalidTimeWindow) Category() string   { return "entitlement_state" }
func (c *CheckInvalidTimeWindow) Severity() Severity { return SeverityHigh }

func (c *CheckInvalidTimeWindow) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	type result struct {
		ID          uuid.UUID `bun:"id"`
		UserID      string    `bun:"user_id"`
		Entitlement string    `bun:"entitlement"`
		StartAt     time.Time `bun:"start_at"`
		EndAt       time.Time `bun:"end_at"`
	}

	var results []result
	err := db.NewRaw(`
		SELECT id, tenant_subject_id::text AS user_id, entitlement, start_at, end_at
		FROM billing.entitlements
		WHERE end_at IS NOT NULL
		  AND start_at >= end_at
		  AND deleted_at IS NULL
	`).Scan(ctx, &results)

	if err != nil {
		return nil, fmt.Errorf("query invalid time window: %w", err)
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
			EntityID:       r.ID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Entitlement '%s' has start_at (%s) >= end_at (%s)", r.Entitlement, r.StartAt.Format(time.RFC3339), r.EndAt.Format(time.RFC3339)),
			Recommendation: "MANUAL REVIEW - Data corruption detected",
			AutoFixable:    false,
			Details: map[string]any{
				"start_at": r.StartAt,
				"end_at":   r.EndAt,
			},
		})
	}

	return findings, nil
}

// ES-5: Multiple indefinite entitlements
type CheckMultipleIndefiniteEntitlements struct{}

func (c *CheckMultipleIndefiniteEntitlements) ID() string { return "ES-5" }
func (c *CheckMultipleIndefiniteEntitlements) Name() string {
	return "multiple_indefinite_entitlements"
}
func (c *CheckMultipleIndefiniteEntitlements) Category() string   { return "entitlement_state" }
func (c *CheckMultipleIndefiniteEntitlements) Severity() Severity { return SeverityMedium }

func (c *CheckMultipleIndefiniteEntitlements) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	type result struct {
		UserID         string      `bun:"user_id"`
		Entitlement    string      `bun:"entitlement"`
		Count          int         `bun:"count"`
		EntitlementIDs []uuid.UUID `bun:"ent_ids,array"`
	}

	var results []result
	err := db.NewRaw(`
		SELECT
			tenant_subject_id::text AS user_id,
			entitlement,
			COUNT(*) as count,
			ARRAY_AGG(id ORDER BY created_at DESC) as ent_ids
		FROM billing.entitlements
		WHERE end_at IS NULL
		  AND revoked_at IS NULL
		  AND deleted_at IS NULL
		GROUP BY tenant_subject_id, entitlement
		HAVING COUNT(*) > 1
	`).Scan(ctx, &results)

	if err != nil {
		return nil, fmt.Errorf("query multiple indefinite entitlements: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		// Use the first (most recent) entitlement ID as the entity
		entityID := r.EntitlementIDs[0]

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntityEntitlement,
			EntityID:       entityID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("User has %d indefinite entitlements for '%s'", r.Count, r.Entitlement),
			Recommendation: "Revoke all but the most recent",
			AutoFixable:    true,
			Details: map[string]any{
				"entitlement_ids": r.EntitlementIDs,
				"entitlement":     r.Entitlement,
			},
		})
	}

	return findings, nil
}
