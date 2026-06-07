package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/open-rails/openrails/internal/db/models"
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

func (c *CheckActiveSubscriptionMissingEntitlements) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	// Get all active subscriptions with their product
	var subs []models.Subscription
	q := db.NewSelect().
		Model(&subs).
		Relation("Price").
		Relation("Price.Product").
		Where("sub.status = ?", models.StatusActive)

	if opts.UserID != "" {
		tsid, err := repo.ResolveTenantSubjectID(ctx, db, uuid.Nil, opts.UserID)
		if err != nil {
			return nil, err
		}
		q = q.Where("sub.tenant_subject_id = ?", tsid)
	}
	if opts.Since != nil {
		q = q.Where("sub.created_at >= ?", opts.Since)
	}

	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("query active subscriptions: %w", err)
	}

	for _, sub := range subs {
		if sub.Price == nil || sub.Price.Product == nil {
			continue
		}

		product := sub.Price.Product
		if product.EntitlementsSpec == nil {
			continue
		}

		// Check each entitlement in the product spec
		for entName := range product.EntitlementsSpec {
			// Check if user has this entitlement from this subscription
			var count int
			count, err := db.NewSelect().
				Model((*models.Entitlement)(nil)).
				Where("tenant_subject_id = ?", sub.TenantSubjectID).
				Where("entitlement = ?", entName).
				Where("source_type = ?", models.EntitlementSourceSubscription).
				Where("source_id = ?", sub.ID).
				Where("revoked_at IS NULL").
				Where("(end_at IS NULL OR end_at > ?)", time.Now()).
				Count(ctx)

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
					Description:    fmt.Sprintf("Active subscription missing entitlement '%s' from product '%s'", entName, product.Slug),
					Recommendation: fmt.Sprintf("Grant entitlement '%s' with source_type=subscription, source_id=%s", entName, sub.ID),
					AutoFixable:    true,
					Details: map[string]any{
						"missing_entitlement": entName,
						"product_slug":        product.Slug,
						"product_id":          product.ID,
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

func (c *CheckOrphanSubscriptionEntitlements) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	// Find entitlements where source_type=subscription but subscription is not active/pending/past_due or doesn't exist
	type orphanResult struct {
		EntitlementID   uuid.UUID `bun:"entitlement_id"`
		UserID          string    `bun:"user_id"`
		EntitlementName string    `bun:"entitlement"`
		SourceID        uuid.UUID `bun:"source_id"`
		SubStatus       *string   `bun:"sub_status"`
	}

	var results []orphanResult
	q := db.NewRaw(`
		SELECT
			ent.id as entitlement_id,
			ent.tenant_subject_id::text AS user_id,
			ent.entitlement,
			ent.source_id,
			sub.status as sub_status
		FROM billing.entitlements ent
		LEFT JOIN billing.subscriptions sub ON ent.source_id = sub.id
		WHERE ent.source_type = 'subscription'
		  AND ent.revoked_at IS NULL
		  AND ent.deleted_at IS NULL
		  AND (ent.end_at IS NULL OR ent.end_at > NOW())
		  AND (sub.id IS NULL OR sub.status NOT IN ('active', 'pending', 'past_due'))
	`)

	if err := db.NewRaw(q.String()).Scan(ctx, &results); err != nil {
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
			Description:    fmt.Sprintf("Entitlement '%s' references subscription %s which is %s", r.EntitlementName, r.SourceID, status),
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

func (c *CheckCancelledSubscriptionActiveEntitlements) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	// Find cancelled subscriptions with ended_at set (immediate revoke) but entitlements still active
	type result struct {
		SubID           uuid.UUID `bun:"sub_id"`
		UserID          string    `bun:"user_id"`
		EndedAt         time.Time `bun:"ended_at"`
		EntitlementID   uuid.UUID `bun:"ent_id"`
		EntitlementName string    `bun:"entitlement"`
	}

	var results []result
	err := db.NewRaw(`
		SELECT
			sub.id as sub_id,
			sub.tenant_subject_id::text AS user_id,
			sub.ended_at,
			ent.id as ent_id,
			ent.entitlement
		FROM billing.subscriptions sub
		INNER JOIN billing.entitlements ent ON ent.source_id = sub.id
		WHERE sub.status = 'cancelled'
		  AND sub.ended_at IS NOT NULL
		  AND ent.source_type = 'subscription'
		  AND ent.revoked_at IS NULL
		  AND ent.deleted_at IS NULL
		  AND (ent.end_at IS NULL OR ent.end_at > NOW())
	`).Scan(ctx, &results)

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
			EntityID:       r.EntitlementID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Subscription %s was cancelled with immediate revoke (ended_at=%s) but entitlement '%s' is still active", r.SubID, r.EndedAt.Format(time.RFC3339), r.EntitlementName),
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

func (c *CheckWrongEntitlementEndDate) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	// Find cancelled subscriptions without ended_at (period-end cancel) where entitlement end_at != period_ends_at
	type result struct {
		SubID            uuid.UUID  `bun:"sub_id"`
		UserID           string     `bun:"user_id"`
		PeriodEndsAt     time.Time  `bun:"period_ends_at"`
		EntitlementID    uuid.UUID  `bun:"ent_id"`
		EntitlementName  string     `bun:"entitlement"`
		EntitlementEndAt *time.Time `bun:"ent_end_at"`
	}

	var results []result
	err := db.NewRaw(`
		SELECT
			sub.id as sub_id,
			sub.tenant_subject_id::text AS user_id,
			sub.current_period_ends_at as period_ends_at,
			ent.id as ent_id,
			ent.entitlement,
			ent.end_at as ent_end_at
		FROM billing.subscriptions sub
		INNER JOIN billing.entitlements ent ON ent.source_id = sub.id
		WHERE sub.status = 'cancelled'
		  AND sub.cancelled_at IS NOT NULL
		  AND sub.ended_at IS NULL
		  AND sub.current_period_ends_at IS NOT NULL
		  AND ent.source_type = 'subscription'
		  AND ent.revoked_at IS NULL
		  AND ent.deleted_at IS NULL
		  AND (ent.end_at IS NULL OR ent.end_at != sub.current_period_ends_at)
	`).Scan(ctx, &results)

	if err != nil {
		return nil, fmt.Errorf("query wrong end dates: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		entEndStr := "NULL"
		if r.EntitlementEndAt != nil {
			entEndStr = r.EntitlementEndAt.Format(time.RFC3339)
		}

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntityEntitlement,
			EntityID:       r.EntitlementID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Entitlement '%s' end_at (%s) doesn't match subscription period end (%s)", r.EntitlementName, entEndStr, r.PeriodEndsAt.Format(time.RFC3339)),
			Recommendation: fmt.Sprintf("Update entitlement.end_at to %s", r.PeriodEndsAt.Format(time.RFC3339)),
			AutoFixable:    true,
			Details: map[string]any{
				"subscription_id": r.SubID,
				"expected_end_at": r.PeriodEndsAt,
				"actual_end_at":   r.EntitlementEndAt,
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

func (c *CheckEntitlementSourceMismatch) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	// Find entitlements where subscription exists but belongs to different user
	type result struct {
		EntitlementID   uuid.UUID `bun:"ent_id"`
		EntUserID       string    `bun:"ent_user_id"`
		EntitlementName string    `bun:"entitlement"`
		SourceID        uuid.UUID `bun:"source_id"`
		SubUserID       *string   `bun:"sub_user_id"`
	}

	var results []result
	err := db.NewRaw(`
		SELECT
			ent.id as ent_id,
			ent.tenant_subject_id::text AS ent_user_id,
			ent.entitlement,
			ent.source_id,
			sub.tenant_subject_id::text AS sub_user_id
		FROM billing.entitlements ent
		LEFT JOIN billing.subscriptions sub ON ent.source_id = sub.id
		WHERE ent.source_type = 'subscription'
		  AND ent.source_id IS NOT NULL
		  AND ent.deleted_at IS NULL
		  AND (sub.id IS NULL OR sub.tenant_subject_id != ent.tenant_subject_id)
	`).Scan(ctx, &results)

	if err != nil {
		return nil, fmt.Errorf("query source mismatches: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.EntUserID != opts.UserID {
			continue
		}

		desc := fmt.Sprintf("Entitlement '%s' references subscription %s which doesn't exist", r.EntitlementName, r.SourceID)
		if r.SubUserID != nil {
			desc = fmt.Sprintf("Entitlement '%s' for user %s references subscription %s which belongs to user %s", r.EntitlementName, r.EntUserID, r.SourceID, *r.SubUserID)
		}

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntityEntitlement,
			EntityID:       r.EntitlementID,
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
