package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/open-rails/openrails/internal/db/gen"
)

// SS-1: Active subscription past period end
type CheckActiveSubscriptionPastPeriodEnd struct{}

func (c *CheckActiveSubscriptionPastPeriodEnd) ID() string { return "SS-1" }
func (c *CheckActiveSubscriptionPastPeriodEnd) Name() string {
	return "active_subscription_past_period_end"
}
func (c *CheckActiveSubscriptionPastPeriodEnd) Category() string   { return "subscription_state" }
func (c *CheckActiveSubscriptionPastPeriodEnd) Severity() Severity { return SeverityHigh }

func (c *CheckActiveSubscriptionPastPeriodEnd) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	results, err := q.AuditActiveSubscriptionPastPeriodEnd(ctx)
	if err != nil {
		return nil, fmt.Errorf("query past period end subscriptions: %w", err)
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
			Description:    fmt.Sprintf("Subscription is active but period ended at %s", r.CurrentPeriodEndsAt.Format(time.RFC3339)),
			Recommendation: "Transition to past_due and attempt rebill, or cancel if grace period exceeded",
			AutoFixable:    true,
			Details: map[string]any{
				"period_ends_at": r.CurrentPeriodEndsAt,
				"hours_overdue":  time.Since(r.CurrentPeriodEndsAt).Hours(),
			},
		})
	}

	return findings, nil
}

// SS-2: Cancelled without metadata
type CheckCancelledWithoutMetadata struct{}

func (c *CheckCancelledWithoutMetadata) ID() string         { return "SS-2" }
func (c *CheckCancelledWithoutMetadata) Name() string       { return "cancelled_without_metadata" }
func (c *CheckCancelledWithoutMetadata) Category() string   { return "subscription_state" }
func (c *CheckCancelledWithoutMetadata) Severity() Severity { return SeverityMedium }

func (c *CheckCancelledWithoutMetadata) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	results, err := q.AuditCancelledWithoutMetadata(ctx)
	if err != nil {
		return nil, fmt.Errorf("query cancelled without metadata: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		missing := []string{}
		if r.CancelledAt == nil {
			missing = append(missing, "cancelled_at")
		}
		if r.CancelType == nil {
			missing = append(missing, "cancel_type")
		}

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntitySubscription,
			EntityID:       r.ID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Cancelled subscription missing: %v", missing),
			Recommendation: "Set cancelled_at=updated_at and cancel_type='unknown'",
			AutoFixable:    true,
			Details: map[string]any{
				"missing_fields": missing,
				"updated_at":     r.UpdatedAt,
			},
		})
	}

	return findings, nil
}

// SS-3: Past due without retry scheduled
type CheckPastDueWithoutRetry struct{}

func (c *CheckPastDueWithoutRetry) ID() string         { return "SS-3" }
func (c *CheckPastDueWithoutRetry) Name() string       { return "past_due_without_retry" }
func (c *CheckPastDueWithoutRetry) Category() string   { return "subscription_state" }
func (c *CheckPastDueWithoutRetry) Severity() Severity { return SeverityMedium }

func (c *CheckPastDueWithoutRetry) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	results, err := q.AuditPastDueWithoutRetry(ctx)
	if err != nil {
		return nil, fmt.Errorf("query past due without retry: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		attempts := 0
		if r.RetryAttempts != nil {
			attempts = int(*r.RetryAttempts)
		}

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntitySubscription,
			EntityID:       r.ID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Past-due subscription has no next_retry_at scheduled (attempts: %d/5)", attempts),
			Recommendation: "Set next_retry_at to schedule next dunning attempt",
			AutoFixable:    true,
			Details: map[string]any{
				"retry_attempts": attempts,
			},
		})
	}

	return findings, nil
}

// SS-4: Invalid period dates
type CheckInvalidPeriodDates struct{}

func (c *CheckInvalidPeriodDates) ID() string         { return "SS-4" }
func (c *CheckInvalidPeriodDates) Name() string       { return "invalid_period_dates" }
func (c *CheckInvalidPeriodDates) Category() string   { return "subscription_state" }
func (c *CheckInvalidPeriodDates) Severity() Severity { return SeverityHigh }

func (c *CheckInvalidPeriodDates) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	results, err := q.AuditInvalidPeriodDates(ctx)
	if err != nil {
		return nil, fmt.Errorf("query invalid period dates: %w", err)
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
			Description:    fmt.Sprintf("Period start (%s) >= period end (%s)", r.CurrentPeriodStartsAt.Format(time.RFC3339), r.CurrentPeriodEndsAt.Format(time.RFC3339)),
			Recommendation: "MANUAL REVIEW - Data corruption detected",
			AutoFixable:    false,
			Details: map[string]any{
				"period_starts_at": r.CurrentPeriodStartsAt,
				"period_ends_at":   r.CurrentPeriodEndsAt,
			},
		})
	}

	return findings, nil
}

// SS-5: Ended before cancelled
type CheckEndedBeforeCancelled struct{}

func (c *CheckEndedBeforeCancelled) ID() string         { return "SS-5" }
func (c *CheckEndedBeforeCancelled) Name() string       { return "ended_before_cancelled" }
func (c *CheckEndedBeforeCancelled) Category() string   { return "subscription_state" }
func (c *CheckEndedBeforeCancelled) Severity() Severity { return SeverityLow }

func (c *CheckEndedBeforeCancelled) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	results, err := q.AuditEndedBeforeCancelled(ctx)
	if err != nil {
		return nil, fmt.Errorf("query ended before cancelled: %w", err)
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
			Description:    fmt.Sprintf("ended_at (%s) < cancelled_at (%s) - temporal ordering violation", r.EndedAt.Format(time.RFC3339), r.CancelledAt.Format(time.RFC3339)),
			Recommendation: "Set ended_at = cancelled_at",
			AutoFixable:    true,
			Details: map[string]any{
				"ended_at":     r.EndedAt,
				"cancelled_at": r.CancelledAt,
			},
		})
	}

	return findings, nil
}
