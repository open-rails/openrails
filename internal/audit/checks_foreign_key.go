package audit

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/open-rails/openrails/internal/db/models"
)

// FK-1: Orphan subscription product
type CheckOrphanSubscriptionProduct struct{}

func (c *CheckOrphanSubscriptionProduct) ID() string         { return "FK-1" }
func (c *CheckOrphanSubscriptionProduct) Name() string       { return "orphan_subscription_product" }
func (c *CheckOrphanSubscriptionProduct) Category() string   { return "foreign_key" }
func (c *CheckOrphanSubscriptionProduct) Severity() Severity { return SeverityHigh }

func (c *CheckOrphanSubscriptionProduct) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	type result struct {
		SubID      uuid.UUID `bun:"sub_id"`
		UserID     string    `bun:"user_id"`
		ProductID  uuid.UUID `bun:"product_id"`
		ProdExists bool      `bun:"prod_exists"`
		ProdActive *bool     `bun:"prod_active"`
	}

	var results []result
	err := db.NewRaw(`
		SELECT
			sub.id as sub_id,
			sub.user_id,
			sub.product_id,
			CASE WHEN prod.id IS NOT NULL THEN true ELSE false END as prod_exists,
			(prod.status = 'active') as prod_active
		FROM billing.subscriptions sub
		LEFT JOIN billing.products prod ON sub.product_id = prod.id
		WHERE prod.id IS NULL OR prod.status <> 'active'
	`).Scan(ctx, &results)

	if err != nil {
		return nil, fmt.Errorf("query orphan subscription products: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		issue := "does not exist"
		if r.ProdExists && r.ProdActive != nil && !*r.ProdActive {
			issue = "is inactive"
		}

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntitySubscription,
			EntityID:       r.SubID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Subscription references product %s which %s", r.ProductID, issue),
			Recommendation: "MANUAL REVIEW - Product may have been deleted or deactivated",
			AutoFixable:    false,
			Details: map[string]any{
				"product_id":     r.ProductID,
				"product_exists": r.ProdExists,
				"product_active": r.ProdActive,
			},
		})
	}

	return findings, nil
}

// FK-2: Orphan subscription price
type CheckOrphanSubscriptionPrice struct{}

func (c *CheckOrphanSubscriptionPrice) ID() string         { return "FK-2" }
func (c *CheckOrphanSubscriptionPrice) Name() string       { return "orphan_subscription_price" }
func (c *CheckOrphanSubscriptionPrice) Category() string   { return "foreign_key" }
func (c *CheckOrphanSubscriptionPrice) Severity() Severity { return SeverityHigh }

func (c *CheckOrphanSubscriptionPrice) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	type result struct {
		SubID       uuid.UUID `bun:"sub_id"`
		UserID      string    `bun:"user_id"`
		PriceID     uuid.UUID `bun:"price_id"`
		PriceExists bool      `bun:"price_exists"`
		PriceActive *bool     `bun:"price_active"`
	}

	var results []result
	err := db.NewRaw(`
		SELECT
			sub.id as sub_id,
			sub.user_id,
			sub.price_id,
			CASE WHEN price.id IS NOT NULL THEN true ELSE false END as price_exists,
			(price.status = 'active') as price_active
		FROM billing.subscriptions sub
		LEFT JOIN billing.prices price ON sub.price_id = price.id
		WHERE price.id IS NULL OR price.status <> 'active'
	`).Scan(ctx, &results)

	if err != nil {
		return nil, fmt.Errorf("query orphan subscription prices: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		issue := "does not exist"
		if r.PriceExists && r.PriceActive != nil && !*r.PriceActive {
			issue = "is inactive"
		}

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntitySubscription,
			EntityID:       r.SubID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Subscription references price %s which %s", r.PriceID, issue),
			Recommendation: "MANUAL REVIEW - Price may have been deleted or deactivated",
			AutoFixable:    false,
			Details: map[string]any{
				"price_id":     r.PriceID,
				"price_exists": r.PriceExists,
				"price_active": r.PriceActive,
			},
		})
	}

	return findings, nil
}

// FK-3: Price-product mismatch
type CheckPriceProductMismatch struct{}

func (c *CheckPriceProductMismatch) ID() string         { return "FK-3" }
func (c *CheckPriceProductMismatch) Name() string       { return "price_product_mismatch" }
func (c *CheckPriceProductMismatch) Category() string   { return "foreign_key" }
func (c *CheckPriceProductMismatch) Severity() Severity { return SeverityHigh }

func (c *CheckPriceProductMismatch) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	type result struct {
		SubID          uuid.UUID `bun:"sub_id"`
		UserID         string    `bun:"user_id"`
		SubProductID   uuid.UUID `bun:"sub_product_id"`
		PriceProductID uuid.UUID `bun:"price_product_id"`
	}

	var results []result
	err := db.NewRaw(`
		SELECT
			sub.id as sub_id,
			sub.user_id,
			sub.product_id as sub_product_id,
			price.product_id as price_product_id
		FROM billing.subscriptions sub
		JOIN billing.prices price ON sub.price_id = price.id
		WHERE sub.product_id != price.product_id
	`).Scan(ctx, &results)

	if err != nil {
		return nil, fmt.Errorf("query price product mismatch: %w", err)
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
			EntityID:       r.SubID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Subscription product_id (%s) != price.product_id (%s)", r.SubProductID, r.PriceProductID),
			Recommendation: "Update subscription.product_id to match price.product_id",
			AutoFixable:    true,
			Details: map[string]any{
				"subscription_product_id": r.SubProductID,
				"price_product_id":        r.PriceProductID,
			},
		})
	}

	return findings, nil
}

// FK-4: Payment references non-existent subscription
type CheckPaymentOrphanSubscription struct{}

func (c *CheckPaymentOrphanSubscription) ID() string         { return "FK-4" }
func (c *CheckPaymentOrphanSubscription) Name() string       { return "payment_orphan_subscription" }
func (c *CheckPaymentOrphanSubscription) Category() string   { return "foreign_key" }
func (c *CheckPaymentOrphanSubscription) Severity() Severity { return SeverityMedium }

func (c *CheckPaymentOrphanSubscription) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	type result struct {
		PaymentID      uuid.UUID `bun:"payment_id"`
		UserID         string    `bun:"user_id"`
		SubscriptionID uuid.UUID `bun:"subscription_id"`
	}

	var results []result
	err := db.NewRaw(`
		SELECT
			purch.id as payment_id,
			purch.user_id,
			purch.subscription_id
		FROM billing.payments purch
		LEFT JOIN billing.subscriptions sub ON purch.subscription_id = sub.id
		WHERE purch.subscription_id IS NOT NULL
		  AND sub.id IS NULL
	`).Scan(ctx, &results)

	if err != nil {
		return nil, fmt.Errorf("query payment orphan subscription: %w", err)
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
			EntityID:       r.PaymentID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Payment references non-existent subscription %s", r.SubscriptionID),
			Recommendation: "Clear subscription_id (payment record is still valid)",
			AutoFixable:    true,
			Details: map[string]any{
				"missing_subscription_id": r.SubscriptionID,
			},
		})
	}

	return findings, nil
}

// FK-5: Entitlement references non-existent source
type CheckEntitlementOrphanSource struct{}

func (c *CheckEntitlementOrphanSource) ID() string         { return "FK-5" }
func (c *CheckEntitlementOrphanSource) Name() string       { return "entitlement_orphan_source" }
func (c *CheckEntitlementOrphanSource) Category() string   { return "foreign_key" }
func (c *CheckEntitlementOrphanSource) Severity() Severity { return SeverityMedium }

func (c *CheckEntitlementOrphanSource) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	// Check subscription sources
	type result struct {
		EntID       uuid.UUID `bun:"ent_id"`
		UserID      string    `bun:"user_id"`
		Entitlement string    `bun:"entitlement"`
		SourceType  string    `bun:"source_type"`
		SourceID    uuid.UUID `bun:"source_id"`
	}

	var subResults []result
	err := db.NewRaw(`
		SELECT
			ent.id as ent_id,
			ent.user_id,
			ent.entitlement,
			ent.source_type,
			ent.source_id
		FROM billing.entitlements ent
		LEFT JOIN billing.subscriptions sub ON ent.source_id = sub.id
		WHERE ent.source_type = ?
		  AND ent.source_id IS NOT NULL
		  AND ent.deleted_at IS NULL
		  AND sub.id IS NULL
	`, models.EntitlementSourceSubscription).Scan(ctx, &subResults)

	if err != nil {
		return nil, fmt.Errorf("query entitlement orphan subscription: %w", err)
	}

	for _, r := range subResults {
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
			Description:    fmt.Sprintf("Entitlement '%s' references non-existent subscription %s", r.Entitlement, r.SourceID),
			Recommendation: "MANUAL REVIEW - May be valid historical entitlement",
			AutoFixable:    false,
			Details: map[string]any{
				"source_type":             r.SourceType,
				"missing_subscription_id": r.SourceID,
			},
		})
	}

	// Check one_off payment sources
	var payResults []result
	err = db.NewRaw(`
		SELECT
			ent.id as ent_id,
			ent.user_id,
			ent.entitlement,
			ent.source_type,
			ent.source_id
		FROM billing.entitlements ent
		LEFT JOIN billing.payments purch ON ent.source_id = purch.id
		WHERE ent.source_type = ?
		  AND ent.source_id IS NOT NULL
		  AND ent.deleted_at IS NULL
		  AND purch.id IS NULL
	`, models.EntitlementSourceOneOff).Scan(ctx, &payResults)

	if err != nil {
		return nil, fmt.Errorf("query entitlement orphan payment: %w", err)
	}

	for _, r := range payResults {
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
			Description:    fmt.Sprintf("Entitlement '%s' references non-existent payment %s", r.Entitlement, r.SourceID),
			Recommendation: "MANUAL REVIEW - May be valid historical entitlement",
			AutoFixable:    false,
			Details: map[string]any{
				"source_type":        r.SourceType,
				"missing_payment_id": r.SourceID,
			},
		})
	}

	return findings, nil
}
