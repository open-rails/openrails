//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	embcp "github.com/open-rails/openrails/internal/operator"
	"github.com/stretchr/testify/require"
)

// UpgradeSuccess has its own provider field mapping: billedInitialPrice, not
// amount. Keep the actual callback -> catalog replacement -> payment boundary.
func TestCCBillUpgradeBilledPriceAndDuplicateCallback(t *testing.T) {
	h := New(t, t.Context())
	surface := h.StartStandalone("USD", WithConfig(func(c *config.Config) {
		c.MerchantConfigHTTP = true
		c.SecretBackend = config.SecretBackendDB
	}))
	owned := surface.ProvisionOwnedMerchant("ccupgrade-" + uuid.NewString()[:8])
	client := surface.Client(openrails.WithMerchantID(owned.MerchantID), openrails.WithAPIKey(owned.APIKey))
	account, subaccount := fmt.Sprintf("%06d", 100000+uuid.New().ID()%900000), fmt.Sprintf("%04d", uuid.New().ID()%10000)
	SeedPSPs(t.Context(), t, surface.App().Runtime, owned.MerchantID, config.PSPSet{"ccbill": {Rail: "ccbill", AccountID: account + "-" + subaccount, CCBill: &config.CCBillRailConfig{Salt: "synthetic", DataLinkUsername: "synthetic", DataLinkPassword: "synthetic"}}})
	pool := h.MerchantPool(owned.MerchantID.UUID())
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), `UPDATE billing.psps SET archived=true WHERE merchant_id=$1`, owned.MerchantID.UUID())
		require.NoError(t, err)
	})
	product, err := client.Products.Create(t.Context(), &openrails.ProductCreateParams{Key: "upgrade", DisplayName: "CCBill upgrade", EntitlementsSpec: map[string]*int{"upgrade_access": nil}})
	require.NoError(t, err)
	var prices []openrails.PriceID
	for i, amount := range []int64{9_990_000, 24_990_000} {
		hours := 720 * (i + 1)
		price, err := client.Prices.Create(t.Context(), &openrails.PriceCreateParams{ProductID: product.ID, UnitAmount: amount, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours, PSPLinks: map[string]map[string]string{"ccbill": {"flex_id": fmt.Sprintf("upgrade-flex-%d", i), "form_name": "workflow"}}})
		require.NoError(t, err)
		prices = append(prices, sdkPriceID(t, price.ID))
	}
	username := "upgrade" + uuid.NewString()[:8]
	user, err := embcp.Get(surface.App()).Core().CreateUser(t.Context(), username+"@example.test", username)
	require.NoError(t, err)
	oldSub, newSub := "old-"+uuid.NewString(), "new-"+uuid.NewString()
	now := time.Now().UTC().Add(-time.Hour)
	body := map[string]any{"clientAccnum": account, "clientSubacc": subaccount, "subscriptionId": oldSub, "transactionId": uuid.NewString(), "username": username, "timestamp": now.Format("2006-01-02 15:04:05"), "nextRenewalDate": now.Add(30 * 24 * time.Hour).Format("2006-01-02"), "flexId": "upgrade-flex-0", "formName": "workflow", "billedInitialPrice": "9.99", "billedCurrencyCode": 840}
	post := func(event string) {
		t.Helper()
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		res, err := http.Post(surface.BaseURL+"/v1/webhooks/ccbill/"+account+"-"+subaccount+"?eventType="+event, "application/json", bytes.NewReader(raw))
		require.NoError(t, err)
		data, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		require.NoError(t, res.Body.Close())
		require.Equal(t, http.StatusOK, res.StatusCode, string(data))
	}
	post("NewSaleSuccess")
	transaction := uuid.NewString()
	originalAccount, err := strconv.Atoi(account)
	require.NoError(t, err)
	for key, value := range map[string]any{"subscriptionId": newSub, "originalSubscriptionId": oldSub, "originalClientAccnum": originalAccount, "originalClientSubacc": subaccount, "source": "FORM", "scaResponseStatus": "Y", "transactionId": transaction, "flexId": "upgrade-flex-1", "billedInitialPrice": "24.99", "billedRecurringPrice": "24.99", "subscriptionInitialPrice": "24.99", "subscriptionRecurringPrice": "24.99"} {
		body[key] = value
	}
	post("UpgradeSuccess")
	post("UpgradeSuccess")
	var sub, price uuid.UUID
	var state string
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT id,price_id,status FROM billing.subscriptions WHERE merchant_id=$1 AND rail_subscription_id=$2 AND deleted_at IS NULL`, owned.MerchantID.UUID(), newSub).Scan(&sub, &price, &state))
	require.Equal(t, prices[1].UUID(), price)
	require.Equal(t, "active", state)
	var count int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM billing.subscriptions WHERE merchant_id=$1 AND rail_subscription_id=$2 AND deleted_at IS NULL`, owned.MerchantID.UUID(), oldSub).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM billing.payments WHERE merchant_id=$1 AND customer_id=$2 AND subscription_id=$3 AND price_id=$4 AND transaction_id=$5 AND amount=24990000 AND list_amount=24990000 AND currency='USD'`, owned.MerchantID.UUID(), uuid.MustParse(user.ID), sub, price, transaction).Scan(&count))
	require.Equal(t, 1, count)
	active, err := client.HasEntitlement(t.Context(), (openrails.CustomerID(uuid.MustParse(user.ID))).String(), "upgrade_access", time.Now().UTC())
	require.NoError(t, err)
	require.True(t, active)
}
