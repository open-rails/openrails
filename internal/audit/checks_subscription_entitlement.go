package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/repo"
)

// S-E-1: Active subscription missing entitlements
type CheckActiveSubscriptionMissingEntitlements struct{}

func (c *CheckActiveSubscriptionMissingEntitlements) ID() string { return "S-E-1" }
func (c *CheckActiveSubscriptionMissingEntitlements) Name() string {
	return "active_subscription_missing_entitlements"
}
func (c *CheckActiveSubscriptionMissingEntitlements) Category() string {
	return "subscription_entitlement"
}
func (c *CheckActiveSubscriptionMissingEntitlements) Severity() Severity { return SeverityHigh }

func (c *CheckActiveSubscriptionMissingEntitlements) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	// Get all active subscriptions with their product
	var params gen.AuditActiveSubscriptionsWithSpecParams
	if opts.UserID != "" {
		tsid, err := repo.ResolveTenantSubjectID(opts.UserID)
		if err != nil {
			return nil, err
		}
		params.TenantSubjectID = &tsid
	}
	if opts.Since != nil {
		params.Since = opts.Since
	}

	subs, err := q.AuditActiveSubscriptionsWithSpec(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("query active subscriptions: %w", err)
	}

	for _, sub := range subs {
		if len(sub.EntitlementsSpec) == 0 {
			continue
		}
		var spec map[string]*int
		if err := json.Unmarshal(sub.EntitlementsSpec, &spec); err != nil {
			return nil, fmt.Errorf("unmarshal entitlements_spec for product %s: %w", sub.ProductID, err)
		}
		if spec == nil {
			continue
		}

		// Check each entitlement in the product spec
		for entName := range spec {
			// Check if user has this entitlement from this subscription
			count, err := q.AuditActiveSubscriptionEntitlementCount(ctx, gen.AuditActiveSubscriptionEntitlementCountParams{
				TenantSubjectID: sub.TenantSubjectID,
				Entitlement:     entName,
				SourceID:        sub.ID,
				Now:             time.Now(),
			})
			if err != nil {
				return nil, fmt.Errorf("check entitlement %s for sub %s: %w", entName, sub.ID, err)
			}

			if count == 0 {
				findings = append(findings, Finding{
					CheckID:        c.ID(),
					CheckName:      c.Name(),
					Severity:       c.Severity(),
					EntityType:     EntitySubscription,
					EntityID:       sub.ID,
					UserID:         sub.TenantSubjectID.String(),
					Description:    fmt.Sprintf("Active subscription missing entitlement '%s' from product '%s'", entName, sub.ProductSlug),
					Recommendation: fmt.Sprintf("Grant entitlement '%s' with source_type=subscription, source_id=%s", entName, sub.ID),
					AutoFixable:    true,
					Details: map[string]any{
						"missing_entitlement": entName,
						"product_slug":        sub.ProductSlug,
						"product_id":          sub.ProductID,
					},
				})
			}
		}
	}

	return findings, nil
}

// S-E-2: Orphan subscription entitlements
type CheckOrphanSubscriptionEntitlements struct{}

func (c *CheckOrphanSubscriptionEntitlements) ID() string { return "S-E-2" }
func (c *CheckOrphanSubscriptionEntitlements) Name() string {
	return "orphan_subscription_entitlements"
}
func (c *CheckOrphanSubscriptionEntitlements) Category() string   { return "subscription_entitlement" }
func (c *CheckOrphanSubscriptionEntitlements) Severity() Severity { return SeverityHigh }

func (c *CheckOrphanSubscriptionEntitlements) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	// Find entitlements where source_type=subscription but subscription is not active/pending/past_due or doesn't exist
	results, err := q.AuditOrphanSubscriptionEntitlements(ctx)
	if err != nil {
		return nil, fmt.Errorf("query orphan entitlements: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		status := "missing"
		if r.SubStatus != nil {
			status = *r.SubStatus
		}

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntityEntitlement,
			EntityID:       r.EntitlementID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Entitlement '%s' references subscription %s which is %s", r.Entitlement, r.SourceID, status),
			Recommendation: "Revoke entitlement with reason='orphan_cleanup'",
			AutoFixable:    true,
			Details: map[string]any{
				"subscription_id":     r.SourceID,
				"subscription_status": status,
			},
		})
	}

	return findings, nil
}

// S-E-3: Cancelled subscription with active entitlements (when revoke_access=true)
type CheckCancelledSubscriptionActiveEntitlements struct{}

func (c *CheckCancelledSubscriptionActiveEntitlements) ID() string { return "S-E-3" }
func (c *CheckCancelledSubscriptionActiveEntitlements) Name() string {
	return "cancelled_subscription_active_entitlements"
}
func (c *CheckCancelledSubscriptionActiveEntitlements) Category() string {
	return "subscription_entitlement"
}
func (c *CheckCancelledSubscriptionActiveEntitlements) Severity() Severity { return SeverityHigh }

func (c *CheckCancelledSubscriptionActiveEntitlements) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	// Find cancelled subscriptions with ended_at set (immediate revoke) but entitlements still active
	results, err := q.AuditCancelledSubscriptionActiveEntitlements(ctx)
	if err != nil {
		return nil, fmt.Errorf("query cancelled subscription entitlements: %w", err)
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
			Description:    fmt.Sprintf("Subscription %s was cancelled with immediate revoke (ended_at=%s) but entitlement '%s' is still active", r.SubID, r.EndedAt.Format(time.RFC3339), r.Entitlement),
			Recommendation: "Revoke entitlement immediately",
			AutoFixable:    true,
			Details: map[string]any{
				"subscription_id": r.SubID,
				"ended_at":        r.EndedAt,
			},
		})
	}

	return findings, nil
}

// S-E-4: Wrong entitlement end date for period-end cancellation
type CheckWrongEntitlementEndDate struct{}

func (c *CheckWrongEntitlementEndDate) ID() string         { return "S-E-4" }
func (c *CheckWrongEntitlementEndDate) Name() string       { return "wrong_entitlement_end_date" }
func (c *CheckWrongEntitlementEndDate) Category() string   { return "subscription_entitlement" }
func (c *CheckWrongEntitlementEndDate) Severity() Severity { return SeverityMedium }

func (c *CheckWrongEntitlementEndDate) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	// Find cancelled subscriptions without ended_at (period-end cancel) where entitlement end_at != period_ends_at
	results, err := q.AuditWrongEntitlementEndDate(ctx)
	if err != nil {
		return nil, fmt.Errorf("query wrong end dates: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		entEndStr := "NULL"
		if r.EntEndAt != nil {
			entEndStr = r.EntEndAt.Format(time.RFC3339)
		}

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntityEntitlement,
			EntityID:       r.EntID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Entitlement '%s' end_at (%s) doesn't match subscription period end (%s)", r.Entitlement, entEndStr, r.PeriodEndsAt.Format(time.RFC3339)),
			Recommendation: fmt.Sprintf("Update entitlement.end_at to %s", r.PeriodEndsAt.Format(time.RFC3339)),
			AutoFixable:    true,
			Details: map[string]any{
				"subscription_id": r.SubID,
				"expected_end_at": r.PeriodEndsAt,
				"actual_end_at":   r.EntEndAt,
			},
		})
	}

	return findings, nil
}

// S-E-5: Entitlement source mismatch (different user or missing subscription)
type CheckEntitlementSourceMismatch struct{}

func (c *CheckEntitlementSourceMismatch) ID() string         { return "S-E-5" }
func (c *CheckEntitlementSourceMismatch) Name() string       { return "entitlement_source_mismatch" }
func (c *CheckEntitlementSourceMismatch) Category() string   { return "subscription_entitlement" }
func (c *CheckEntitlementSourceMismatch) Severity() Severity { return SeverityHigh }

func (c *CheckEntitlementSourceMismatch) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	// Find entitlements where subscription exists but belongs to different user
	results, err := q.AuditEntitlementSourceMismatch(ctx)
	if err != nil {
		return nil, fmt.Errorf("query source mismatches: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.EntUserID != opts.UserID {
			continue
		}

		desc := fmt.Sprintf("Entitlement '%s' references subscription %s which doesn't exist", r.Entitlement, r.SourceID)
		if r.SubUserID != nil {
			desc = fmt.Sprintf("Entitlement '%s' for user %s references subscription %s which belongs to user %s", r.Entitlement, r.EntUserID, r.SourceID, *r.SubUserID)
		}

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntityEntitlement,
			EntityID:       r.EntID,
			UserID:         r.EntUserID,
			Description:    desc,
			Recommendation: "MANUAL REVIEW - Data corruption detected",
			AutoFixable:    false,
			Details: map[string]any{
				"source_id":         r.SourceID,
				"subscription_user": r.SubUserID,
			},
		})
	}

	return findings, nil
}
