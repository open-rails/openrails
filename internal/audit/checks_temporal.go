package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/open-rails/openrails/internal/db/gen"
)

// T-1: Stale pending subscription (pending but should have started)
type CheckStalePendingSubscription struct{}

func (c *CheckStalePendingSubscription) ID() string         { return "T-1" }
func (c *CheckStalePendingSubscription) Name() string       { return "stale_pending_subscription" }
func (c *CheckStalePendingSubscription) Category() string   { return "temporal" }
func (c *CheckStalePendingSubscription) Severity() Severity { return SeverityHigh }

func (c *CheckStalePendingSubscription) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	// Only flag pending subscriptions where:
	// 1. The period start date has passed (should have started)
	// 2. It's been pending for more than 24 hours after intended start
	results, err := q.AuditStalePendingSubscription(ctx)
	if err != nil {
		return nil, fmt.Errorf("query stale pending subscriptions: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		hoursOverdue := time.Since(r.CurrentPeriodStartsAt).Hours()

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntitySubscription,
			EntityID:       r.ID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Subscription is pending but period should have started %.0f hours ago", hoursOverdue),
			Recommendation: "Process the pending subscription or cancel if payment failed",
			AutoFixable:    false,
			Details: map[string]any{
				"current_period_starts_at": r.CurrentPeriodStartsAt,
				"created_at":               r.CreatedAt,
				"hours_overdue":            hoursOverdue,
			},
		})
	}

	return findings, nil
}

// T-2: Stale past_due subscription (exceeded max retries but not cancelled)
type CheckStalePastDueMaxRetries struct{}

func (c *CheckStalePastDueMaxRetries) ID() string         { return "T-2" }
func (c *CheckStalePastDueMaxRetries) Name() string       { return "stale_past_due_max_retries" }
func (c *CheckStalePastDueMaxRetries) Category() string   { return "temporal" }
func (c *CheckStalePastDueMaxRetries) Severity() Severity { return SeverityHigh }

func (c *CheckStalePastDueMaxRetries) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	// Subscriptions that have exhausted retries and should be cancelled
	results, err := q.AuditStalePastDueMaxRetries(ctx)
	if err != nil {
		return nil, fmt.Errorf("query stale past due max retries: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntitySubscription,
			EntityID:       r.ID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Subscription has exhausted all %d retry attempts but is still past_due", r.RetryAttempts),
			Recommendation: "Cancel subscription as dunning has been exhausted",
			AutoFixable:    true,
			Details: map[string]any{
				"retry_attempts":         r.RetryAttempts,
				"current_period_ends_at": r.CurrentPeriodEndsAt,
			},
		})
	}

	return findings, nil
}

// T-3: Future-dated payment (purchased_at in the future)
type CheckFutureDatedPayment struct{}

func (c *CheckFutureDatedPayment) ID() string         { return "T-3" }
func (c *CheckFutureDatedPayment) Name() string       { return "future_dated_payment" }
func (c *CheckFutureDatedPayment) Category() string   { return "temporal" }
func (c *CheckFutureDatedPayment) Severity() Severity { return SeverityMedium }

func (c *CheckFutureDatedPayment) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	// Allow 5 minute grace period for clock skew
	results, err := q.AuditFutureDatedPayment(ctx)
	if err != nil {
		return nil, fmt.Errorf("query future dated payments: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntityPayment,
			EntityID:       r.ID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Payment has purchased_at in the future: %s", r.PurchasedAt.Format(time.RFC3339)),
			Recommendation: "MANUAL REVIEW - Possible clock skew or data corruption",
			AutoFixable:    false,
			Details: map[string]any{
				"purchased_at": r.PurchasedAt,
				"amount":       r.Amount,
			},
		})
	}

	return findings, nil
}

// T-4: Entitlement start in distant future (might indicate configuration error)
type CheckEntitlementDistantFutureStart struct{}

func (c *CheckEntitlementDistantFutureStart) ID() string         { return "T-4" }
func (c *CheckEntitlementDistantFutureStart) Name() string       { return "entitlement_distant_future_start" }
func (c *CheckEntitlementDistantFutureStart) Category() string   { return "temporal" }
func (c *CheckEntitlementDistantFutureStart) Severity() Severity { return SeverityLow }

func (c *CheckEntitlementDistantFutureStart) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	// Flag entitlements that start more than 1 year in the future
	results, err := q.AuditEntitlementDistantFutureStart(ctx)
	if err != nil {
		return nil, fmt.Errorf("query distant future start entitlements: %w", err)
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
			Description:    fmt.Sprintf("Entitlement '%s' starts more than 1 year in the future: %s", r.Entitlement, r.StartAt.Format(time.RFC3339)),
			Recommendation: "MANUAL REVIEW - Verify this is intentional",
			AutoFixable:    false,
			Details: map[string]any{
				"entitlement": r.Entitlement,
				"start_at":    r.StartAt,
			},
		})
	}

	return findings, nil
}
