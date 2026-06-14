//go:build integration

package money_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

func runGrantSubscriptionCredits_Idempotent_PerPeriod(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	q := gen.New(pool)
	dbtest.EnsureTestTenant(ctx, t, pool)
	ctx = dbtest.WithTestTenant(ctx)

	now := time.Now().UTC().Truncate(time.Second)
	periodEnd := now.Add(30 * 24 * time.Hour)

	// The grant spec key is just a label now (#472: money has no credit_type).
	grantLabel := "test_credits_" + uuid.New().String()
	productID := uuid.New()
	subID := uuid.New()
	userID := uuid.New().String()
	tenantSubjectID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)

	// Unit "USD" so the grant deposits into the USD money balance.
	creditsSpec, err := json.Marshal(models.CreditsSpec{
		grantLabel: {Unit: "USD", Amount: 100, Cadence: models.CreditGrantCadencePerRenewal},
	})
	require.NoError(t, err)
	desc := "Test"
	_, err = q.CreateProduct(ctx, gen.CreateProductParams{
		ID:          productID,
		Slug:        "test_product_" + uuid.New().String(),
		DisplayName: "Test Product",
		Description: &desc,
		CreditsSpec: creditsSpec,
		Status:      string(models.CatalogStatusActive),
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	require.NoError(t, err)

	priceID := uuid.New()
	cycle := int32(30)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID:               priceID,
		ProductID:        productID,
		Status:           string(models.CatalogStatusActive),
		Amount:           100,
		Currency:         "USD",
		BillingCycleDays: &cycle,
		CreatedAt:        now,
		UpdatedAt:        now,
	})
	require.NoError(t, err)

	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID:                      subID,
		CustomerID:              tenantSubjectID,
		ProductID:               productID,
		PriceID:                 &priceID,
		Status:                  string(models.StatusActive),
		Processor:               string(models.ProcessorStripe),
		ProcessorSubscriptionID: "sub_test_" + uuid.New().String(),
		CurrentPeriodStartsAt:   &now,
		CurrentPeriodEndsAt:     &periodEnd,
		StartedAt:               now,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_blocks WHERE customer_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_transactions WHERE customer_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_balances WHERE customer_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	moneySvc := money.NewMoneyService(dbi)
	moneySvc.SetClock(nil)

	require.NoError(t, moneySvc.GrantSubscriptionCredits(ctx, money.GrantSubscriptionCreditsParams{
		SubscriptionID: subID,
		PeriodEnd:      periodEnd,
		Cadence:        models.CreditGrantCadencePerRenewal,
		Source:         "subscription_renewal",
	}))
	require.NoError(t, moneySvc.GrantSubscriptionCredits(ctx, money.GrantSubscriptionCreditsParams{
		SubscriptionID: subID,
		PeriodEnd:      periodEnd,
		Cadence:        models.CreditGrantCadencePerRenewal,
		Source:         "subscription_renewal",
	}))

	var depositCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.money_transactions
		 WHERE customer_id = $1 AND currency = 'USD'
		   AND transaction_type = 'deposit' AND source = 'subscription_renewal'`,
		tenantSubjectID).Scan(&depositCount))
	require.Equal(t, 1, depositCount)

	// Deterministic grant SourceID: openrails:sub_credit_grant:<cadence>:<subID>:<label>:<periodEnd>.
	grantKey := fmt.Sprintf("openrails:sub_credit_grant:%s:%s:%s:%s",
		models.CreditGrantCadencePerRenewal, subID, grantLabel, periodEnd.UTC().Format(time.RFC3339Nano))
	expectedGrantID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(grantKey))

	dep := new(models.MoneyTransaction)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT source_id FROM openrails.money_transactions
		 WHERE customer_id = $1 AND currency = 'USD'
		   AND transaction_type = 'deposit' AND source = 'subscription_renewal'
		 LIMIT 1`,
		tenantSubjectID).Scan(&dep.SourceID))
	require.NotNil(t, dep.SourceID)
	require.Equal(t, expectedGrantID.String(), *dep.SourceID)

	bal := new(models.MoneyBalance)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT balance FROM openrails.money_balances
		 WHERE customer_id = $1 AND currency = 'USD'
		 LIMIT 1`,
		tenantSubjectID).Scan(&bal.Balance))
	require.Equal(t, int64(100), bal.Balance)
}

func TestGrantSubscriptionCredits_Idempotent_PerPeriod(t *testing.T) {
	runGrantSubscriptionCredits_Idempotent_PerPeriod(t)
}

func TestGrantSubscriptionCredits_ReplaySafety_StripeStyle(t *testing.T) {
	// This simulates replayed “renewal success” processing (e.g., Stripe invoice.paid re-delivery)
	// by calling GrantSubscriptionCredits twice for the same (subscription_id, grant label, period_end).
	runGrantSubscriptionCredits_Idempotent_PerPeriod(t)
}

func TestGrantSubscriptionCredits_MixedCadence(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	q := gen.New(pool)
	dbtest.EnsureTestTenant(ctx, t, pool)
	ctx = dbtest.WithTestTenant(ctx)

	now := time.Now().UTC().Truncate(time.Second)
	periodEnd := now.Add(30 * 24 * time.Hour)

	// Two grant labels, both crediting the SAME USD money balance (#472: money has
	// no credit_type — there is one balance per currency).
	onceLabel := "test_once_" + uuid.New().String()
	renewLabel := "test_renew_" + uuid.New().String()
	productID := uuid.New()
	subID := uuid.New()
	userID := uuid.New().String()
	tenantSubjectID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)

	creditsSpec, err := json.Marshal(models.CreditsSpec{
		onceLabel:  {Unit: "USD", Amount: 10, Cadence: models.CreditGrantCadenceOnce},
		renewLabel: {Unit: "USD", Amount: 100, Cadence: models.CreditGrantCadencePerRenewal},
	})
	require.NoError(t, err)
	desc := "Test"
	_, err = q.CreateProduct(ctx, gen.CreateProductParams{
		ID:          productID,
		Slug:        "test_product_" + uuid.New().String(),
		DisplayName: "Test Product",
		Description: &desc,
		CreditsSpec: creditsSpec,
		Status:      string(models.CatalogStatusActive),
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	require.NoError(t, err)

	priceID := uuid.New()
	cycle := int32(30)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID:               priceID,
		ProductID:        productID,
		Status:           string(models.CatalogStatusActive),
		Amount:           100,
		Currency:         "USD",
		BillingCycleDays: &cycle,
		CreatedAt:        now,
		UpdatedAt:        now,
	})
	require.NoError(t, err)

	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID:                      subID,
		CustomerID:              tenantSubjectID,
		ProductID:               productID,
		PriceID:                 &priceID,
		Status:                  string(models.StatusActive),
		Processor:               string(models.ProcessorStripe),
		ProcessorSubscriptionID: "sub_test_" + uuid.New().String(),
		CurrentPeriodStartsAt:   &now,
		CurrentPeriodEndsAt:     &periodEnd,
		StartedAt:               now,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_transactions WHERE customer_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_blocks WHERE customer_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_balances WHERE customer_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	moneySvc := money.NewMoneyService(dbi)

	// Once grant should apply once.
	require.NoError(t, moneySvc.GrantSubscriptionCredits(ctx, money.GrantSubscriptionCreditsParams{
		SubscriptionID: subID,
		PeriodEnd:      periodEnd,
		Cadence:        models.CreditGrantCadenceOnce,
		Source:         "subscription_initial",
	}))
	require.NoError(t, moneySvc.GrantSubscriptionCredits(ctx, money.GrantSubscriptionCreditsParams{
		SubscriptionID: subID,
		PeriodEnd:      periodEnd,
		Cadence:        models.CreditGrantCadenceOnce,
		Source:         "subscription_initial",
	}))

	// Renewal grant should apply once.
	require.NoError(t, moneySvc.GrantSubscriptionCredits(ctx, money.GrantSubscriptionCreditsParams{
		SubscriptionID: subID,
		PeriodEnd:      periodEnd,
		Cadence:        models.CreditGrantCadencePerRenewal,
		Source:         "subscription_renewal",
	}))
	require.NoError(t, moneySvc.GrantSubscriptionCredits(ctx, money.GrantSubscriptionCreditsParams{
		SubscriptionID: subID,
		PeriodEnd:      periodEnd,
		Cadence:        models.CreditGrantCadencePerRenewal,
		Source:         "subscription_renewal",
	}))

	// Both labels credit the one USD balance: 10 (once) + 100 (renewal), each once.
	bal := new(models.MoneyBalance)
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT balance FROM openrails.money_balances WHERE customer_id = $1 AND currency = 'USD'",
		tenantSubjectID).Scan(&bal.Balance))
	require.Equal(t, int64(110), bal.Balance)
}
