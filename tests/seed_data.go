//go:build integration

package tests

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
)

// TestProduct represents a seeded product with its prices
type TestProduct struct {
	Product *models.Product
	Prices  []*models.Price
}

// CCBill test data constants - these match values in testdata/webhooks/ccbill/*.json
const (
	// CCBillTestFlexID is the flexId from saved webhook payloads
	CCBillTestFlexID = "75383d6a-41d4-4bd0-ac12-6c8c37fde5e5"
	// CCBillTestFormName is the formName from saved webhook payloads
	CCBillTestFormName = "211cc"
	// CCBillTestUsername is the username from saved newsalesuccess.json webhook
	CCBillTestUsername = "a9ab7b27-a31c-45cd-9bb5-8a38999afd7d"
	// CCBillTestUsername2 is the username from other webhook files (upgrade, reactivation, etc.)
	CCBillTestUsername2 = "test_user_8421"
	// CCBillTestUserID is the user ID we map the first test username to (must be a valid UUID)
	CCBillTestUserID = "cccccccc-cccc-cccc-cccc-cccccccc0001"
	// CCBillTestUserID2 is the user ID we map the second test username to (must be a valid UUID)
	CCBillTestUserID2 = "cccccccc-cccc-cccc-cccc-cccccccc0002"
	// CCBillTestSubscriptionID is the subscriptionId from saved webhook payloads
	CCBillTestSubscriptionID = "0125217202000000017"
	// CCBillTestVoidSubscriptionID is from the void.json webhook (different subscription)
	CCBillTestVoidSubscriptionID = "0125217202000000020"
)

// DefaultTestProducts returns a comprehensive set of test products covering:
// - Multiple products with different entitlements
// - Multiple prices per product with varying currencies (USD, EUR, JPY)
// - Different billing cycles (monthly, quarterly, yearly, one-time)
// - Both recurring and one-off pricing options
func (suite *TestContainerSuite) DefaultTestProducts() []TestProduct {
	return []TestProduct{
		{
			// Product 1: Premium subscription with multiple price options
			Product: &models.Product{
				ID:          uuid.MustParse("11111111-1111-1111-1111-111111111111"),
				Slug:        "premium",
				DisplayName: "Premium",
				Description: "Premium subscription with full access",
				EntitlementsSpec: map[string]*int{
					"premium": nil, // Indefinite while subscription is active
				},
			},
			Prices: []*models.Price{
				{
					// Price 1.1: Monthly USD recurring
					ID:               uuid.MustParse("22222222-2222-2222-2222-222222222222"),
					DisplayName:      "Monthly - $9.99",
					Amount:           999, // Amount in cents ($9.99)
					Currency:         "usd",
					BillingCycleDays: intPtr(30),
					Processors: map[string]map[string]string{
						string(models.ProcessorMobius): {
							models.ProcessorKeyPlanID: "plan_monthly_usd_999",
						},
						// CCBillPriceID matches flexId from testdata/webhooks/ccbill/newsalesuccess.json
						string(models.ProcessorCCBill): {
							models.ProcessorKeyCCBillFormName: CCBillTestFormName,
							models.ProcessorKeyCCBillFlexID:   CCBillTestFlexID,
						},
						string(models.ProcessorSolana): {
							"enabled": "true",
						},
					},
				},
				{
					// Price 1.2: Quarterly USD recurring (discounted)
					ID:               uuid.MustParse("22222222-2222-2222-2222-222222222223"),
					DisplayName:      "Quarterly - $24.99",
					Amount:           2499, // Amount in cents ($24.99, ~17% discount)
					Currency:         "usd",
					BillingCycleDays: intPtr(90),
					Processors: map[string]map[string]string{
						string(models.ProcessorMobius): {
							models.ProcessorKeyPlanID: "plan_quarterly_usd_2499",
						},
						string(models.ProcessorCCBill): {
							models.ProcessorKeyCCBillFormName: "FormQuarterlyUSD",
							models.ProcessorKeyCCBillFlexID:   "ccbill_quarterly_usd_2499",
						},
						string(models.ProcessorSolana): {
							"enabled": "true",
						},
					},
				},
				{
					// Price 1.3: Monthly EUR recurring
					ID:               uuid.MustParse("22222222-2222-2222-2222-222222222224"),
					DisplayName:      "Monthly - €8.99",
					Amount:           899, // Amount in cents (€8.99)
					Currency:         "eur",
					BillingCycleDays: intPtr(30),
					Processors: map[string]map[string]string{
						string(models.ProcessorMobius): {
							models.ProcessorKeyPlanID: "plan_monthly_eur_899",
						},
						string(models.ProcessorCCBill): {
							models.ProcessorKeyCCBillFormName: "FormMonthlyEUR",
							models.ProcessorKeyCCBillFlexID:   "ccbill_monthly_eur_899",
						},
					},
				},
				{
					// Price 1.4: Monthly JPY recurring
					ID:               uuid.MustParse("22222222-2222-2222-2222-222222222225"),
					DisplayName:      "Monthly - ¥1,200",
					Amount:           1200, // Amount in yen (no decimals for JPY)
					Currency:         "jpy",
					BillingCycleDays: intPtr(30),
					Processors: map[string]map[string]string{
						string(models.ProcessorMobius): {
							models.ProcessorKeyPlanID: "plan_monthly_jpy_1200",
						},
						// CCBill doesn't support JPY in this example
					},
				},
				{
					// Price 1.5: Yearly USD recurring (heavily discounted)
					ID:               uuid.MustParse("22222222-2222-2222-2222-222222222226"),
					DisplayName:      "Yearly - $79.99",
					Amount:           7999, // Amount in cents ($79.99, ~33% discount)
					Currency:         "usd",
					BillingCycleDays: intPtr(365),
					Processors: map[string]map[string]string{
						string(models.ProcessorMobius): {
							models.ProcessorKeyPlanID: "plan_yearly_usd_7999",
						},
						string(models.ProcessorCCBill): {
							models.ProcessorKeyCCBillFormName: "FormYearlyUSD",
							models.ProcessorKeyCCBillFlexID:   "ccbill_yearly_usd_7999",
						},
						string(models.ProcessorSolana): {
							"enabled": "true",
						},
					},
				},
			},
		},
		{
			// Product 2: Pro tier with higher pricing and additional features
			Product: &models.Product{
				ID:          uuid.MustParse("33333333-3333-3333-3333-333333333333"),
				Slug:        "pro",
				DisplayName: "Pro",
				Description: "Pro subscription with premium features and priority support",
				EntitlementsSpec: map[string]*int{
					"premium":          nil,
					"priority_support": nil,
					"api_access":       nil,
				},
			},
			Prices: []*models.Price{
				{
					// Price 2.1: Monthly USD recurring
					ID:               uuid.MustParse("44444444-4444-4444-4444-444444444444"),
					DisplayName:      "Pro Monthly - $19.99",
					Amount:           1999, // Amount in cents ($19.99)
					Currency:         "usd",
					BillingCycleDays: intPtr(30),
					Processors: map[string]map[string]string{
						string(models.ProcessorMobius): {
							models.ProcessorKeyPlanID: "plan_pro_monthly_usd_1999",
						},
						string(models.ProcessorCCBill): {
							models.ProcessorKeyCCBillFormName: "FormProMonthlyUSD",
							models.ProcessorKeyCCBillFlexID:   "ccbill_pro_monthly_usd_1999",
						},
						string(models.ProcessorSolana): {
							"enabled": "true",
						},
					},
				},
				{
					// Price 2.2: Yearly USD recurring
					ID:               uuid.MustParse("44444444-4444-4444-4444-444444444445"),
					DisplayName:      "Pro Yearly - $149.99",
					Amount:           14999, // Amount in cents ($149.99)
					Currency:         "usd",
					BillingCycleDays: intPtr(365),
					Processors: map[string]map[string]string{
						string(models.ProcessorMobius): {
							models.ProcessorKeyPlanID: "plan_pro_yearly_usd_14999",
						},
						string(models.ProcessorCCBill): {
							models.ProcessorKeyCCBillFormName: "FormProYearlyUSD",
							models.ProcessorKeyCCBillFlexID:   "ccbill_pro_yearly_usd_14999",
						},
						string(models.ProcessorSolana): {
							"enabled": "true",
						},
					},
				},
				{
					// Price 2.3: Monthly EUR recurring
					ID:               uuid.MustParse("44444444-4444-4444-4444-444444444446"),
					DisplayName:      "Pro Monthly - €17.99",
					Amount:           1799, // Amount in cents (€17.99)
					Currency:         "eur",
					BillingCycleDays: intPtr(30),
					Processors: map[string]map[string]string{
						string(models.ProcessorMobius): {
							models.ProcessorKeyPlanID: "plan_pro_monthly_eur_1799",
						},
						string(models.ProcessorCCBill): {
							models.ProcessorKeyCCBillFormName: "FormProMonthlyEUR",
							models.ProcessorKeyCCBillFlexID:   "ccbill_pro_monthly_eur_1799",
						},
					},
				},
			},
		},
		{
			// Product 3: One-time purchase (lifetime access or credits)
			Product: &models.Product{
				ID:          uuid.MustParse("55555555-5555-5555-5555-555555555555"),
				Slug:        "lifetime",
				DisplayName: "Lifetime Access",
				Description: "One-time purchase for lifetime premium access",
				EntitlementsSpec: map[string]*int{
					"premium":  nil, // Indefinite
					"lifetime": nil, // Special lifetime marker
				},
			},
			Prices: []*models.Price{
				{
					// Price 3.1: One-time USD purchase (no billing cycle)
					ID:               uuid.MustParse("66666666-6666-6666-6666-666666666666"),
					DisplayName:      "Lifetime - $299.99",
					Amount:           29999, // Amount in cents ($299.99)
					Currency:         "usd",
					BillingCycleDays: nil, // One-time purchase, no recurring billing
					Processors: map[string]map[string]string{
						string(models.ProcessorMobius): {
							models.ProcessorKeyPlanID: "plan_lifetime_usd_29999",
						},
						string(models.ProcessorCCBill): {
							models.ProcessorKeyCCBillFormName: "FormLifetimeUSD",
							models.ProcessorKeyCCBillFlexID:   "ccbill_lifetime_usd_29999",
						},
						string(models.ProcessorSolana): {
							"enabled": "true",
						},
					},
				},
				{
					// Price 3.2: One-time EUR purchase
					ID:               uuid.MustParse("66666666-6666-6666-6666-666666666667"),
					DisplayName:      "Lifetime - €269.99",
					Amount:           26999, // Amount in cents (€269.99)
					Currency:         "eur",
					BillingCycleDays: nil, // One-time purchase
					Processors: map[string]map[string]string{
						string(models.ProcessorMobius): {
							models.ProcessorKeyPlanID: "plan_lifetime_eur_26999",
						},
						string(models.ProcessorCCBill): {
							models.ProcessorKeyCCBillFormName: "FormLifetimeEUR",
							models.ProcessorKeyCCBillFlexID:   "ccbill_lifetime_eur_26999",
						},
					},
				},
				{
					// Price 3.3: One-time JPY purchase
					ID:               uuid.MustParse("66666666-6666-6666-6666-666666666668"),
					DisplayName:      "Lifetime - ¥39,800",
					Amount:           39800, // Amount in yen
					Currency:         "jpy",
					BillingCycleDays: nil, // One-time purchase
					Processors: map[string]map[string]string{
						string(models.ProcessorMobius): {
							models.ProcessorKeyPlanID: "plan_lifetime_jpy_39800",
						},
						// CCBill doesn't support JPY in this example
					},
				},
			},
		},
		{
			// Product 4: NMI-only product (not available via CCBill)
			Product: &models.Product{
				ID:          uuid.MustParse("77777777-7777-7777-7777-777777777777"),
				Slug:        "basic",
				DisplayName: "Basic",
				Description: "Basic subscription (NMI/Mobius only)",
				EntitlementsSpec: map[string]*int{
					"basic": nil,
				},
			},
			Prices: []*models.Price{
				{
					// Price 4.1: Monthly USD - NMI only (no CCBill)
					ID:               uuid.MustParse("88888888-8888-8888-8888-888888888888"),
					DisplayName:      "Basic Monthly - $4.99",
					Amount:           499, // Amount in cents ($4.99)
					Currency:         "usd",
					BillingCycleDays: intPtr(30),
					Processors: map[string]map[string]string{
						string(models.ProcessorMobius): {
							models.ProcessorKeyPlanID: "plan_basic_monthly_usd_499",
						},
						// No CCBill - this price is NMI-only
						string(models.ProcessorSolana): {
							"enabled": "true",
						},
					},
				},
			},
		},
	}
}

// SeedProducts creates test products in the database (idempotent - uses UPSERT)
func (suite *TestContainerSuite) SeedProducts() []TestProduct {
	suite.t.Helper()
	ctx := context.Background()

	testProducts := suite.DefaultTestProducts()
	now := time.Now()

	for i := range testProducts {
		tp := &testProducts[i]
		tp.Product.CreatedAt = now
		tp.Product.UpdatedAt = now

		// Use ON CONFLICT to make this idempotent
		_, err := suite.BunDB.NewInsert().Model(tp.Product).
			On("CONFLICT (id) DO UPDATE").
			Set("display_name = EXCLUDED.display_name").
			Set("updated_at = EXCLUDED.updated_at").
			Exec(ctx)
		require.NoError(suite.t, err, "Failed to seed product %s", tp.Product.Slug)

		for _, price := range tp.Prices {
			price.ProductID = tp.Product.ID
			price.CreatedAt = now
			price.UpdatedAt = now

			// Prices are immutable - use DO NOTHING to preserve existing records
			_, err := suite.BunDB.NewInsert().Model(price).
				On("CONFLICT (id) DO NOTHING").
				Exec(ctx)
			require.NoError(suite.t, err, "Failed to seed price %s", price.DisplayName)
		}
	}

	return testProducts
}

// TieredTestProducts returns test products with tier groups for upgrade/downgrade testing
// Premium (tier_group="premium", rank=1) and Premium+ (tier_group="premium", rank=2)
func (suite *TestContainerSuite) TieredTestProducts() []TestProduct {
	premiumGroup := "premium"
	return []TestProduct{
		{
			Product: &models.Product{
				ID:          uuid.MustParse("aaaa1111-1111-1111-1111-111111111111"),
				Slug:        "premium-basic",
				DisplayName: "Premium",
				Description: "Basic premium tier",
				EntitlementsSpec: map[string]*int{
					"premium": intPtr(30),
				},
				TierGroup: &premiumGroup,
				TierRank:  1,
				IsActive:  true,
			},
			Prices: []*models.Price{
				{
					ID:               uuid.MustParse("aaaa2222-2222-2222-2222-222222222222"),
					DisplayName:      "Premium Monthly - $10",
					Amount:           1000,
					Currency:         "usd",
					BillingCycleDays: intPtr(30),
					Processors: map[string]map[string]string{
						string(models.ProcessorMobius): {
							models.ProcessorKeyPlanID: "plan_premium_basic_1000",
						},
					},
				},
			},
		},
		{
			Product: &models.Product{
				ID:          uuid.MustParse("bbbb1111-1111-1111-1111-111111111111"),
				Slug:        "premium-plus",
				DisplayName: "Premium+",
				Description: "Enhanced premium tier",
				EntitlementsSpec: map[string]*int{
					"premium": intPtr(30),
					"extra":   intPtr(30),
				},
				TierGroup: &premiumGroup,
				TierRank:  2,
				IsActive:  true,
			},
			Prices: []*models.Price{
				{
					ID:               uuid.MustParse("bbbb2222-2222-2222-2222-222222222222"),
					DisplayName:      "Premium+ Monthly - $20",
					Amount:           2000,
					Currency:         "usd",
					BillingCycleDays: intPtr(30),
					Processors: map[string]map[string]string{
						string(models.ProcessorMobius): {
							models.ProcessorKeyPlanID: "plan_premium_plus_2000",
						},
					},
				},
			},
		},
		{
			Product: &models.Product{
				ID:          uuid.MustParse("cccc1111-1111-1111-1111-111111111111"),
				Slug:        "premium-ultimate",
				DisplayName: "Premium Ultimate",
				Description: "Top tier premium",
				EntitlementsSpec: map[string]*int{
					"premium": intPtr(30),
					"extra":   intPtr(30),
					"vip":     intPtr(30),
				},
				TierGroup: &premiumGroup,
				TierRank:  3,
				IsActive:  true,
			},
			Prices: []*models.Price{
				{
					ID:               uuid.MustParse("cccc2222-2222-2222-2222-222222222222"),
					DisplayName:      "Premium Ultimate Monthly - $30",
					Amount:           3000,
					Currency:         "usd",
					BillingCycleDays: intPtr(30),
					Processors: map[string]map[string]string{
						string(models.ProcessorMobius): {
							models.ProcessorKeyPlanID: "plan_premium_ultimate_3000",
						},
					},
				},
			},
		},
	}
}

// SeedTieredProducts creates tiered test products in the database for upgrade/downgrade testing
func (suite *TestContainerSuite) SeedTieredProducts() []TestProduct {
	suite.t.Helper()
	ctx := context.Background()

	testProducts := suite.TieredTestProducts()
	now := time.Now()

	for i := range testProducts {
		tp := &testProducts[i]
		tp.Product.CreatedAt = now
		tp.Product.UpdatedAt = now

		_, err := suite.BunDB.NewInsert().Model(tp.Product).
			On("CONFLICT (id) DO UPDATE").
			Set("tier_group = EXCLUDED.tier_group").
			Set("tier_rank = EXCLUDED.tier_rank").
			Exec(ctx)
		require.NoError(suite.t, err, "Failed to seed tiered product %s", tp.Product.DisplayName)

		for _, price := range tp.Prices {
			price.ProductID = tp.Product.ID
			price.CreatedAt = now
			price.UpdatedAt = now

			_, err := suite.BunDB.NewInsert().Model(price).
				On("CONFLICT (id) DO NOTHING").
				Exec(ctx)
			require.NoError(suite.t, err, "Failed to seed tiered price %s", price.DisplayName)
		}
	}

	return testProducts
}

// GetSeededProduct retrieves a product by slug from the database
func (suite *TestContainerSuite) GetSeededProduct(slug string) *models.Product {
	suite.t.Helper()
	ctx := context.Background()

	var product models.Product
	err := suite.BunDB.NewSelect().
		Model(&product).
		Where("slug = ?", slug).
		Relation("Prices").
		Scan(ctx)
	require.NoError(suite.t, err, "Failed to get product %s", slug)

	return &product
}

// GetSeededPrice retrieves a price by ID from the database
func (suite *TestContainerSuite) GetSeededPrice(priceID uuid.UUID) *models.Price {
	suite.t.Helper()
	ctx := context.Background()

	var price models.Price
	err := suite.BunDB.NewSelect().
		Model(&price).
		Where("id = ?", priceID).
		Relation("Product").
		Scan(ctx)
	require.NoError(suite.t, err, "Failed to get price %s", priceID)

	return &price
}

// CreateTestSubscription creates a test subscription for a user
func (suite *TestContainerSuite) CreateTestSubscription(userID string, priceID uuid.UUID, status models.SubscriptionStatus) *models.Subscription {
	suite.t.Helper()
	ctx := context.Background()
	now := time.Now()

	// Look up the price to get ProductID
	price := suite.GetPrice(priceID)

	periodStart := now
	periodEnd := now.Add(30 * 24 * time.Hour)

	sub := &models.Subscription{
		ID:                      uuid.New(),
		UserID:                  userID,
		ProductID:               price.ProductID,
		PriceID:                 priceID,
		Status:                  status,
		StartedAt:               now,
		CurrentPeriodStartsAt:   &periodStart,
		CurrentPeriodEndsAt:     &periodEnd,
		Processor:               models.ProcessorMobius,
		ProcessorSubscriptionID: "test-sub-" + uuid.New().String()[:8],
		CreatedAt:               now,
		UpdatedAt:               now,
	}

	_, err := suite.BunDB.NewInsert().Model(sub).Exec(ctx)
	require.NoError(suite.t, err, "Failed to create test subscription")

	return sub
}

// CreateTestSubscriptionWithOptions creates a subscription with custom options
type SubscriptionOptions struct {
	UserID              string
	PriceID             uuid.UUID
	Status              models.SubscriptionStatus
	Processor           models.Processor
	PeriodStart         time.Time
	PeriodEnd           time.Time
	CurrentPeriodEndsAt *time.Time // Optional: if set, overrides PeriodEnd for current_period_ends_at
	PaymentMethodID     *uuid.UUID
	CancelType          *models.CancelType
	CancelFeedback      *string
	ProcessorSubID      string
	RetryAttempts       *int
	NextRetryAt         *time.Time
}

func (suite *TestContainerSuite) CreateTestSubscriptionWithOptions(opts SubscriptionOptions) *models.Subscription {
	suite.t.Helper()
	ctx := context.Background()
	now := suite.GetClock().Now()

	// Look up the price to get ProductID
	price := suite.GetPrice(opts.PriceID)

	if opts.PeriodStart.IsZero() {
		opts.PeriodStart = now
	}
	if opts.PeriodEnd.IsZero() {
		opts.PeriodEnd = now.Add(30 * 24 * time.Hour)
	}
	if opts.Processor == "" {
		opts.Processor = models.ProcessorMobius
	}
	if opts.ProcessorSubID == "" {
		opts.ProcessorSubID = "test-sub-" + uuid.New().String()[:8]
	}

	periodEndsAt := &opts.PeriodEnd
	if opts.CurrentPeriodEndsAt != nil {
		periodEndsAt = opts.CurrentPeriodEndsAt
	}

	sub := &models.Subscription{
		ID:                      uuid.New(),
		UserID:                  opts.UserID,
		ProductID:               price.ProductID,
		PriceID:                 opts.PriceID,
		Status:                  opts.Status,
		StartedAt:               opts.PeriodStart,
		CurrentPeriodStartsAt:   &opts.PeriodStart,
		CurrentPeriodEndsAt:     periodEndsAt,
		Processor:               opts.Processor,
		ProcessorSubscriptionID: opts.ProcessorSubID,
		PaymentMethodID:         opts.PaymentMethodID,
		CancelType:              opts.CancelType,
		CancelFeedback:          opts.CancelFeedback,
		RetryAttempts:           opts.RetryAttempts,
		NextRetryAt:             opts.NextRetryAt,
		CreatedAt:               now,
		UpdatedAt:               now,
	}

	if opts.Status == models.StatusCancelled {
		cancelledAt := now
		sub.CancelledAt = &cancelledAt
		sub.EndedAt = &cancelledAt
	}

	_, err := suite.BunDB.NewInsert().Model(sub).Exec(ctx)
	require.NoError(suite.t, err, "Failed to create test subscription with options")

	return sub
}

// CreateTestPaymentMethod creates a test payment method for a user
func (suite *TestContainerSuite) CreateTestPaymentMethod(userID string) *models.PaymentMethod {
	suite.t.Helper()
	ctx := context.Background()
	now := time.Now()

	pm := &models.PaymentMethod{
		ID:                   uuid.New(),
		UserID:               userID,
		Processor:            models.ProcessorMobius,
		VaultID:              "vault-" + uuid.New().String()[:8],
		BillingID:            strPtr("billing-" + uuid.New().String()[:8]),
		InitialTransactionID: "txn-" + uuid.New().String()[:8],
		LastFour:             strPtr("4242"),
		CardType:             strPtr("Visa"),
		ExpiryDate:           strPtr("12/25"),
		CreatedAt:            now,
		UpdatedAt:            now,
	}

	_, err := suite.BunDB.NewInsert().Model(pm).Exec(ctx)
	require.NoError(suite.t, err, "Failed to create test payment method")

	return pm
}

// CreateTestPaymentMethodWithOptions creates a payment method with custom options
type PaymentMethodOptions struct {
	UserID               string
	Processor            models.Processor
	VaultID              string
	BillingID            string
	InitialTransactionID string
	LastFour             string
	CardType             string
	ExpiryDate           string
	FailureReason        *string
}

func (suite *TestContainerSuite) CreateTestPaymentMethodWithOptions(opts PaymentMethodOptions) *models.PaymentMethod {
	suite.t.Helper()
	ctx := context.Background()
	now := time.Now()

	if opts.Processor == "" {
		opts.Processor = models.ProcessorMobius
	}
	if opts.VaultID == "" {
		opts.VaultID = "vault-" + uuid.New().String()[:8]
	}
	if opts.InitialTransactionID == "" {
		opts.InitialTransactionID = "txn-" + uuid.New().String()[:8]
	}

	pm := &models.PaymentMethod{
		ID:                   uuid.New(),
		UserID:               opts.UserID,
		Processor:            opts.Processor,
		VaultID:              opts.VaultID,
		BillingID:            strPtrOrNil(opts.BillingID),
		InitialTransactionID: opts.InitialTransactionID,
		LastFour:             strPtrOrNil(opts.LastFour),
		CardType:             strPtrOrNil(opts.CardType),
		ExpiryDate:           strPtrOrNil(opts.ExpiryDate),
		FailureReason:        opts.FailureReason,
		CreatedAt:            now,
		UpdatedAt:            now,
	}

	_, err := suite.BunDB.NewInsert().Model(pm).Exec(ctx)
	require.NoError(suite.t, err, "Failed to create test payment method with options")

	return pm
}

// CreateTestPayment creates a test payment record
func (suite *TestContainerSuite) CreateTestPayment(userID string, priceID uuid.UUID, subscriptionID *uuid.UUID) *models.Payment {
	suite.t.Helper()
	ctx := context.Background()
	now := time.Now()

	payment := &models.Payment{
		ID:             uuid.New(),
		UserID:         userID,
		PriceID:        priceID,
		SubscriptionID: subscriptionID,
		Processor:      models.ProcessorMobius,
		TransactionID:  "txn-" + uuid.New().String()[:8],
		Amount:         999, // Amount in cents ($9.99)
		Currency:       "usd",
		PurchasedAt:    now,
		CreatedAt:      now,
	}

	_, err := suite.BunDB.NewInsert().Model(payment).Exec(ctx)
	require.NoError(suite.t, err, "Failed to create test payment")

	return payment
}

// CreateTestPaymentWithOptions creates a payment with custom options
type PaymentOptions struct {
	UserID            string
	PriceID           uuid.UUID
	SubscriptionID    *uuid.UUID
	RefundedPaymentID *uuid.UUID
	Processor         models.Processor
	TransactionID     string
	Amount            int64 // Amount in cents
	Currency          string
	PurchasedAt       time.Time
}

func (suite *TestContainerSuite) CreateTestPaymentWithOptions(opts PaymentOptions) *models.Payment {
	suite.t.Helper()
	ctx := context.Background()
	now := time.Now()

	if opts.Processor == "" {
		opts.Processor = models.ProcessorMobius
	}
	if opts.TransactionID == "" {
		opts.TransactionID = "txn-" + uuid.New().String()[:8]
	}
	if opts.Amount == 0 {
		opts.Amount = 999 // Default: $9.99 in cents
	}
	if opts.Currency == "" {
		opts.Currency = "usd"
	}
	if opts.PurchasedAt.IsZero() {
		opts.PurchasedAt = now
	}

	payment := &models.Payment{
		ID:                uuid.New(),
		UserID:            opts.UserID,
		PriceID:           opts.PriceID,
		SubscriptionID:    opts.SubscriptionID,
		RefundedPaymentID: opts.RefundedPaymentID,
		Processor:         opts.Processor,
		TransactionID:     opts.TransactionID,
		Amount:            opts.Amount,
		Currency:          opts.Currency,
		PurchasedAt:       opts.PurchasedAt,
		CreatedAt:         now,
	}

	_, err := suite.BunDB.NewInsert().Model(payment).Exec(ctx)
	require.NoError(suite.t, err, "Failed to create test payment with options")

	return payment
}

// CreateTestEntitlement creates a test entitlement for a user.
// Uses the mock clock if set, otherwise falls back to real time.
func (suite *TestContainerSuite) CreateTestEntitlement(userID string, entitlementName string, sourceID *uuid.UUID, sourceType models.EntitlementSourceType) *models.Entitlement {
	suite.t.Helper()
	ctx := context.Background()
	now := suite.GetClock().Now()

	if sourceID == nil {
		switch sourceType {
		case models.EntitlementSourceAdmin:
			adminGrant := &models.AdminGrant{
				ID:           uuid.New(),
				UserID:       userID,
				GrantedBy:    "test-admin",
				Reason:       "test_admin_entitlement",
				DurationDays: nil,
				CreatedAt:    now,
			}
			_, err := suite.BunDB.NewInsert().Model(adminGrant).Exec(ctx)
			require.NoError(suite.t, err, "Failed to create test admin_grant source")
			sourceID = &adminGrant.ID
		default:
			require.FailNow(suite.t, "sourceID is required for this sourceType", "sourceType=%s", sourceType)
		}
	}

	// For subscription-sourced entitlements, end_at should be NULL (indefinite while subscription is active)
	// For other sources, we may want a finite window
	var endAt *time.Time
	if sourceType != models.EntitlementSourceSubscription {
		end := now.Add(30 * 24 * time.Hour)
		endAt = &end
	}

	ent := &models.Entitlement{
		ID:          uuid.New(),
		UserID:      userID,
		Entitlement: entitlementName,
		StartAt:     now,
		EndAt:       endAt,
		SourceID:    sourceID,
		SourceType:  sourceType,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	_, err := suite.BunDB.NewInsert().Model(ent).Exec(ctx)
	require.NoError(suite.t, err, "Failed to create test entitlement")

	return ent
}

// CreateTestNotification creates a test notification for a user
func (suite *TestContainerSuite) CreateTestNotification(userID string, eventType models.NotificationEventType, data map[string]any) *models.NotificationQueue {
	suite.t.Helper()
	ctx := context.Background()
	now := time.Now()

	notif := &models.NotificationQueue{
		ID:        uuid.New(),
		UserID:    userID,
		EventType: eventType,
		Data:      data,
		Seen:      false,
		CreatedAt: now,
	}

	_, err := suite.BunDB.NewInsert().Model(notif).Exec(ctx)
	require.NoError(suite.t, err, "Failed to create test notification")

	return notif
}

// Query helpers for assertions

// GetSubscription retrieves a subscription by ID
func (suite *TestContainerSuite) GetSubscription(id uuid.UUID) *models.Subscription {
	suite.t.Helper()
	ctx := context.Background()

	var sub models.Subscription
	err := suite.BunDB.NewSelect().
		Model(&sub).
		Where("sub.id = ?", id).
		Relation("Price").
		Relation("PaymentMethod").
		Scan(ctx)
	require.NoError(suite.t, err, "Failed to get subscription %s", id)

	return &sub
}

// GetSubscriptionByUserID retrieves the active subscription for a user
func (suite *TestContainerSuite) GetSubscriptionByUserID(userID string) *models.Subscription {
	suite.t.Helper()
	ctx := context.Background()

	var sub models.Subscription
	err := suite.BunDB.NewSelect().
		Model(&sub).
		Where("user_id = ?", userID).
		Where("status = ?", models.StatusActive).
		Relation("Price").
		Scan(ctx)
	if err != nil {
		return nil
	}

	return &sub
}

// GetAllSubscriptionsByUserID retrieves all subscriptions for a user
func (suite *TestContainerSuite) GetAllSubscriptionsByUserID(userID string) []*models.Subscription {
	suite.t.Helper()
	ctx := context.Background()

	var subs []*models.Subscription
	err := suite.BunDB.NewSelect().
		Model(&subs).
		Where("user_id = ?", userID).
		Order("created_at DESC").
		Relation("Price").
		Scan(ctx)
	require.NoError(suite.t, err, "Failed to get subscriptions for user %s", userID)

	return subs
}

// GetPaymentsByUserID retrieves all payments for a user
func (suite *TestContainerSuite) GetPaymentsByUserID(userID string) []*models.Payment {
	suite.t.Helper()
	ctx := context.Background()

	var payments []*models.Payment
	err := suite.BunDB.NewSelect().
		Model(&payments).
		Where("user_id = ?", userID).
		Order("purchased_at DESC").
		Relation("Price").
		Scan(ctx)
	require.NoError(suite.t, err, "Failed to get payments for user %s", userID)

	return payments
}

// GetPaymentMethodsByUserID retrieves all payment methods for a user
func (suite *TestContainerSuite) GetPaymentMethodsByUserID(userID string) []*models.PaymentMethod {
	suite.t.Helper()
	ctx := context.Background()

	var pms []*models.PaymentMethod
	err := suite.BunDB.NewSelect().
		Model(&pms).
		Where("user_id = ?", userID).
		Order("created_at DESC").
		Scan(ctx)
	require.NoError(suite.t, err, "Failed to get payment methods for user %s", userID)

	return pms
}

// GetEntitlementsByUserID retrieves all active entitlements for a user
func (suite *TestContainerSuite) GetEntitlementsByUserID(userID string) []*models.Entitlement {
	suite.t.Helper()
	ctx := context.Background()
	now := time.Now()

	var ents []*models.Entitlement
	err := suite.BunDB.NewSelect().
		Model(&ents).
		Where("user_id = ?", userID).
		Where("start_at <= ?", now).
		Where("(end_at IS NULL OR end_at > ?)", now).
		Where("revoked_at IS NULL").
		Where("deleted_at IS NULL").
		Scan(ctx)
	require.NoError(suite.t, err, "Failed to get entitlements for user %s", userID)

	return ents
}

// GetNotificationsByUserID retrieves all notifications for a user
func (suite *TestContainerSuite) GetNotificationsByUserID(userID string) []*models.NotificationQueue {
	suite.t.Helper()
	ctx := context.Background()

	var notifs []*models.NotificationQueue
	err := suite.BunDB.NewSelect().
		Model(&notifs).
		Where("user_id = ?", userID).
		Order("created_at DESC").
		Scan(ctx)
	require.NoError(suite.t, err, "Failed to get notifications for user %s", userID)

	return notifs
}

// CountUnreadNotifications returns the count of unread notifications for a user
func (suite *TestContainerSuite) CountUnreadNotifications(userID string) int {
	suite.t.Helper()
	ctx := context.Background()

	count, err := suite.BunDB.NewSelect().
		Model((*models.NotificationQueue)(nil)).
		Where("user_id = ?", userID).
		Where("seen = ?", false).
		Count(ctx)
	require.NoError(suite.t, err, "Failed to count unread notifications for user %s", userID)

	return count
}

// GetSubscriptionByProcessorID retrieves a subscription by processor subscription ID
func (suite *TestContainerSuite) GetSubscriptionByProcessorID(processorSubID string) *models.Subscription {
	suite.t.Helper()
	ctx := context.Background()

	var sub models.Subscription
	err := suite.BunDB.NewSelect().
		Model(&sub).
		Where("processor_subscription_id = ?", processorSubID).
		Relation("Price").
		Scan(ctx)
	if err != nil {
		return nil
	}

	return &sub
}

// CreateProfileUser creates a profile user for testing CCBill webhook resolution.
// CCBill webhooks include the username, which we resolve to user_id via profiles.users.
func (suite *TestContainerSuite) CreateProfileUser(userID string, username string) {
	suite.t.Helper()
	ctx := context.Background()
	now := time.Now()

	// Insert into profiles.users table (profiles schema, not billing schema)
	_, err := suite.BunDB.NewRaw(`
		INSERT INTO profiles.users (id, username, email, email_verified, is_active, created_at, updated_at)
		VALUES (?, ?, ?, true, true, ?, ?)
		ON CONFLICT (id) DO UPDATE SET username = EXCLUDED.username, updated_at = EXCLUDED.updated_at
	`, userID, username, username+"@test.example.com", now, now).Exec(ctx)
	require.NoError(suite.t, err, "Failed to create profile user")
}

// SeedCCBillTestData seeds all data needed for CCBill webhook replay tests.
// This creates profile users that connect webhook payloads to test users.
// CCBill webhooks include usernames which are resolved via profiles.users.
func (suite *TestContainerSuite) SeedCCBillTestData() {
	suite.t.Helper()

	// Create profile users (usernames from webhooks → test user IDs)
	// CCBillTestUsername is used in newsalesuccess.json
	suite.CreateProfileUser(CCBillTestUserID, CCBillTestUsername)
	// CCBillTestUsername2 is used in other webhooks (upgrade, reactivation, billingdatechange, etc.)
	suite.CreateProfileUser(CCBillTestUserID2, CCBillTestUsername2)
}

// SeedCCBillTestDataWithSubscription seeds CCBill test data including an active subscription
// This is needed for tests that require an existing subscription (renewal, cancellation, etc.)
func (suite *TestContainerSuite) SeedCCBillTestDataWithSubscription() *models.Subscription {
	suite.t.Helper()

	// First seed the basic test data
	suite.SeedCCBillTestData()

	// Get the monthly price (which has the CCBillTestFlexID)
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	// Create an active subscription with the processor subscription ID from saved webhooks
	return suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:         CCBillTestUserID,
		PriceID:        priceID,
		Status:         models.StatusActive,
		Processor:      models.ProcessorCCBill,
		ProcessorSubID: CCBillTestSubscriptionID,
	})
}

// CleanupSubscriptionsForUser deletes all subscriptions for a user
// Use this for test isolation when tests share the same suite
func (suite *TestContainerSuite) CleanupSubscriptionsForUser(userID string) {
	suite.t.Helper()
	ctx := context.Background()

	// Also delete entitlements for this user
	_, _ = suite.BunDB.NewDelete().
		Model((*models.Entitlement)(nil)).
		Where("user_id = ?", userID).
		Exec(ctx)

	// Delete subscriptions
	_, err := suite.BunDB.NewDelete().
		Model((*models.Subscription)(nil)).
		Where("user_id = ?", userID).
		Exec(ctx)
	if err != nil {
		suite.t.Logf("Warning: failed to cleanup subscriptions for user %s: %v", userID, err)
	}
}

// Helper functions for pointers
func intPtr(i int) *int {
	return &i
}

func strPtr(s string) *string {
	return &s
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func boolPtr(b bool) *bool {
	return &b
}
