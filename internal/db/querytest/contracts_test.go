//go:build integration

package querytest

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
)

func TestQueryContractsHighValueBillingDomains(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedSuperuserPGXPool(t, "querytest asserts the QUERY PREDICATE with RLS deliberately disabled (SEC-18) and TRUNCATE/ANALYZEs the perf corpus")
	dbtest.EnsureTestMerchant(ctx, t, pool)
	q := gen.New(pool)

	now := time.Now().UTC().Truncate(time.Second)
	merchantID := dbtest.TestMerchantID.UUID()
	customerID := uuid.New()
	customerSubject := customerID.String()
	productID := uuid.New()
	priceID := uuid.New()
	subscriptionID := uuid.New()

	display := "CCBill Test"
	account, err := q.UpsertPSP(ctx, gen.UpsertPSPParams{
		MerchantID:  merchantID,
		Rail:        "ccbill",
		Environment: strptr("test"),
		AccountID:   "acct_" + subscriptionID.String(),
		Key:         &display,
		Evidence:    []byte(`{"source":"query-contract"}`),
	})
	require.NoError(t, err)
	require.False(t, account.Archived)

	_, err = q.EnsureCustomer(ctx, gen.EnsureCustomerParams{
		ID:         customerID,
		MerchantID: merchantID,
		Subject:    &customerSubject,
	})
	require.NoError(t, err)
	customers, err := q.LookupCustomerIDsBySubjects(ctx, gen.LookupCustomerIDsBySubjectsParams{
		MerchantID: merchantID,
		Subjects:   []string{customerSubject},
	})
	require.NoError(t, err)
	require.Len(t, customers, 1)
	require.Equal(t, customerID, customers[0].ID)

	_, err = q.CreateProduct(ctx, gen.CreateProductParams{
		ID:               productID,
		MerchantID:       merchantID,
		Key:              "query-contract-" + productID.String(),
		DisplayName:      "Query Contract Premium",
		EntitlementsSpec: []byte(`["premium"]`),
		CreditsSpec:      []byte(`[]`),
		TierGroup:        strptr("premium"),
		TierRank:         10,
		CreatedAt:        now,
		UpdatedAt:        now,
	})
	require.NoError(t, err)

	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID:                  priceID,
		ProductID:           productID,
		MerchantID:          merchantID,
		Amount:              1999,
		Currency:            "USD",
		AccessDurationHours: int32ptr(30 * 24),
		AutoRenew:           true,
		PspLinks:            []byte(`{"ccbill":{"form":"948"}}`),
		CreatedAt:           now,
		UpdatedAt:           now,
	})
	require.NoError(t, err)

	product, err := q.GetProductByID(ctx, productID)
	require.NoError(t, err)
	require.Equal(t, "Query Contract Premium", product.DisplayName)

	prices, err := q.ListActivePricesByProductOrdered(ctx, productID)
	require.NoError(t, err)
	require.Len(t, prices, 1)
	require.Equal(t, int64(1999), prices[0].Amount)

	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID:                       subscriptionID,
		CustomerID:               customerID,
		ProductID:                productID,
		PriceID:                  &priceID,
		MerchantID:               merchantID,
		EntitlementsSpecSnapshot: []byte(`["premium"]`),
		CreditsSpecSnapshot:      []byte(`[]`),
		Status:                   "active",
		StartedAt:                now,
		CurrentPeriodStartsAt:    &now,
		CurrentPeriodEndsAt:      timeptr(now.Add(30 * 24 * time.Hour)),
		Rail:                     "ccbill",
		RailSubscriptionID:       "sub_" + subscriptionID.String(),
		GatewayResponse:          []byte(`{}`),
		CreatedAt:                now,
		UpdatedAt:                now,
	})
	require.NoError(t, err)

	activeSub, err := q.GetActiveSubscriptionByCustomerAt(ctx, gen.GetActiveSubscriptionByCustomerAtParams{
		CustomerID: customerID,
		Now:        now,
	})
	require.NoError(t, err)
	require.Equal(t, subscriptionID, activeSub.ID)

	err = q.UpsertRailCustomerAccount(ctx, gen.UpsertRailCustomerAccountParams{
		ID:         uuid.New(),
		CustomerID: customerID,
		Rail:       "ccbill",
		AccountID:  "rail_customer_" + customerID.String(),
		CreatedAt:  now,
		UpdatedAt:  now,
		MerchantID: merchantID,
	})
	require.NoError(t, err)
	railCustomerID, err := q.GetRailCustomerAccountIDForMerchant(ctx, gen.GetRailCustomerAccountIDForMerchantParams{
		MerchantID: merchantID,
		CustomerID: customerID,
		Rail:       "ccbill",
	})
	require.NoError(t, err)
	require.Equal(t, "rail_customer_"+customerID.String(), railCustomerID)

	paymentMethodID := uuid.New()
	last4 := "4242"
	cardType := "visa"
	expiry := "12/30"
	_, err = q.CreatePaymentMethod(ctx, gen.CreatePaymentMethodParams{
		ID:                   paymentMethodID,
		CustomerID:           customerID,
		Rail:                 "ccbill",
		MerchantID:           merchantID,
		RailCustomerRef:      railCustomerID,
		RailMethodRef:        "method_" + paymentMethodID.String(),
		InitialTransactionID: "initial_" + paymentMethodID.String(),
		LastFour:             &last4,
		CardType:             &cardType,
		ExpiryDate:           &expiry,
		Metadata:             []byte(`{"contract":true}`),
		CreatedAt:            now,
		UpdatedAt:            now,
	})
	require.NoError(t, err)
	methods, err := q.ListPaymentMethodsByCustomer(ctx, customerID)
	require.NoError(t, err)
	require.Len(t, methods, 1)
	require.Equal(t, paymentMethodID, methods[0].ID)

	_, err = q.CreateEntitlement(ctx, gen.CreateEntitlementParams{
		Entitlement: "premium",
		StartAt:     now.Add(-time.Hour),
		SourceType:  "subscription",
		SourceID:    &subscriptionID,
		MerchantID:  merchantID,
		CustomerID:  customerID,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	require.NoError(t, err)

	ok, err := q.EntitlementExistsActive(ctx, gen.EntitlementExistsActiveParams{
		MerchantID:  merchantID,
		CustomerID:  customerID,
		Entitlement: "premium",
		At:          now,
	})
	require.NoError(t, err)
	require.True(t, ok)

	names, err := q.ListActiveEntitlementNamesMerchant(ctx, gen.ListActiveEntitlementNamesMerchantParams{
		MerchantID: merchantID,
		CustomerID: customerID,
		At:         now,
	})
	require.NoError(t, err)
	require.Equal(t, []string{"premium"}, names)

	_, err = q.CreatePaymentIfNotExists(ctx, gen.CreatePaymentIfNotExistsParams{
		ID:             uuid.New(),
		PriceID:        priceID,
		Rail:           string(models.RailCCBill),
		TransactionID:  "txn_" + subscriptionID.String(),
		Amount:         1999,
		ListAmount:     1999,
		MerchantID:     merchantID,
		Currency:       "USD",
		Status:         "completed",
		SubscriptionID: &subscriptionID,
		Metadata:       []byte(`{"contract":true}`),
		PurchasedAt:    now,
		CreatedAt:      now,
		CustomerID:     customerID,
	})
	require.NoError(t, err)

	latestPayment, err := q.GetLatestChargeBySubscriptionID(ctx, &subscriptionID)
	require.NoError(t, err)
	require.Equal(t, int64(1999), latestPayment.Amount)

	err = q.InsertMoneyAccountSettingsIfAbsent(ctx, gen.InsertMoneyAccountSettingsIfAbsentParams{
		MerchantID:  merchantID,
		CustomerID:  customerID,
		BillingMode: "prepaid",
		Currency:    "USD",
		Now:         now,
	})
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.ledger_accounts (
			id, merchant_id, customer_id, account_type, currency,
			debits_must_not_exceed_credits, credits_posted, debits_posted
		) VALUES ($1, $2, $3, 'customer_balance', 'USD', true, 5000, 1250)
	`, uuid.New(), merchantID, customerID)
	require.NoError(t, err)

	capacity, err := q.GetAdmissionCapacity(ctx, gen.GetAdmissionCapacityParams{
		MerchantID: merchantID,
		CustomerID: customerID,
		Currency:   "USD",
	})
	require.NoError(t, err)
	require.Equal(t, int64(3750), capacity.Balance)
	require.Equal(t, "prepaid", capacity.BillingMode)

	periodFrom := now.Add(-24 * time.Hour)
	periodTo := now
	invoiceID := uuid.New()
	invoiceNumber := "INV-" + invoiceID.String()
	dueAt := now.Add(7 * 24 * time.Hour)
	err = q.InsertInvoice(ctx, gen.InsertInvoiceParams{
		ID:               invoiceID,
		MerchantID:       merchantID,
		CustomerID:       customerID,
		Currency:         "USD",
		PeriodFrom:       periodFrom,
		PeriodTo:         periodTo,
		UsageTotal:       600,
		ClosingBalance:   600,
		InvoiceNumber:    &invoiceNumber,
		SubtotalAmount:   600,
		TotalAmount:      600,
		AmountDue:        600,
		LineItems:        []byte(`[]`),
		MoneyMovements:   []byte(`{}`),
		Status:           "open",
		CollectionMethod: "charge_automatically",
		DueAt:            &dueAt,
		CreatedAt:        now,
		UpdatedAt:        now,
	})
	require.NoError(t, err)
	invoice, err := q.GetInvoiceByPeriod(ctx, gen.GetInvoiceByPeriodParams{
		MerchantID: merchantID,
		CustomerID: customerID,
		PeriodFrom: periodFrom,
		PeriodTo:   periodTo,
		Currency:   "USD",
	})
	require.NoError(t, err)
	require.Equal(t, invoiceID, invoice.ID)

	resource := "query-contract-resource"
	err = q.InsertUsageEventIfAbsent(ctx, gen.InsertUsageEventIfAbsentParams{
		ID:         uuid.New(),
		MerchantID: merchantID,
		CustomerID: customerID,
		InvokerID:  "contract-invoker",
		Resource:   &resource,
		EventType:  "api_call",
		Currency:   "USD",
		Amount:     600,
		Source:     "contract",
		SourceID:   "usage-" + customerID.String(),
		Metadata:   []byte(`{}`),
		OccurredAt: now.Add(-time.Hour),
		CreatedAt:  now,
		Dimensions: []byte(`{"tokens":3}`),
	})
	require.NoError(t, err)
	usageTotals, err := q.AggregateUsageTotals(ctx, gen.AggregateUsageTotalsParams{
		MerchantID: merchantID,
		CustomerID: customerID,
		Currency:   "USD",
		FromAt:     periodFrom,
		ToAt:       periodTo.Add(time.Second),
	})
	require.NoError(t, err)
	require.Len(t, usageTotals, 1)
	require.Equal(t, int64(600), usageTotals[0].TotalAmount)
	usageDims, err := q.AggregateUsageDimensions(ctx, gen.AggregateUsageDimensionsParams{
		MerchantID: merchantID,
		CustomerID: customerID,
		Currency:   "USD",
		FromAt:     periodFrom,
		ToAt:       periodTo.Add(time.Second),
	})
	require.NoError(t, err)
	require.Len(t, usageDims, 1)
	require.Equal(t, int64(3), usageDims[0].Total)

	intent, err := q.EnqueueRailIntent(ctx, gen.EnqueueRailIntentParams{
		MerchantID:     merchantID,
		Rail:           "ccbill",
		IntentType:     "nmi_delete_subscription",
		SubscriptionID: &subscriptionID,
		PriceID:        &priceID,
		Payload:        []byte(`{"contract":true}`),
		IdempotencyKey: "intent-" + subscriptionID.String(),
		NextAttemptAt:  now,
		Origin:         "system",
		PspID:          &account.ID,
	})
	require.NoError(t, err)
	claimed, err := q.ClaimRailIntentByID(ctx, gen.ClaimRailIntentByIDParams{
		LeaseUntil: now.Add(time.Minute),
		ID:         intent.ID,
		Now:        now,
	})
	require.NoError(t, err)
	require.Equal(t, "in_flight", claimed.Status)
	intents, err := q.ListRailIntents(ctx, gen.ListRailIntentsParams{
		Rail:           strptr("ccbill"),
		SubscriptionID: &subscriptionID,
		PageLimit:      10,
	})
	require.NoError(t, err)
	require.Len(t, intents, 1)

	merchantIDs, err := q.ListActiveMerchantIDs(ctx)
	require.NoError(t, err)
	require.Contains(t, merchantIDs, merchantID)
	stalled, err := q.ListDunningStalledSubscriptions(ctx, gen.ListDunningStalledSubscriptionsParams{
		MerchantID: merchantID,
		CustomerID: &customerID,
		Now:        now,
	})
	require.NoError(t, err)
	require.Empty(t, stalled)
}

func TestRailMerchantAccountIdentityIsGlobal(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedSuperuserPGXPool(t, "querytest asserts the QUERY PREDICATE with RLS deliberately disabled (SEC-18) and TRUNCATE/ANALYZEs the perf corpus")
	dbtest.EnsureTestMerchant(ctx, t, pool)
	q := gen.New(pool)

	otherMerchantID := uuid.New()
	_, err := pool.Exec(ctx,
		`INSERT INTO openrails.merchants (id, slug, status)
		 VALUES ($1, $2, 'active')`,
		otherMerchantID, "other-"+otherMerchantID.String()[:8])
	require.NoError(t, err)

	accountID := "acct-global-" + uuid.NewString()
	first, err := q.UpsertPSP(ctx, gen.UpsertPSPParams{
		MerchantID:  dbtest.TestMerchantID.UUID(),
		Rail:        "stripe",
		Environment: strptr("test"),
		AccountID:   accountID,
	})
	require.NoError(t, err)

	same, err := q.UpsertPSP(ctx, gen.UpsertPSPParams{
		MerchantID:  dbtest.TestMerchantID.UUID(),
		Rail:        "stripe",
		Environment: strptr("test"),
		AccountID:   accountID,
	})
	require.NoError(t, err)
	require.Equal(t, first.ID, same.ID)

	_, err = q.UpsertPSP(ctx, gen.UpsertPSPParams{
		MerchantID:  otherMerchantID,
		Rail:        "stripe",
		Environment: strptr("test"),
		AccountID:   accountID,
	})
	require.Error(t, err)

	byIdentity, err := q.GetPSPByRailIdentity(ctx, gen.GetPSPByRailIdentityParams{
		Rail:        "stripe",
		Environment: strptr("test"),
		AccountID:   accountID,
	})
	require.NoError(t, err)
	require.Equal(t, dbtest.TestMerchantID.UUID(), byIdentity.MerchantID)
}

func strptr(s string) *string { return &s }

func int32ptr(i int32) *int32 { return &i }

func timeptr(t time.Time) *time.Time { return &t }
