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
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/stretchr/testify/require"
)

func runGrantSubscriptionCredits_Idempotent_PerPeriod(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	q := gen.New(pool)

	// Ensure migration table exists; if not, fail fast with a helpful message.
	var exists bool
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='billing' AND table_name='credit_blocks')").
		Scan(&exists))
	if !exists {
		t.Skip("openrails.credit_blocks not found; run migrations before integration tests")
	}

	now := time.Now().UTC().Truncate(time.Second)
	periodEnd := now.Add(30 * 24 * time.Hour)

	creditTypeName := "test_credits_" + uuid.New().String()
	creditTypeID := uuid.New()
	productID := uuid.New()
	subID := uuid.New()
	userID := uuid.New().String()
	tenantSubjectID := dbtest.EnsureTenantSubjectIDPgx(ctx, t, pool, userID)

	_, err := q.CreateCreditType(ctx, gen.CreateCreditTypeParams{
		ID:            creditTypeID,
		Name:          creditTypeName,
		DisplayName:   "Test Credits",
		Unit:          "units",
		DecimalPlaces: 0,
		IsActive:      true,
		CreatedAt:     now,
	})
	require.NoError(t, err)

	creditsSpec, err := json.Marshal(models.CreditsSpec{
		creditTypeName: {Amount: 100, Cadence: models.CreditGrantCadencePerRenewal},
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
		TenantSubjectID:         tenantSubjectID,
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
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.credit_blocks WHERE tenant_subject_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.credit_transactions WHERE tenant_subject_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.credit_balances WHERE tenant_subject_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.credit_types WHERE id = $1", creditTypeID)
	})

	creditsSvc := credits.NewMoneyService(dbi)
	creditsSvc.SetClock(nil)

	require.NoError(t, creditsSvc.GrantSubscriptionCredits(ctx, credits.GrantSubscriptionCreditsParams{
		SubscriptionID: subID,
		PeriodEnd:      periodEnd,
		Cadence:        models.CreditGrantCadencePerRenewal,
		Source:         "subscription_renewal",
	}))
	require.NoError(t, creditsSvc.GrantSubscriptionCredits(ctx, credits.GrantSubscriptionCreditsParams{
		SubscriptionID: subID,
		PeriodEnd:      periodEnd,
		Cadence:        models.CreditGrantCadencePerRenewal,
		Source:         "subscription_renewal",
	}))

	var depositCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.credit_transactions
		 WHERE tenant_subject_id = $1 AND credit_type_id = $2
		   AND transaction_type = 'deposit' AND source = 'subscription_renewal'`,
		tenantSubjectID, creditTypeID).Scan(&depositCount))
	require.Equal(t, 1, depositCount)

	expectedGrantID := uuid.NewSHA1(
		uuid.NameSpaceOID,
		[]byte(fmt.Sprintf("openrails:sub_credit_grant:%s:%s:%s:%s", models.CreditGrantCadencePerRenewal, subID, creditTypeID, periodEnd.UTC().Format(time.RFC3339Nano))),
	)

	dep := new(models.CreditTransaction)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT source_id FROM openrails.credit_transactions
		 WHERE tenant_subject_id = $1 AND credit_type_id = $2
		   AND transaction_type = 'deposit' AND source = 'subscription_renewal'
		 LIMIT 1`,
		tenantSubjectID, creditTypeID).Scan(&dep.SourceID))
	require.NotNil(t, dep.SourceID)
	require.Equal(t, expectedGrantID.String(), *dep.SourceID)

	bal := new(models.CreditBalance)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT balance FROM openrails.credit_balances
		 WHERE tenant_subject_id = $1 AND credit_type_id = $2
		 LIMIT 1`,
		tenantSubjectID, creditTypeID).Scan(&bal.Balance))
	require.Equal(t, int64(100), bal.Balance)
}

func TestGrantSubscriptionCredits_Idempotent_PerPeriod(t *testing.T) {
	runGrantSubscriptionCredits_Idempotent_PerPeriod(t)
}

func TestGrantSubscriptionCredits_ReplaySafety_StripeStyle(t *testing.T) {
	// This simulates replayed “renewal success” processing (e.g., Stripe invoice.paid re-delivery)
	// by calling GrantSubscriptionCredits twice for the same (subscription_id, credit_type, period_end).
	runGrantSubscriptionCredits_Idempotent_PerPeriod(t)
}

func TestGrantSubscriptionCredits_MixedCadence(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	q := gen.New(pool)

	var exists bool
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='billing' AND table_name='credit_blocks')").
		Scan(&exists))
	if !exists {
		t.Skip("openrails.credit_blocks not found; run migrations before integration tests")
	}

	now := time.Now().UTC().Truncate(time.Second)
	periodEnd := now.Add(30 * 24 * time.Hour)

	ctOnceName := "test_once_" + uuid.New().String()
	ctRenewName := "test_renew_" + uuid.New().String()
	ctOnceID := uuid.New()
	ctRenewID := uuid.New()
	productID := uuid.New()
	subID := uuid.New()
	userID := uuid.New().String()
	tenantSubjectID := dbtest.EnsureTenantSubjectIDPgx(ctx, t, pool, userID)

	_, err := q.CreateCreditType(ctx, gen.CreateCreditTypeParams{
		ID:            ctOnceID,
		Name:          ctOnceName,
		DisplayName:   "Once Credits",
		Unit:          "units",
		DecimalPlaces: 0,
		IsActive:      true,
		CreatedAt:     now,
	})
	require.NoError(t, err)
	_, err = q.CreateCreditType(ctx, gen.CreateCreditTypeParams{
		ID:            ctRenewID,
		Name:          ctRenewName,
		DisplayName:   "Renew Credits",
		Unit:          "units",
		DecimalPlaces: 0,
		IsActive:      true,
		CreatedAt:     now,
	})
	require.NoError(t, err)

	creditsSpec, err := json.Marshal(models.CreditsSpec{
		ctOnceName:  {Amount: 10, Cadence: models.CreditGrantCadenceOnce},
		ctRenewName: {Amount: 100, Cadence: models.CreditGrantCadencePerRenewal},
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
		TenantSubjectID:         tenantSubjectID,
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
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.credit_transactions WHERE tenant_subject_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.credit_balances WHERE tenant_subject_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.credit_types WHERE id = ANY($1::uuid[])", []uuid.UUID{ctOnceID, ctRenewID})
	})

	creditsSvc := credits.NewMoneyService(dbi)

	// Once grant should apply once.
	require.NoError(t, creditsSvc.GrantSubscriptionCredits(ctx, credits.GrantSubscriptionCreditsParams{
		SubscriptionID: subID,
		PeriodEnd:      periodEnd,
		Cadence:        models.CreditGrantCadenceOnce,
		Source:         "subscription_initial",
	}))
	require.NoError(t, creditsSvc.GrantSubscriptionCredits(ctx, credits.GrantSubscriptionCreditsParams{
		SubscriptionID: subID,
		PeriodEnd:      periodEnd,
		Cadence:        models.CreditGrantCadenceOnce,
		Source:         "subscription_initial",
	}))

	// Renewal grant should apply once.
	require.NoError(t, creditsSvc.GrantSubscriptionCredits(ctx, credits.GrantSubscriptionCreditsParams{
		SubscriptionID: subID,
		PeriodEnd:      periodEnd,
		Cadence:        models.CreditGrantCadencePerRenewal,
		Source:         "subscription_renewal",
	}))
	require.NoError(t, creditsSvc.GrantSubscriptionCredits(ctx, credits.GrantSubscriptionCreditsParams{
		SubscriptionID: subID,
		PeriodEnd:      periodEnd,
		Cadence:        models.CreditGrantCadencePerRenewal,
		Source:         "subscription_renewal",
	}))

	// Total should be 10 + 100 across two types, each exactly once.
	balOnce := new(models.CreditBalance)
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT balance FROM openrails.credit_balances WHERE tenant_subject_id = $1 AND credit_type_id = $2",
		tenantSubjectID, ctOnceID).Scan(&balOnce.Balance))
	require.Equal(t, int64(10), balOnce.Balance)
	balRenew := new(models.CreditBalance)
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT balance FROM openrails.credit_balances WHERE tenant_subject_id = $1 AND credit_type_id = $2",
		tenantSubjectID, ctRenewID).Scan(&balRenew.Balance))
	require.Equal(t, int64(100), balRenew.Balance)
}
