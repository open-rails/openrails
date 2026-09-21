//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

// Actual HTTP callbacks must use the same clock as the host runtime for
// paid terms, dunning and expiry/reactivation decisions.
func TestCCBillCallbacksUseRuntimeClock(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	clock := clockwork.NewFakeClockAt(now)
	h := New(t, t.Context())
	surface := h.StartStandalone("USD", WithClock(clock), WithConfig(func(c *config.Config) {
		c.MerchantSource = config.MerchantSourceAPI
		c.SecretBackend = config.SecretBackendDB
	}))
	owned := surface.ProvisionOwnedMerchant("ccbill-" + uuid.NewString()[:8])
	client := surface.Client(openrails.WithAPIKey(owned.APIKey), openrails.WithMerchantID(owned.MerchantID))
	account, subaccount := fmt.Sprintf("%06d", 100000+uuid.New().ID()%900000), fmt.Sprintf("%04d", uuid.New().ID()%10000)
	rt := surface.App().Runtime
	SeedPSPs(t.Context(), t, rt, owned.MerchantID, config.PSPSet{"ccbill": {Rail: "ccbill", AccountID: account + "-" + subaccount, CCBill: &config.CCBillRailConfig{Salt: "synthetic", DataLinkUsername: "synthetic", DataLinkPassword: "synthetic"}}})
	product, err := client.CreateProduct(t.Context(), openrails.CreateProductRequest{Key: "callback", DisplayName: "Callback access", EntitlementsSpec: map[string]*int{"callback_access": nil}})
	require.NoError(t, err)
	hours := 720
	_, err = client.CreatePrice(t.Context(), openrails.CreatePriceRequest{ProductID: product.ID, UnitAmount: 9_990_000, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours, PSPLinks: map[string]map[string]string{"ccbill": {models.RailKeyCCBillFormName: "workflow", models.RailKeyCCBillFlexID: "workflow-flex", models.RailKeyCCBillRecurringBillingOption: "workflow-recurring"}}})
	require.NoError(t, err)
	username := "callback" + uuid.NewString()[:8]
	user, err := embcp.Get(surface.App()).Core().CreateUser(t.Context(), username+"@example.test", username)
	require.NoError(t, err)
	customer := openrails.CustomerID(uuid.MustParse(user.ID))
	pool := h.MerchantPool(owned.MerchantID.UUID())
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), `UPDATE billing.psps SET archived=true WHERE merchant_id=$1`, owned.MerchantID.UUID())
		require.NoError(t, err)
	})
	psp := dbtest.EnsureTestPSP(t.Context(), t, pool, owned.MerchantID.UUID(), "ccbill")
	ctx := db.WithPSPID(merchant.WithID(t.Context(), owned.MerchantID), psp)
	railSub := "callback-" + uuid.NewString()
	end := now.Add(5 * 24 * time.Hour).Truncate(24 * time.Hour).Add(24*time.Hour - time.Second)
	payload := func() map[string]any {
		return map[string]any{"clientAccnum": account, "clientSubacc": subaccount, "subscriptionId": railSub, "transactionId": uuid.NewString(), "timestamp": clock.Now().Format("2006-01-02 15:04:05"), "nextRenewalDate": end.Format("2006-01-02")}
	}
	post := func(event string, body map[string]any) {
		t.Helper()
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		response, err := http.Post(surface.BaseURL+"/v1/webhooks/ccbill?eventType="+event, "application/json", bytes.NewReader(raw))
		require.NoError(t, err)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		require.Equal(t, http.StatusOK, response.StatusCode, string(data))
	}
	subscription := func() *models.Subscription {
		t.Helper()
		var sub *models.Subscription
		err := rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
			var err error
			sub, err = rt.SubscriptionService.GetByPSPSubscriptionID(ctx, "ccbill", railSub)
			return err
		})
		require.NoError(t, err)
		return sub
	}
	access := func(at time.Time, want bool) {
		t.Helper()
		got, err := client.HasEntitlement(t.Context(), customer, "callback_access", at)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	initial := payload()
	for key, value := range map[string]any{"username": username, "flexId": "workflow-flex", "formName": "workflow", "subscriptionTypeId": "workflow-recurring", "billedInitialPrice": "9.99", "billedCurrencyCode": 840} {
		initial[key] = value
	}
	post("NewSaleSuccess", initial)
	sub := subscription()
	require.Equal(t, models.StatusActive, sub.Status)
	require.Equal(t, customer.UUID(), sub.CustomerID)
	require.NotNil(t, sub.CurrentPeriodEndsAt)
	require.True(t, end.Equal(*sub.CurrentPeriodEndsAt))
	access(now, true)
	var amount int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT amount FROM billing.payments WHERE merchant_id=$1 AND transaction_id=$2`, owned.MerchantID.UUID(), initial["transactionId"]).Scan(&amount))
	require.EqualValues(t, 9_990_000, amount)

	failure := payload()
	retry := end.Add(3 * 24 * time.Hour)
	failure["nextRetryDate"], failure["renewalDate"] = retry.Format("2006-01-02"), end.Format("2006-01-02")
	post("RenewalFailure", failure)
	sub = subscription()
	require.Equal(t, models.StatusPastDue, sub.Status)
	require.NotNil(t, sub.NextRetryAt)
	require.True(t, retry.Equal(*sub.NextRetryAt))
	require.NotNil(t, sub.GraceEndsAt)
	require.True(t, retry.Equal(*sub.GraceEndsAt))
	require.Nil(t, sub.RetryAttempts)
	require.Nil(t, sub.LastRetryAt)
	access(retry, true)
	end = end.Add(30 * 24 * time.Hour)
	renewal := payload()
	renewal["billedAmount"], renewal["billedCurrencyCode"] = "9.99", 840
	post("RenewalSuccess", renewal)
	post("RenewalSuccess", renewal)
	sub = subscription()
	require.Equal(t, models.StatusActive, sub.Status)
	require.NotNil(t, sub.CurrentPeriodEndsAt)
	require.True(t, end.Equal(*sub.CurrentPeriodEndsAt))
	require.Nil(t, sub.NextRetryAt)
	require.Nil(t, sub.GraceEndsAt)
	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM billing.payments WHERE merchant_id=$1 AND transaction_id=$2`, owned.MerchantID.UUID(), renewal["transactionId"]).Scan(&count))
	require.Equal(t, 1, count)

	cancel := payload()
	cancel["source"], cancel["reason"] = "merchant", "requested"
	post("Cancellation", cancel)
	sub = subscription()
	require.Equal(t, models.StatusCancelled, sub.Status)
	require.NotNil(t, sub.EndedAt)
	require.True(t, end.Equal(*sub.EndedAt))
	access(end.Add(-time.Microsecond), true)
	access(end, false)
	// A merchant cancellation remains terminal even after its paid term. A
	// separate provider-expired subscription is the reactivation boundary.
	clock.Advance(end.Sub(clock.Now()) + time.Second)
	railSub = "expired-" + uuid.NewString()
	end = end.Add(5 * 24 * time.Hour)
	for key, value := range payload() {
		initial[key] = value
	}
	post("NewSaleSuccess", initial)
	require.Equal(t, models.StatusActive, subscription().Status)
	clock.Advance(end.Sub(clock.Now()) + time.Second)
	post("Expiration", payload())
	access(clock.Now(), false)
	end = end.Add(30 * 24 * time.Hour)
	post("UserReactivation", payload())
	require.Equal(t, models.StatusActive, subscription().Status)
	access(clock.Now(), true)
}
