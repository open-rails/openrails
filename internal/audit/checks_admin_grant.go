package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/open-rails/openrails/internal/db/gen"
)

// NOTE (schema drift, found during the sqlc migration): the bun-era AG checks
// referenced admin_grants.entitlement / granted_at / expires_at / revoked_at,
// none of which exist in billing.admin_grants (it has price_id, granted_by,
// reason, duration_days, created_at). The old AG-1/AG-3/AG-4 SQL could never
// have executed. AG-1 and AG-4 are adapted to the real schema (grant expiry is
// derived from created_at + duration_days); AG-3 (grant-level revocation) has
// no equivalent column and was removed.

// AG-1: Admin grant without corresponding entitlement
type CheckAdminGrantMissingEntitlements struct{}

func (c *CheckAdminGrantMissingEntitlements) ID() string         { return "AG-1" }
func (c *CheckAdminGrantMissingEntitlements) Name() string       { return "admin_grant_missing_entitlements" }
func (c *CheckAdminGrantMissingEntitlements) Category() string   { return "admin_grant" }
func (c *CheckAdminGrantMissingEntitlements) Severity() Severity { return SeverityHigh }

func (c *CheckAdminGrantMissingEntitlements) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	results, err := q.AuditAdminGrantMissingEntitlements(ctx)
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
			Description:    fmt.Sprintf("Admin grant for '%s' has no corresponding entitlement", r.Reason),
			Recommendation: "Create entitlement record for this admin grant",
			AutoFixable:    true,
			Details: map[string]any{
				"reason":        r.Reason,
				"granted_at":    r.GrantedAt,
				"duration_days": r.DurationDays,
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

func (c *CheckOrphanAdminEntitlements) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	results, err := q.AuditOrphanAdminEntitlements(ctx)
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

// AG-4: Expired admin grant with active entitlement
type CheckExpiredAdminGrantActiveEntitlement struct{}

func (c *CheckExpiredAdminGrantActiveEntitlement) ID() string { return "AG-4" }
func (c *CheckExpiredAdminGrantActiveEntitlement) Name() string {
	return "expired_admin_grant_active_entitlement"
}
func (c *CheckExpiredAdminGrantActiveEntitlement) Category() string   { return "admin_grant" }
func (c *CheckExpiredAdminGrantActiveEntitlement) Severity() Severity { return SeverityHigh }

func (c *CheckExpiredAdminGrantActiveEntitlement) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	results, err := q.AuditExpiredAdminGrantActiveEntitlement(ctx)
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
