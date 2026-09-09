//go:build integration

package money_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
)

func runGrantSubscriptionCredits_Idempotent_PerPeriod(t *testing.T) {

	ctx := context.Background()
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	q := gen.New(pool)
	dbtest.EnsureTestMerchant(ctx, t, pool)
	ctx = dbtest.WithTestMerchant(ctx)

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
		MerchantID:  dbtest.TestMerchantID.UUID(),
		Key:         "test_product_" + uuid.New().String(),
		DisplayName: "Test Product",
		Description: &desc,
		CreditsSpec: creditsSpec,
		Archived:    false,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	require.NoError(t, err)

	priceID := uuid.New()
	cycleHours := int32(30 * 24)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID:                  priceID,
		MerchantID:          dbtest.TestMerchantID.UUID(),
		ProductID:           productID,
		Archived:            false,
		Amount:              100,
		Currency:            "USD",
		AccessDurationHours: &cycleHours,
		AutoRenew:           true,
		CreatedAt:           now,
		UpdatedAt:           now,
	})
	require.NoError(t, err)

	pspID := dbtest.EnsureTestPSP(ctx, t, pool, dbtest.TestMerchantID.UUID(), string(models.RailStripe))
	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID:                    subID,
		MerchantID:            dbtest.TestMerchantID.UUID(),
		CustomerID:            tenantSubjectID,
		ProductID:             productID,
		PriceID:               &priceID,
		Status:                string(models.StatusActive),
		Rail:                  string(models.RailStripe),
		PspID:                 pspID,
		RailSubscriptionID:    "sub_test_" + uuid.New().String(),
		CurrentPeriodStartsAt: &now,
		CurrentPeriodEndsAt:   &periodEnd,
		StartedAt:             now,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
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

	// A deposit is now a #514 credit grant (the lot). #491: source_id IS the
	// natural-key string itself (uuidv7 pk + UNIQUE natural key), no uuidv5.
	grantKey := fmt.Sprintf("openrails:sub_credit_grant:%s:%s:%s:%s",
		models.CreditGrantCadencePerRenewal, subID, grantLabel, periodEnd.UTC().Format(time.RFC3339Nano))

	var depositCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.grants
		 WHERE customer_id = $1 AND currency = 'USD'
		   AND kind = 'credit' AND event = 'grant' AND source_id = $2`,
		tenantSubjectID, grantKey).Scan(&depositCount))
	require.Equal(t, 1, depositCount, "renewal grants exactly one credit lot (idempotent)")

	bal, err := moneySvc.GetBalance(ctx, tenantSubjectID.String(), money.DefaultCurrency)
	require.NoError(t, err)
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

	ctx := context.Background()
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	q := gen.New(pool)
	dbtest.EnsureTestMerchant(ctx, t, pool)
	ctx = dbtest.WithTestMerchant(ctx)

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
		MerchantID:  dbtest.TestMerchantID.UUID(),
		Key:         "test_product_" + uuid.New().String(),
		DisplayName: "Test Product",
		Description: &desc,
		CreditsSpec: creditsSpec,
		Archived:    false,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	require.NoError(t, err)

	priceID := uuid.New()
	cycleHours := int32(30 * 24)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID:                  priceID,
		MerchantID:          dbtest.TestMerchantID.UUID(),
		ProductID:           productID,
		Archived:            false,
		Amount:              100,
		Currency:            "USD",
		AccessDurationHours: &cycleHours,
		AutoRenew:           true,
		CreatedAt:           now,
		UpdatedAt:           now,
	})
	require.NoError(t, err)

	pspID := dbtest.EnsureTestPSP(ctx, t, pool, dbtest.TestMerchantID.UUID(), string(models.RailStripe))
	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID:                    subID,
		MerchantID:            dbtest.TestMerchantID.UUID(),
		CustomerID:            tenantSubjectID,
		ProductID:             productID,
		PriceID:               &priceID,
		Status:                string(models.StatusActive),
		Rail:                  string(models.RailStripe),
		PspID:                 pspID,
		RailSubscriptionID:    "sub_test_" + uuid.New().String(),
		CurrentPeriodStartsAt: &now,
		CurrentPeriodEndsAt:   &periodEnd,
		StartedAt:             now,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
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
	bal, err := moneySvc.GetBalance(ctx, tenantSubjectID.String(), money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(110), bal.Balance)
}

type lifecycleCreditsFixture struct {
	ctx        context.Context
	pool       *pgxpool.Pool
	lifecycle  *subscriptions.SubscriptionLifecycleService
	money      *money.MoneyService
	productID  uuid.UUID
	priceID    uuid.UUID
	customerID uuid.UUID
	pspID      uuid.UUID
}

func newLifecycleCreditsFixture(t *testing.T, credits models.CreditsSpec) *lifecycleCreditsFixture {
	t.Helper()
	baseCtx := context.Background()
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(baseCtx, t, pool)
	pspID := dbtest.EnsureTestPSP(baseCtx, t, pool, dbtest.TestMerchantID.UUID(), string(models.RailNMI))
	ctx := db.WithPSPID(dbtest.WithTestMerchant(baseCtx), pspID)
	q := gen.New(pool)
	now := time.Now().UTC().Truncate(time.Second)
	productID, priceID := uuid.New(), uuid.New()
	customerID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, uuid.NewString())

	creditsJSON, err := json.Marshal(credits)
	require.NoError(t, err)
	description := "Lifecycle credits transaction test"
	_, err = q.CreateProduct(ctx, gen.CreateProductParams{
		ID: productID, MerchantID: dbtest.TestMerchantID.UUID(),
		Key: "lifecycle_credits_" + uuid.NewString(), DisplayName: "Lifecycle Credits",
		Description: &description, CreditsSpec: creditsJSON,
		Archived: false, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	cycleHours := int32(30 * 24)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID: priceID, MerchantID: dbtest.TestMerchantID.UUID(), ProductID: productID,
		Amount: 999, Currency: "USD", AccessDurationHours: &cycleHours,
		AutoRenew: true, Archived: false, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	productSvc := catalog.NewProductService(dbi)
	priceSvc := catalog.NewPriceService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi)
	notificationSvc := subscriptions.NewNotificationService(dbi, nil)
	paymentSvc := payments.NewPaymentService(dbi)
	lifecycle := subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, notificationSvc, paymentSvc)
	moneySvc := money.NewMoneyService(dbi)
	lifecycle.SetCreditGranter(moneySvc)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.ledger_transfers WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.grants WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payments WHERE price_id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.notification_queue WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE product_id = $1", productID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})
	return &lifecycleCreditsFixture{
		ctx: ctx, pool: pool, lifecycle: lifecycle, money: moneySvc,
		productID: productID, priceID: priceID, customerID: customerID, pspID: pspID,
	}
}

func (f *lifecycleCreditsFixture) countRows(t *testing.T, query string, arg any) int {
	t.Helper()
	var count int
	require.NoError(t, f.pool.QueryRow(f.ctx, query, arg).Scan(&count))
	return count
}

func TestSubscriptionLifecycleCreditsAreAtomicAndReplaySafe(t *testing.T) {
	t.Run("grant failure rolls back activation", func(t *testing.T) {
		f := newLifecycleCreditsFixture(t, models.CreditsSpec{
			"broken": {Unit: "NOPE", Amount: 10, Cadence: models.CreditGrantCadenceOnce},
		})
		railSubID, transactionID := "atomic_create_"+uuid.NewString(), "atomic_txn_"+uuid.NewString()
		periodStart := time.Now().UTC().Truncate(time.Second)
		periodEnd := periodStart.Add(30 * 24 * time.Hour)
		pendingID := uuid.New()
		_, err := gen.New(f.pool).CreateSubscription(f.ctx, gen.CreateSubscriptionParams{
			ID: pendingID, MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: f.customerID,
			ProductID: f.productID, PriceID: &f.priceID, Status: string(models.StatusPending),
			Rail: string(models.RailNMI), PspID: f.pspID, RailSubscriptionID: railSubID,
			StartedAt: periodStart, CreatedAt: periodStart, UpdatedAt: periodStart,
		})
		require.NoError(t, err)

		_, err = f.lifecycle.CreateMembership(f.ctx, &subscriptions.CreateMembershipParams{
			UserID: f.customerID.String(), PriceID: f.priceID, Rail: models.RailNMI,
			RailSubscriptionID: &railSubID, TransactionID: transactionID,
			Amount: 999, AmountProvided: true, Currency: "USD",
			CurrentPeriodStartsAt: &periodStart, CurrentPeriodEndsAt: &periodEnd,
		})
		require.Error(t, err)
		pending, loadErr := gen.New(f.pool).GetSubscriptionByID(f.ctx, pendingID)
		require.NoError(t, loadErr)
		require.Equal(t, models.StatusPending, models.SubscriptionStatus(pending.Status))
		require.Nil(t, pending.CurrentPeriodEndsAt, "failed credit grant must roll back activation")
		require.Zero(t, f.countRows(t, "SELECT count(*) FROM openrails.payments WHERE transaction_id = $1", transactionID))
		require.Zero(t, f.countRows(t, "SELECT count(*) FROM openrails.grants WHERE customer_id = $1", f.customerID))
	})

	t.Run("activation and renewal commit one lot each", func(t *testing.T) {
		f := newLifecycleCreditsFixture(t, models.CreditsSpec{
			"initial": {Unit: "USD", Amount: 10, Cadence: models.CreditGrantCadenceOnce},
			"renewal": {Unit: "USD", Amount: 100, Cadence: models.CreditGrantCadencePerRenewal},
		})
		railSubID := "atomic_success_" + uuid.NewString()
		periodStart := time.Now().UTC().Truncate(time.Second)
		initialEnd := periodStart.Add(30 * 24 * time.Hour)
		initialTxn := "initial_" + uuid.NewString()
		sub, err := f.lifecycle.CreateMembership(f.ctx, &subscriptions.CreateMembershipParams{
			UserID: f.customerID.String(), PriceID: f.priceID, Rail: models.RailNMI,
			RailSubscriptionID: &railSubID, TransactionID: initialTxn,
			Amount: 999, AmountProvided: true, Currency: "USD",
			CurrentPeriodStartsAt: &periodStart, CurrentPeriodEndsAt: &initialEnd,
		})
		require.NoError(t, err)
		require.Equal(t, 1, f.countRows(t, "SELECT count(*) FROM openrails.grants WHERE customer_id = $1", f.customerID))
		balance, err := f.money.GetBalance(f.ctx, f.customerID.String(), money.DefaultCurrency)
		require.NoError(t, err)
		require.Equal(t, int64(10), balance.Balance)

		renewalEnd := initialEnd.Add(30 * 24 * time.Hour)
		renewalTxn := "renewal_" + uuid.NewString()
		renewal := &subscriptions.RenewMembershipParams{
			Rail: models.RailNMI, RailSubscriptionID: railSubID, TransactionID: renewalTxn,
			Amount: 999, AmountProvided: true, Currency: "USD",
			CurrentPeriodStartsAt: &initialEnd, CurrentPeriodEndsAt: &renewalEnd,
		}
		require.NoError(t, f.lifecycle.RenewMembership(f.ctx, renewal))
		require.NoError(t, f.lifecycle.RenewMembership(f.ctx, renewal), "same provider transaction must be replay-safe")

		updated, err := gen.New(f.pool).GetSubscriptionByID(f.ctx, sub.ID)
		require.NoError(t, err)
		require.Equal(t, renewalEnd, updated.CurrentPeriodEndsAt.UTC())
		require.Equal(t, 2, f.countRows(t, "SELECT count(*) FROM openrails.grants WHERE customer_id = $1", f.customerID))
		require.Equal(t, 2, f.countRows(t, "SELECT count(*) FROM openrails.payments WHERE subscription_id = $1", sub.ID))
		balance, err = f.money.GetBalance(f.ctx, f.customerID.String(), money.DefaultCurrency)
		require.NoError(t, err)
		require.Equal(t, int64(110), balance.Balance)
	})
}

func TestGrantPurchaseCredits_OnlyOnceCadence(t *testing.T) {

	ctx := context.Background()
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(ctx, t, pool)
	ctx = dbtest.WithTestMerchant(ctx)

	userID := uuid.New().String()
	tenantSubjectID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	payer := identity.CustomerID(tenantSubjectID)

	moneySvc := money.NewMoneyService(dbi)
	paymentID := uuid.New()
	require.NoError(t, moneySvc.GrantPurchaseCredits(ctx, money.GrantPurchaseCreditsParams{
		Payer:     payer,
		PaymentID: paymentID,
		Source:    "purchase",
		Spec: models.CreditsSpec{
			"once":  {Unit: "USD", Amount: 10, Cadence: models.CreditGrantCadenceOnce},
			"renew": {Unit: "USD", Amount: 100, Cadence: models.CreditGrantCadencePerRenewal},
		},
	}))
	require.NoError(t, moneySvc.GrantPurchaseCredits(ctx, money.GrantPurchaseCreditsParams{
		Payer:     payer,
		PaymentID: paymentID,
		Source:    "purchase",
		Spec: models.CreditsSpec{
			"once":  {Unit: "USD", Amount: 10, Cadence: models.CreditGrantCadenceOnce},
			"renew": {Unit: "USD", Amount: 100, Cadence: models.CreditGrantCadencePerRenewal},
		},
	}))

	bal, err := moneySvc.GetBalance(ctx, tenantSubjectID.String(), money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(10), bal.Balance)
}
