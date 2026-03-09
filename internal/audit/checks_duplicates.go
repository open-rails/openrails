package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/uptrace/bun"
)

// D-1: Multiple active subscriptions for same user
type CheckMultipleActiveSubscriptions struct{}

func (c *CheckMultipleActiveSubscriptions) ID() string         { return "D-1" }
func (c *CheckMultipleActiveSubscriptions) Name() string       { return "multiple_active_subscriptions" }
func (c *CheckMultipleActiveSubscriptions) Category() string   { return "duplicates" }
func (c *CheckMultipleActiveSubscriptions) Severity() Severity { return SeverityCritical }

func (c *CheckMultipleActiveSubscriptions) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	type result struct {
		UserID string      `bun:"user_id"`
		Count  int         `bun:"count"`
		SubIDs []uuid.UUID `bun:"sub_ids,array"`
	}

	var results []result
	err := db.NewRaw(`
		SELECT
			user_id,
			COUNT(*) as count,
			ARRAY_AGG(id ORDER BY created_at DESC) as sub_ids
		FROM billing.subscriptions
		WHERE status = 'active'
		GROUP BY user_id
		HAVING COUNT(*) > 1
	`).Scan(ctx, &results)

	if err != nil {
		return nil, fmt.Errorf("query multiple active subscriptions: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		// Use the most recent subscription ID as the entity
		entityID := r.SubIDs[0]

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
				"subscription_ids": r.SubIDs,
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

func (c *CheckDuplicateChargesSamePeriod) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	// Find payments for same user, same product, within 30-day window
	type result struct {
		UserID      string      `bun:"user_id"`
		ProductID   uuid.UUID   `bun:"product_id"`
		ProductSlug string      `bun:"product_slug"`
		Count       int         `bun:"count"`
		PaymentIDs  []uuid.UUID `bun:"payment_ids,array"`
		TotalAmount int64       `bun:"total_amount"`
		FirstDate   time.Time   `bun:"first_date"`
		LastDate    time.Time   `bun:"last_date"`
	}

	var results []result
	err := db.NewRaw(`
		WITH payment_products AS (
			SELECT
				purch.id,
				purch.user_id,
				purch.amount,
				purch.purchased_at,
				price.product_id,
				prod.slug as product_slug
			FROM billing.payments purch
			JOIN billing.prices price ON purch.price_id = price.id
			JOIN billing.products prod ON price.product_id = prod.id
			WHERE purch.amount > 0
			  AND purch.refunded_payment_id IS NULL
		)
		SELECT
			user_id,
			product_id,
			product_slug,
			COUNT(*) as count,
			ARRAY_AGG(id ORDER BY purchased_at DESC) as payment_ids,
			SUM(amount) as total_amount,
			MIN(purchased_at) as first_date,
			MAX(purchased_at) as last_date
		FROM payment_products
		GROUP BY user_id, product_id, product_slug, DATE_TRUNC('month', purchased_at)
		HAVING COUNT(*) > 1
	`).Scan(ctx, &results)

	if err != nil {
		return nil, fmt.Errorf("query duplicate charges: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		// Use the most recent payment ID as the entity
		entityID := r.PaymentIDs[0]

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
				"payment_ids":  r.PaymentIDs,
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

func (c *CheckOverlappingEntitlementWindows) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	// Find overlapping entitlements for same user and entitlement name
	type result struct {
		UserID         string      `bun:"user_id"`
		Entitlement    string      `bun:"entitlement"`
		Count          int         `bun:"count"`
		EntitlementIDs []uuid.UUID `bun:"ent_ids,array"`
	}

	var results []result
	err := db.NewRaw(`
		WITH active_entitlements AS (
			SELECT
				id,
				user_id,
				entitlement,
				start_at,
				COALESCE(end_at, '9999-12-31'::timestamptz) as end_at
			FROM billing.entitlements
			WHERE revoked_at IS NULL
			  AND deleted_at IS NULL
		)
		SELECT
			e1.user_id,
			e1.entitlement,
			COUNT(DISTINCT e1.id) + COUNT(DISTINCT e2.id) as count,
			ARRAY_AGG(DISTINCT e1.id) || ARRAY_AGG(DISTINCT e2.id) as ent_ids
		FROM active_entitlements e1
		JOIN active_entitlements e2 ON
			e1.user_id = e2.user_id
			AND e1.entitlement = e2.entitlement
			AND e1.id < e2.id
			AND e1.start_at < e2.end_at
			AND e2.start_at < e1.end_at
		GROUP BY e1.user_id, e1.entitlement
	`).Scan(ctx, &results)

	if err != nil {
		return nil, fmt.Errorf("query overlapping entitlements: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		// Use the first entitlement ID as the entity
		entityID := r.EntitlementIDs[0]

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
				"entitlement_ids": r.EntitlementIDs,
				"entitlement":     r.Entitlement,
			},
		})
	}

	return findings, nil
}
