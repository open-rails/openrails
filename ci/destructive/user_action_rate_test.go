//go:build e2e && integration

package destructive_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/stretchr/testify/require"
)

func TestUserActionGlobalRateReservations(t *testing.T) {
	w := newAdmissionsWorld(t)
	mid := w.merchant()
	ctx := merchant.WithID(t.Context(), billing.MerchantID(mid))
	var allowed, denied atomic.Int32
	var firstMu sync.Mutex
	var firstID uuid.UUID
	var firstKey string
	var firstKind destructive.Kind
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Separate limiter instances represent different servers and actors.
			limiter := destructive.NewUserActionLimiter(w.db, config.UserActionRateLimitConfig{})
			id := uuid.New()
			kind := destructive.DeletePaymentMethod
			if i%2 == 0 {
				kind = destructive.CancelSubscription
			}
			key := fmt.Sprintf("%s:%s", kind, id)
			err := limiter.Reserve(ctx, kind, key, id)
			if err == nil {
				allowed.Add(1)
				firstMu.Lock()
				firstID, firstKey, firstKind = id, key, kind
				firstMu.Unlock()
			} else {
				var limited *destructive.UserActionRateError
				if !errors.As(err, &limited) {
					t.Errorf("unexpected admission error: %v", err)
					return
				}
				if limited.RetryAfterSeconds < 1 || limited.RetryAfterSeconds > 3600 {
					t.Errorf("retry-after=%d", limited.RetryAfterSeconds)
				}
				denied.Add(1)
			}
		}(i)
	}
	wg.Wait()
	require.EqualValues(t, 2, allowed.Load())
	require.EqualValues(t, 98, denied.Load())
	limiter := destructive.NewUserActionLimiter(w.db, config.UserActionRateLimitConfig{})
	require.NoError(t, limiter.Reserve(ctx, firstKind, firstKey, firstID), "same operation replay is free")
	require.Error(t, limiter.Reserve(ctx, firstKind, firstKey, uuid.New()), "a key cannot be shared across customers")
	require.Error(t, limiter.Reserve(ctx, destructive.RevokeAccess, firstKey, firstID))
	for i := range 8 {
		require.NoError(t, limiter.Reserve(ctx, destructive.DeletePaymentMethod, fmt.Sprintf("another-card:%s:%d", firstID, i), firstID), "same customer adds no affected customer")
	}
	other := merchant.WithID(t.Context(), billing.MerchantID(w.merchant()))
	require.NoError(t, limiter.Reserve(other, destructive.DeletePaymentMethod, "other-merchant", uuid.New()))
	var holds int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.destructive_holds WHERE merchant_id=$1`), mid).Scan(&holds))
	require.Zero(t, holds, "temporary rate refusal never opens a permanent hold")
	var findings int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.reconciliation_findings WHERE merchant_id=$1 AND finding_type='life.customer_action_rate.exceeded'`), mid).Scan(&findings))
	require.Equal(t, 1, findings)
}

func TestUserActionPaidScaleAndBurst(t *testing.T) {
	w := newAdmissionsWorld(t)
	mid := w.merchant()
	w.exec(`INSERT INTO billing.customers(merchant_id,id,issuer) SELECT $1,gen_random_uuid(),'rate-fixture' FROM generate_series(1,4000)`, mid)
	w.exec(`INSERT INTO billing.orders(merchant_id,id,customer_id,origin,status,payment_status,currency,total,expires_at)
        SELECT $1,id,id,'customer','open','requires_payment_method','USD',100,clock_timestamp()+interval '1 day' FROM billing.customers WHERE merchant_id=$1`, mid)
	w.exec(`INSERT INTO billing.payments(merchant_id,id,customer_id,order_id,channel,transaction_id,amount,list_amount,currency,status,money_movement,purchased_at)
 SELECT $1,gen_random_uuid(),id,id,'manual','rate-'||id::text,100,100,'USD','succeeded','rail',clock_timestamp() FROM billing.customers WHERE merchant_id=$1`, mid)
	for i, status := range []string{"failed", "succeeded", "succeeded"} {
		id := uuid.New()
		w.exec(`INSERT INTO billing.customers(merchant_id,id) VALUES($1,$2)`, mid, id)
		amount, offset := 100, "0 seconds"
		if i == 1 {
			amount = 0
		}
		if i == 2 {
			offset = "1 minute"
		}
		w.exec(`INSERT INTO billing.orders(merchant_id,id,customer_id,origin,status,payment_status,currency,total,expires_at)
            VALUES($1,$2,$2,'customer','open','requires_payment_method','USD',$3,clock_timestamp()+interval '1 day')`, mid, id, amount)
		w.exec(`INSERT INTO billing.payments(merchant_id,id,customer_id,order_id,channel,transaction_id,amount,list_amount,currency,status,money_movement,purchased_at)
            VALUES($1,gen_random_uuid(),$2::uuid,$2::uuid,'manual',$2::uuid::text,$3,$3,'USD',$4,'rail',clock_timestamp()+$5::interval)`, mid, id, amount, status, offset)
	}
	ctx := merchant.WithID(t.Context(), billing.MerchantID(mid))
	limiter := destructive.NewUserActionLimiter(w.db, config.UserActionRateLimitConfig{})
	for i := range 10 {
		require.NoError(t, limiter.Reserve(ctx, destructive.CancelSubscription, fmt.Sprintf("cancel:%d", i), uuid.New()))
	}
	var limited *destructive.UserActionRateError
	require.ErrorAs(t, limiter.Reserve(ctx, destructive.DeletePaymentMethod, "card:eleventh", uuid.New()), &limited)
	require.InDelta(t, 3600, limited.RetryAfterSeconds, 10, "an eleventh customer is refused immediately inside a ten-minute burst")
}

func TestUserActionDailyWindowAndPruning(t *testing.T) {
	w := newAdmissionsWorld(t)
	mid := w.merchant()
	ctx := merchant.WithID(t.Context(), billing.MerchantID(mid))
	limiter := destructive.NewUserActionLimiter(w.db, config.UserActionRateLimitConfig{MinimumCustomersPerHour: 100})
	ids := make([]uuid.UUID, 5)
	for i := range ids {
		ids[i] = uuid.New()
		require.NoError(t, limiter.Reserve(ctx, destructive.CancelSubscription, fmt.Sprintf("cancel:%d", i), ids[i]))
	}
	// Hour is deliberately clear, but the day still holds five distinct customers.
	w.exec(`UPDATE billing.user_destructive_reservations SET reserved_at=clock_timestamp()-interval '2 hours' WHERE merchant_id=$1`, mid)
	var limited *destructive.UserActionRateError
	require.ErrorAs(t, limiter.Reserve(ctx, destructive.DeletePaymentMethod, "sixth", uuid.New()), &limited)
	require.InDelta(t, 22*3600, limited.RetryAfterSeconds, 10)
	// Duplicates must use the customer's newest observation for expiry.
	require.NoError(t, limiter.Reserve(ctx, destructive.DeletePaymentMethod, "same-customer-new-action", ids[0]))
	w.exec(`UPDATE billing.user_destructive_reservations SET reserved_at=clock_timestamp()-interval '25 hours' WHERE merchant_id=$1 AND action_key='cancel:0'`, mid)
	require.ErrorAs(t, limiter.Reserve(ctx, destructive.DeletePaymentMethod, "still-sixth", uuid.New()), &limited)
	require.InDelta(t, 22*3600, limited.RetryAfterSeconds, 10, "expired oldest duplicate does not release its still-counted customer")
	staleCustomer := uuid.New()
	w.exec(`INSERT INTO billing.user_destructive_reservations(merchant_id,customer_id,action_key,kind,reserved_at)
       VALUES($1,$2,'stale-failed-operation','delete_payment_method',clock_timestamp()-interval '25 hours')`, mid, staleCustomer)
	require.ErrorAs(t, limiter.Reserve(ctx, destructive.DeletePaymentMethod, "stale-failed-operation", staleCustomer), &limited, "a stale failed operation must re-enter the current budget")
	n, err := destructive.PruneUserActionReservations(context.Background(), w.db, 100)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	// The old failed operation is not free forever after its reservation expires.
	require.NoError(t, limiter.Reserve(ctx, destructive.CancelSubscription, "cancel:0", ids[0]))
	w.exec(`UPDATE billing.user_destructive_reservations SET reserved_at=clock_timestamp()-interval '25 hours' WHERE merchant_id=$1`, mid)
	_, err = destructive.PruneUserActionReservations(t.Context(), w.db, 100)
	require.NoError(t, err)
	require.NoError(t, limiter.Reserve(ctx, destructive.DeletePaymentMethod, "after-window", uuid.New()))
}

func TestUserActionRetryAfterShrinkingBudget(t *testing.T) {
	w := newAdmissionsWorld(t)
	for _, test := range []struct {
		name    string
		ages    []string
		config  config.UserActionRateLimitConfig
		seconds int64
	}{
		{"hour", []string{"50 minutes", "40 minutes", "30 minutes", "20 minutes"}, config.UserActionRateLimitConfig{MinimumCustomersPerDay: 100}, 1800},
		{"day", []string{"23 hours", "22 hours", "21 hours", "20 hours"}, config.UserActionRateLimitConfig{MinimumCustomersPerHour: 100, MinimumCustomersPerDay: 1}, 4 * 3600},
	} {
		t.Run(test.name, func(t *testing.T) {
			mid := w.merchant()
			for i, age := range test.ages {
				w.exec(`INSERT INTO billing.user_destructive_reservations(merchant_id,customer_id,action_key,kind,reserved_at)
     VALUES($1,$2,$3,'cancel_subscription',clock_timestamp()-$4::interval)`, mid, uuid.New(), fmt.Sprintf("prior:%d", i), age)
			}
			ctx := merchant.WithID(t.Context(), billing.MerchantID(mid))
			limiter := destructive.NewUserActionLimiter(w.db, test.config)
			var limited *destructive.UserActionRateError
			require.ErrorAs(t, limiter.Reserve(ctx, destructive.DeletePaymentMethod, "after-shrink", uuid.New()), &limited)
			require.InDelta(t, test.seconds, limited.RetryAfterSeconds, 10, "enough distinct customers must expire, not just the first one")
		})
	}
}

func TestCustomerDeleteUsesRateLimitInsteadOfStickyVolumeHold(t *testing.T) {
	w := newAdmissionsWorld(t)
	mid, customer, method, psp := w.merchant(), uuid.New(), uuid.New(), uuid.New()
	ctx := merchant.WithID(t.Context(), billing.MerchantID(mid))
	_, err := w.db.GenDirectory().UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{MerchantID: mid, FindingType: intents.HeldBulkFindingType, SubjectKey: "destructive_volume", Severity: "critical", Status: "requires_review", Evidence: []byte(`{}`)})
	require.NoError(t, err)
	payload, err := json.Marshal(intents.NMIPaymentMethodDeletePayload{UserID: customer.String(), PaymentMethodID: method})
	require.NoError(t, err)
	actor := customer.String()
	in := gen.BillingProviderIntent{ID: uuid.New(), MerchantID: mid, Origin: string(intents.OriginUser), Actor: &actor, Rail: "nmi", PspID: &psp, IntentType: intents.TypeNMIPaymentMethodDelete, IdempotencyKey: intents.NMIPaymentMethodDeleteIdempotencyKey(method), Payload: payload}
	breaker := intents.NewVolumeBreaker(w.db)
	calls := 0
	require.NoError(t, w.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		held, _, err := breaker.Check(ctx, in, time.Now(), func(context.Context, *db.DB) error { calls++; return nil })
		require.NoError(t, err)
		require.False(t, held)
		in.IntentType = intents.TypeNMIDeleteSubscription
		subscription := uuid.New()
		in.SubscriptionID = &subscription
		held, _, err = breaker.Check(ctx, in, time.Now(), func(context.Context, *db.DB) error { calls++; return nil })
		require.NoError(t, err)
		require.False(t, held, "accepted customer cancellation cleanup is not admitted again")
		in.Origin = string(intents.OriginSystem)
		held, _, err = breaker.Check(ctx, in, time.Now(), func(context.Context, *db.DB) error { calls++; return nil })
		require.NoError(t, err)
		require.True(t, held, "system bulk protection remains")
		in.Origin = string(intents.OriginAdmin)
		held, _, err = breaker.Check(ctx, in, time.Now(), func(context.Context, *db.DB) error { calls++; return nil })
		require.NoError(t, err)
		require.True(t, held, "operator volume protection remains")
		return nil
	}))
	require.Equal(t, 2, calls)
}
