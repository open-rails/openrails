//go:build integration

package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/merchant"
)

type listResponse[T any] struct {
	Object  string `json:"object"`
	Data    []T    `json:"data"`
	Total   int64  `json:"total"`
	Limit   int    `json:"limit"`
	Offset  int    `json:"offset"`
	HasMore bool   `json:"has_more"`
}

// The account page reads its actual mounted self-service surface using a
// verified host credential. Catalog wire parity lives in the shared Client
// workflows; this sequence owns account state and subject isolation.
func TestCustomerBillingReadWorkflow(t *testing.T) {
	f := newTreasuryWorkflow(t)
	customer, token := f.actor(t, []string{permissions.CustomerAll})
	read := func(path, bearer string, out any) {
		t.Helper()
		status, raw := requestWorkflowJSON(t, http.MethodGet, f.hostURL+path, bearer, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.NoError(t, json.Unmarshal(raw, out))
	}
	list := func(path, bearer string) []map[string]any {
		t.Helper()
		var out listResponse[map[string]any]
		read(path, bearer, &out)
		require.Equal(t, "list", out.Object)
		return out.Data
	}
	empty := func(bearer string) {
		t.Helper()
		for _, path := range []string{"/v1/me/subscriptions?status=active", "/v1/me/subscriptions?status=all", "/v1/me/payments"} {
			require.Empty(t, list(path, bearer), path)
		}
		var status map[string]any
		read("/v1/me/status", bearer, &status)
		require.Nil(t, status["subscription"])
		require.Nil(t, status["next_renewal_at"])
		require.Empty(t, status["entitlements"])
	}
	empty(token)
	product, err := f.client.Products.Create(t.Context(), &openrails.ProductCreateParams{Key: "account", DisplayName: "Account access", EntitlementsSpec: map[string]*int{"premium": nil}})
	require.NoError(t, err)
	duration := 720
	price, err := f.client.Prices.Create(t.Context(), &openrails.PriceCreateParams{ProductID: product.ID, UnitAmount: 9_990_000, Currency: "USD", AccessDurationHours: &duration, AutoRenew: true})
	require.NoError(t, err)
	active, cancelled := uuid.New(), uuid.New()
	first, second := uuid.New(), uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	end := now.Add(720 * time.Hour)
	database := f.surface.App().Runtime.DB
	require.NoError(t, database.RunInMerchantConn(merchant.WithID(t.Context(), f.merchant.MerchantID), func(ctx context.Context) error {
		q := database.Qx(ctx)
		mid := f.merchant.MerchantID.UUID()
		psp := dbtest.EnsureTestPSP(ctx, t, q, mid, "ccbill")
		for _, row := range []struct {
			id    uuid.UUID
			state string
		}{{active, "active"}, {cancelled, "cancelled"}} {
			if _, err := q.Exec(ctx, `INSERT INTO billing.subscriptions(id,merchant_id,customer_id,product_id,price_id,status,rail,rail_subscription_id,psp_id,started_at,current_period_starts_at,current_period_ends_at,cancelled_at,cancel_type) VALUES($1,$2,$3,$4,$5,$6::text::billing.subscription_status,'ccbill',$7,$8,$9,$9,$10,CASE WHEN $6::text='cancelled' THEN $9::timestamptz END,CASE WHEN $6::text='cancelled' THEN $11::text END)`, row.id, mid, customer.UUID(), sdkProductID(t, product.ID).UUID(), sdkPriceID(t, price.ID).UUID(), row.state, "account-"+row.id.String(), psp, now, end, string(models.CancelTypeUser)); err != nil {
				return err
			}
		}
		for _, id := range []uuid.UUID{first, second} {
			if _, err := q.Exec(ctx, `INSERT INTO billing.payments(id,merchant_id,customer_id,price_id,subscription_id,rail,transaction_id,amount,list_amount,currency,status,money_movement,psp_id) VALUES($1,$2,$3,$4,$5,'ccbill',$6,9990000,9990000,'USD','completed','rail',$7)`, id, mid, customer.UUID(), sdkPriceID(t, price.ID).UUID(), active, "account-"+id.String(), psp); err != nil {
				return err
			}
		}
		return nil
	}))
	_, err = f.client.GrantEntitlement(t.Context(), customer, openrails.GrantEntitlementRequest{Entitlement: "premium", EndAt: &end})
	require.NoError(t, err)
	current := list("/v1/me/subscriptions?status=active", token)
	require.Len(t, current, 1)
	require.Equal(t, openrails.SubscriptionID(active).String(), current[0]["id"])
	require.Equal(t, "active", current[0]["status"])
	nested := current[0]["price"].(map[string]any)
	require.Equal(t, price.ID, nested["id"])
	require.Equal(t, "9990000", nested["unit_amount"])
	require.NotContains(t, nested, "amount")
	history := list("/v1/me/subscriptions?status=all", token)
	require.Len(t, history, 2)
	statuses := map[string]any{}
	for _, row := range history {
		statuses[row["id"].(string)] = row["status"]
	}
	require.Equal(t, map[string]any{openrails.SubscriptionID(active).String(): "active", openrails.SubscriptionID(cancelled).String(): "cancelled"}, statuses)
	paid := list("/v1/me/payments", token)
	require.Len(t, paid, 2)
	ids := make([]any, 0, len(paid))
	for _, row := range paid {
		ids = append(ids, row["id"])
		require.Equal(t, "9990000", row["amount"])
		require.Equal(t, "USD", row["currency"])
	}
	require.ElementsMatch(t, []any{openrails.PaymentID(first).String(), openrails.PaymentID(second).String()}, ids)
	var status map[string]any
	read("/v1/me/status", token, &status)
	require.NotNil(t, status["subscription"])
	require.NotNil(t, status["next_renewal_at"])
	require.NotEmpty(t, status["entitlements"])
	_, other := f.actor(t, []string{permissions.CustomerAll})
	empty(other)
	for _, route := range []struct{ method, path string }{
		{"GET", "/v1/me/balance"}, {"GET", "/v1/me/transactions"}, {"PUT", "/v1/me/collection-payment-method"},
		{"GET", "/v1/me/payment-methods"}, {"PUT", "/v1/me/payment-methods/123"}, {"DELETE", "/v1/me/payment-methods/123"},
		{"GET", "/v1/me/subscriptions"}, {"GET", "/v1/me/payments"}, {"GET", "/v1/me/status"},
	} {
		code, raw := requestWorkflowJSON(t, route.method, f.hostURL+route.path, "", nil)
		require.Equal(t, http.StatusUnauthorized, code, string(raw))
	}
}
