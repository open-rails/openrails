package webhooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

func TestTransactionSubscriptionID_Fallbacks(t *testing.T) {
	tests := []struct {
		name string
		body *NMITransactionEventBody
		want string
	}{
		{
			name: "subscription reference",
			body: &NMITransactionEventBody{
				Subscription: &NMISubscriptionRef{SubscriptionID: " sub-123 "},
			},
			want: "sub-123",
		},
		/*{
			name: "transaction detail subscription",
			body: &NMITransactionEventBody{
				TransactionDetail: &NMITransactionDetail{
					Subscription: &NMISubscriptionRef{SubscriptionID: " detail-456 "},
				},
			},
			want: "detail-456",
		},*/
		{
			name: "order id fallback",
			body: &NMITransactionEventBody{OrderID: " order-789 "},
			want: "order-789",
		},
		{
			name: "transaction detail order id fallback",
			body: &NMITransactionEventBody{
				TransactionDetail: &NMITransactionDetail{OrderID: " detail-order "},
			},
			want: "detail-order",
		},
		{
			name: "po number fallback",
			body: &NMITransactionEventBody{PONumber: " po-001 "},
			want: "po-001",
		},
		{
			name: "transaction detail po number fallback",
			body: &NMITransactionEventBody{
				TransactionDetail: &NMITransactionDetail{PONumber: " detail-po "},
			},
			want: "detail-po",
		},
		/*{
			name: "customer id fallback",
			body: &NMITransactionEventBody{CustomerID: " cust-002 "},
			want: "cust-002",
		},
		{
			name: "transaction detail customer id fallback",
			body: &NMITransactionEventBody{
				TransactionDetail: &NMITransactionDetail{CustomerID: " detail-cust "},
			},
			want: "detail-cust",
		},*/
		{
			name: "empty payload",
			body: &NMITransactionEventBody{},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, transactionSubscriptionID(tc.body))
		})
	}
}

func TestTransactionActionSource(t *testing.T) {
	require.Equal(t, "recurring", transactionActionSource(&NMITransactionEventBody{
		Action: &NMIAction{Source: "Recurring"},
	}))

	require.Equal(t, "retry", transactionActionSource(&NMITransactionEventBody{
		TransactionDetail: &NMITransactionDetail{
			Action: &NMIAction{Source: "Retry"},
		},
	}))

	require.Equal(t, "", transactionActionSource(&NMITransactionEventBody{}))
}

func TestIsRecurringSource(t *testing.T) {
	require.True(t, isRecurringSource("recurring"))
	require.True(t, isRecurringSource("RETRY"))
	require.False(t, isRecurringSource("api"))
	require.False(t, isRecurringSource(""))
}

func TestNormalizeNMIChargebackLast4(t *testing.T) {
	require.Equal(t, "1111", normalizeNMIChargebackLast4("411111******1111"))
	require.Equal(t, "1111", normalizeNMIChargebackLast4(" 1111 "))
	require.Equal(t, "", normalizeNMIChargebackLast4("****"))
}

func TestTransactionAmountCentsFallsBackAfterMalformedAmount(t *testing.T) {
	amount, err := transactionAmountCents(&NMITransactionEventBody{
		Amount: Stringish("not-a-number"),
		TransactionDetail: &NMITransactionDetail{
			Amount: Stringish("12.34"),
		},
	})
	require.NoError(t, err)
	require.EqualValues(t, 1234, amount)
}

func TestSplitNMIChargebackReason(t *testing.T) {
	code, reason := splitNMIChargebackReason("101: Introductory chargeback", "")
	require.Equal(t, "101", code)
	require.Equal(t, "Introductory chargeback", reason)

	code, reason = splitNMIChargebackReason("Introductory chargeback", "204")
	require.Equal(t, "204", code)
	require.Equal(t, "Introductory chargeback", reason)
}

func TestHandleChargebackComplete_RequiresProcessor(t *testing.T) {
	body, err := json.Marshal(NMIChargebackBatchEventBody{
		Chargebacks: []NMIChargebackEntry{},
		Batch: &NMIChargebackBatch{
			Count: 0,
		},
	})
	require.NoError(t, err)

	svc := &NMIWebhookService{
		Data: NMIWebhookEvent{
			EventID:   uuid.New().String(),
			EventType: string(EventTypeNMIChargebackComplete),
			EventBody: body,
		},
	}

	err = svc.handleChargebackComplete(context.Background())
	require.Error(t, err)
	require.ErrorContains(t, err, "nmi webhook processor is required")
}

func TestNMIProviderTransactionIDFromMetadata(t *testing.T) {
	raw, err := json.Marshal(map[string]any{"provider_transaction_id": " txn_123 "})
	require.NoError(t, err)
	require.Equal(t, "txn_123", nmiProviderTransactionIDFromMetadata(raw))

	raw, err = json.Marshal(map[string]any{"provider_transaction_id": "nmi_sub_attempt:order_123"})
	require.NoError(t, err)
	require.Empty(t, nmiProviderTransactionIDFromMetadata(raw))
}

func TestHandleAddSubscription_ActivatesPendingWithSettledTransactionMetadata(t *testing.T) {
	dsn := os.Getenv("OPENRAILS_TEST_DB_URL")
	if dsn == "" {
		t.Skip("set OPENRAILS_TEST_DB_URL to run integration tests")
	}

	svc, bunDB, ids := setupNMIAddSubscriptionTest(t, dsn, true)
	ctx := context.Background()

	require.NoError(t, svc.handleAddSubscription(ctx))

	var gotSub models.Subscription
	require.NoError(t, bunDB.NewSelect().Model(&gotSub).Where("id = ?", ids.subscriptionID).Scan(ctx))
	require.Equal(t, models.StatusActive, gotSub.Status)
	require.NotNil(t, gotSub.CurrentPeriodStartsAt)
	require.NotNil(t, gotSub.CurrentPeriodEndsAt)
	require.Equal(t, time.Date(2026, time.June, 17, 0, 0, 0, 0, time.UTC), gotSub.CurrentPeriodEndsAt.UTC())

	var payment models.Payment
	require.NoError(t, bunDB.NewSelect().Model(&payment).Where("transaction_id = ?", ids.transactionID).Scan(ctx))
	require.Equal(t, ids.userID, payment.UserID)
	require.Equal(t, ids.subscriptionID, *payment.SubscriptionID)

	exists, err := bunDB.NewSelect().Model((*models.Entitlement)(nil)).
		Where("user_id = ?", ids.userID).
		Where("entitlement = ?", "premium").
		Where("source_type = ?", models.EntitlementSourceSubscription).
		Where("source_id = ?", ids.subscriptionID).
		Where("revoked_at IS NULL").
		Where("deleted_at IS NULL").
		Exists(ctx)
	require.NoError(t, err)
	require.True(t, exists)

	svc.Data = NMIWebhookEvent{EventID: uuid.New().String(), EventType: string(EventTypeNMITransactionSuccess), EventBody: ids.transactionBody}
	require.NoError(t, svc.handleTransactionSaleSuccess(ctx))
	count, err := bunDB.NewSelect().Model((*models.Payment)(nil)).Where("transaction_id = ?", ids.transactionID).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestHandleAddSubscription_WithoutSettledTransactionMetadataStaysPending(t *testing.T) {
	dsn := os.Getenv("OPENRAILS_TEST_DB_URL")
	if dsn == "" {
		t.Skip("set OPENRAILS_TEST_DB_URL to run integration tests")
	}

	svc, bunDB, ids := setupNMIAddSubscriptionTest(t, dsn, false)
	ctx := context.Background()

	require.NoError(t, svc.handleAddSubscription(ctx))

	var gotSub models.Subscription
	require.NoError(t, bunDB.NewSelect().Model(&gotSub).Where("id = ?", ids.subscriptionID).Scan(ctx))
	require.Equal(t, models.StatusPending, gotSub.Status)

	count, err := bunDB.NewSelect().Model((*models.Payment)(nil)).Where("transaction_id = ?", ids.transactionID).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, count)
}

type nmiAddSubscriptionTestIDs struct {
	userID          string
	subscriptionID  uuid.UUID
	transactionID   string
	transactionBody []byte
}

func setupNMIAddSubscriptionTest(t *testing.T, dsn string, includeTransactionMetadata bool) (*NMIWebhookService, *bun.DB, nmiAddSubscriptionTestIDs) {
	t.Helper()

	sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = sqlDB.Close() })
	bunDB := bun.NewDB(sqlDB, pgdialect.New())
	models.RegisterModels(bunDB)

	ctx := context.Background()
	require.NoError(t, bunDB.PingContext(ctx))

	dbi, err := db.NewWithBun(bunDB)
	require.NoError(t, err)

	now := time.Date(2026, time.May, 18, 13, 28, 21, 0, time.UTC)
	fakeClock := clockwork.NewFakeClockAt(now)
	provider := string(models.ProcessorMobius)
	planID := "premium_test_" + uuid.New().String()
	providerSubID := "nmi_sub_" + uuid.New().String()
	transactionID := "txn_" + uuid.New().String()
	userID := uuid.New().String()
	productID := uuid.New()
	priceID := uuid.New()
	subscriptionID := uuid.New()
	durationDays := 30

	product := &models.Product{
		ID:          productID,
		Slug:        "nmi_add_subscription_" + uuid.New().String(),
		DisplayName: "Premium Membership",
		Description: "Test premium product",
		EntitlementsSpec: map[string]*int{
			"premium": &durationDays,
		},
		IsActive:  true,
		CreatedAt: now,
		UpdatedAt: now,
	}
	price := &models.Price{
		ID:          priceID,
		ProductID:   productID,
		DisplayName: "Premium Monthly",
		IsActive:    true,
		Amount:      2399,
		Currency:    "USD",
		BillingCycleDays: func() *int {
			days := 30
			return &days
		}(),
		Processors: map[string]map[string]string{
			provider: {
				models.ProcessorKeyPlanID:   planID,
				models.ProcessorKeyProvider: provider,
			},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	metadata := map[string]any{
		"order_id": "order_" + uuid.New().String(),
	}
	if includeTransactionMetadata {
		metadata["provider_transaction_id"] = transactionID
	}
	metadataBytes, err := json.Marshal(metadata)
	require.NoError(t, err)
	subscription := &models.Subscription{
		ID:                       subscriptionID,
		UserID:                   userID,
		ProductID:                productID,
		PriceID:                  priceID,
		EntitlementsSpecSnapshot: models.CloneEntitlementsSpec(product.EntitlementsSpec),
		Status:                   models.StatusPending,
		Processor:                models.ProcessorMobius,
		ProcessorSubscriptionID:  providerSubID,
		StartedAt:                now,
		Metadata:                 metadataBytes,
		CreatedAt:                now,
		UpdatedAt:                now,
	}

	_, err = bunDB.NewInsert().Model(product).Exec(ctx)
	require.NoError(t, err)
	_, err = bunDB.NewInsert().Model(price).Exec(ctx)
	require.NoError(t, err)
	_, err = bunDB.NewInsert().Model(subscription).Exec(ctx)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.NotificationQueue)(nil)).Where("user_id = ?", userID).Exec(context.Background())
		_, _ = bunDB.NewDelete().Model((*models.Entitlement)(nil)).Where("user_id = ?", userID).Exec(context.Background())
		_, _ = bunDB.NewDelete().Model((*models.Payment)(nil)).Where("user_id = ?", userID).Exec(context.Background())
		_, _ = bunDB.NewDelete().Model((*models.Subscription)(nil)).Where("id = ?", subscriptionID).Exec(context.Background())
		_, _ = bunDB.NewDelete().Model((*models.Price)(nil)).Where("id = ?", priceID).Exec(context.Background())
		_, _ = bunDB.NewDelete().Model((*models.Product)(nil)).Where("id = ?", productID).Exec(context.Background())
	})

	body, err := json.Marshal(NMIRecurringEventBody{
		SubscriptionID: Stringish(providerSubID),
		NextChargeDate: Stringish("2026-06-17"),
		Plan: &NMIPlan{
			ID:     Stringish(planID),
			Amount: Stringish("23.99"),
		},
	})
	require.NoError(t, err)
	transactionBody, err := json.Marshal(NMITransactionEventBody{
		TransactionID: Stringish(transactionID),
		Amount:        Stringish("23.99"),
		Currency:      Stringish("USD"),
		Subscription: &NMISubscriptionRef{
			SubscriptionID: Stringish(providerSubID),
		},
	})
	require.NoError(t, err)

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi, fakeClock)
	paymentSvc := payments.NewPaymentService(dbi, fakeClock)
	subscriptionSvc := subscriptions.NewSubscriptionService(dbi, priceSvc, productSvc, nil, nil, nil, fakeClock)
	lifecycleSvc := subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, nil, paymentSvc, nil, fakeClock)

	return &NMIWebhookService{
			DB:                           dbi,
			Clock:                        fakeClock,
			PriceService:                 priceSvc,
			ProductService:               productSvc,
			Data:                         NMIWebhookEvent{EventID: uuid.New().String(), EventType: string(EventTypeNMIAddSubscription), EventBody: body},
			Processor:                    provider,
			SubscriptionService:          subscriptionSvc,
			PaymentService:               paymentSvc,
			SubscriptionLifecycleService: lifecycleSvc,
		}, bunDB, nmiAddSubscriptionTestIDs{
			userID:          userID,
			subscriptionID:  subscriptionID,
			transactionID:   transactionID,
			transactionBody: transactionBody,
		}
}

func TestParseNMIChargebackDate(t *testing.T) {
	ts, ok := parseNMIChargebackDate("3/29/2020")
	require.True(t, ok)
	require.Equal(t, time.Date(2020, time.March, 29, 0, 0, 0, 0, time.UTC), ts)

	_, ok = parseNMIChargebackDate("not-a-date")
	require.False(t, ok)
}

func TestParseNMIChargebackAmountCents(t *testing.T) {
	amount, err := parseNMIChargebackAmountCents("11.11")
	require.NoError(t, err)
	require.EqualValues(t, 1111, amount)

	_, err = parseNMIChargebackAmountCents("")
	require.Error(t, err)
}

func TestParseDecimalToCents(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    int64
		wantErr bool
	}{
		{name: "whole dollars", raw: "10", want: 1000},
		{name: "two decimals", raw: "10.01", want: 1001},
		{name: "three decimals rounds up", raw: "10.015", want: 1002},
		{name: "three decimals rounds down", raw: "10.014", want: 1001},
		{name: "exact half up", raw: "1.005", want: 101},
		{name: "negative rounds away from zero", raw: "-1.005", want: -101},
		{name: "invalid", raw: "abc", wantErr: true},
		{name: "empty", raw: "", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := moneyutil.ParseDecimalToCents(tc.raw)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestTransactionAmountCents_UsesFallbackFields(t *testing.T) {
	body := &NMITransactionEventBody{
		Amount: "",
		TransactionDetail: &NMITransactionDetail{
			Amount: "7.255",
		},
	}

	amount, err := transactionAmountCents(body)
	require.NoError(t, err)
	require.EqualValues(t, 726, amount)
}
