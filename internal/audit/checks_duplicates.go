package audit

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// D-1: Multiple active subscriptions for same user
type CheckMultipleActiveSubscriptions struct{}

func (c *CheckMultipleActiveSubscriptions) ID() string         { return "D-1" }
func (c *CheckMultipleActiveSubscriptions) Name() string       { return "multiple_active_subscriptions" }
func (c *CheckMultipleActiveSubscriptions) Category() string   { return "duplicates" }
func (c *CheckMultipleActiveSubscriptions) Severity() Severity { return SeverityCritical }

func (c *CheckMultipleActiveSubscriptions) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	results, err := q.AuditMultipleActiveSubscriptions(ctx)
	if err != nil {
		return nil, fmt.Errorf("query multiple active subscriptions: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		// Use the most recent subscription ID as the entity
		entityID := r.SubIds[0]

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntitySubscription,
			EntityID:       entityID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("User has %d active subscriptions", r.Count),
			Recommendation: "MANUAL REVIEW - Cancel all but the most recent subscription",
			AutoFixable:    false,
			Details: map[string]any{
				"subscription_ids": r.SubIds,
				"count":            r.Count,
			},
		})
	}

	return findings, nil
}

// D-2: Duplicate charges in same billing period
type CheckDuplicateChargesSamePeriod struct{}

func (c *CheckDuplicateChargesSamePeriod) ID() string         { return "D-2" }
func (c *CheckDuplicateChargesSamePeriod) Name() string       { return "duplicate_charges_same_period" }
func (c *CheckDuplicateChargesSamePeriod) Category() string   { return "duplicates" }
func (c *CheckDuplicateChargesSamePeriod) Severity() Severity { return SeverityCritical }

func (c *CheckDuplicateChargesSamePeriod) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	// Find payments for same user, same product, within same calendar month
	results, err := q.AuditDuplicateChargesSamePeriod(ctx)
	if err != nil {
		return nil, fmt.Errorf("query duplicate charges: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		// Use the most recent payment ID as the entity
		entityID := r.PaymentIds[0]

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntityPayment,
			EntityID:       entityID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("User charged %d times for product '%s' in same period (total %s)", r.Count, r.ProductSlug, moneyutil.FormatUSD(r.TotalAmount)),
			Recommendation: "MANUAL REVIEW - Refund duplicate charges",
			AutoFixable:    false,
			Details: map[string]any{
				"payment_ids":  r.PaymentIds,
				"product_id":   r.ProductID,
				"product_slug": r.ProductSlug,
				"count":        r.Count,
				"total_amount": r.TotalAmount,
				"first_date":   r.FirstDate,
				"last_date":    r.LastDate,
			},
		})
	}

	return findings, nil
}

// D-3: Overlapping entitlement windows
type CheckOverlappingEntitlementWindows struct{}

func (c *CheckOverlappingEntitlementWindows) ID() string         { return "D-3" }
func (c *CheckOverlappingEntitlementWindows) Name() string       { return "overlapping_entitlement_windows" }
func (c *CheckOverlappingEntitlementWindows) Category() string   { return "duplicates" }
func (c *CheckOverlappingEntitlementWindows) Severity() Severity { return SeverityMedium }

func (c *CheckOverlappingEntitlementWindows) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	// Find overlapping entitlements for same user and entitlement name
	results, err := q.AuditOverlappingEntitlementWindows(ctx)
	if err != nil {
		return nil, fmt.Errorf("query overlapping entitlements: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		// Use the first entitlement ID as the entity
		entityID := r.EntIds[0]

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntityEntitlement,
			EntityID:       entityID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("User has %d overlapping windows for entitlement '%s'", r.Count, r.Entitlement),
			Recommendation: "Merge or revoke duplicate entitlement windows",
			AutoFixable:    true,
			Details: map[string]any{
				"entitlement_ids": r.EntIds,
				"entitlement":     r.Entitlement,
			},
		})
	}

	return findings, nil
}
