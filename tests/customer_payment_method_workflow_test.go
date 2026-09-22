//go:build integration

package tests

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestCustomerPaymentMethodAuthorityAndSavedSourceUpdate(t *testing.T) {
	var writes, calls atomic.Int64
	var vault atomic.Value
	vault.Store("old-vault")
	railSub := "source-" + uuid.NewString()
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method == http.MethodGet && r.URL.Path == "/subscriptions/"+railSub {
			fmt.Fprintf(w, `{"id":%q,"customer_vault_id":%q,"delayed_condition":"active"}`, railSub, vault.Load().(string))
			return
		}
		if r.Method == http.MethodPost {
			require.NoError(t, r.ParseForm())
			require.Equal(t, "update_subscription", r.PostFormValue("recurring"))
			require.Equal(t, railSub, r.PostFormValue("subscription_id"))
			require.Equal(t, "new-vault", r.PostFormValue("customer_vault_id"))
			writes.Add(1)
			vault.Store(r.PostFormValue("customer_vault_id"))
			fmt.Fprint(w, "response=1&responsetext=SUCCESS")
			return
		}
		t.Errorf("unexpected provider request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(500)
	}))
	t.Cleanup(gateway.Close)
	f := newTreasuryWorkflow(t, &config.ProviderSandboxConfig{NMIGatewayURL: gateway.URL})
	customer, token := f.actor(t, []string{permissions.CustomerAll})
	other, otherToken := f.actor(t, []string{permissions.CustomerAll})
	rt := f.surface.App().Runtime
	integrationharness.SeedPSPs(t.Context(), t, rt, f.merchant.MerchantID, config.PSPSet{"nmi": {Rail: "nmi", AccountID: "source-" + uuid.NewString(), NMI: &config.NMIRailConfig{SecurityKey: "synthetic", WebhookSigningSecret: "synthetic"}}})
	product, err := f.client.Products.Create(t.Context(), &openrails.ProductCreateParams{Key: "source", DisplayName: "Saved source"})
	require.NoError(t, err)
	hours := 720
	price, err := f.client.Prices.Create(t.Context(), &openrails.PriceCreateParams{ProductID: product.ID, UnitAmount: 9_990_000, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours})
	require.NoError(t, err)
	otherProduct, err := f.client.Products.Create(t.Context(), &openrails.ProductCreateParams{Key: "unsupported", DisplayName: "Provider managed"})
	require.NoError(t, err)
	otherPrice, err := f.client.Prices.Create(t.Context(), &openrails.PriceCreateParams{ProductID: otherProduct.ID, UnitAmount: 9_990_000, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours})
	require.NoError(t, err)
	old, newMethod, foreign, cross, custodial := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	sub, unsupported := uuid.New(), uuid.New()
	mid := f.merchant.MerchantID.UUID()
	ctx := merchant.WithID(t.Context(), f.merchant.MerchantID)
	require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		q := rt.DB.Qx(ctx)
		psp := dbtest.EnsureTestPSP(ctx, t, q, mid, "nmi")
		ccbill := dbtest.EnsureTestPSP(ctx, t, q, mid, "ccbill")
		otherPSP, custodian := uuid.New(), uuid.New()
		_, err := q.Exec(ctx, `INSERT INTO billing.psps(id,merchant_id,rail,environment,account_id,archived) VALUES($1,$2,'nmi','test',$3,true)`, otherPSP, mid, "other-"+otherPSP.String())
		if err != nil {
			return err
		}
		_, err = q.Exec(ctx, `INSERT INTO billing.custodians(id,merchant_id,key,kind,account_id) VALUES($1,$2,$3,'basis_theory',$3)`, custodian, mid, "custodian-"+custodian.String())
		if err != nil {
			return err
		}
		for _, row := range []struct {
			id, payer, psp uuid.UUID
			vault          string
		}{{old, customer.UUID(), psp, "old-vault"}, {newMethod, customer.UUID(), psp, "new-vault"}, {foreign, other.UUID(), psp, "foreign-vault"}, {cross, customer.UUID(), otherPSP, "cross-vault"}, {custodial, customer.UUID(), psp, "custodian-vault"}} {
			_, err = q.Exec(ctx, `INSERT INTO billing.payment_methods(id,merchant_id,customer_id,psp_id,rail,custodian,rail_customer_ref,rail_method_ref,initial_transaction_id) VALUES($1,$2,$3,$4,'nmi','psp',$5,'billing','')`, row.id, mid, row.payer, row.psp, row.vault)
			if err != nil {
				return err
			}
		}
		_, err = q.Exec(ctx, `UPDATE billing.payment_methods SET custodian='basis_theory',custodian_id=$2 WHERE id=$1`, custodial, custodian)
		if err != nil {
			return err
		}
		for _, row := range []struct {
			id, psp, product, price uuid.UUID
			rail, provider          string
		}{{sub, psp, sdkProductID(t, product.ID).UUID(), sdkPriceID(t, price.ID).UUID(), "nmi", railSub}, {unsupported, ccbill, sdkProductID(t, otherProduct.ID).UUID(), sdkPriceID(t, otherPrice.ID).UUID(), "ccbill", "unsupported-" + uuid.NewString()}} {
			_, err = q.Exec(ctx, `INSERT INTO billing.subscriptions(id,merchant_id,customer_id,product_id,price_id,psp_id,payment_method_id,rail,rail_subscription_id,status,current_period_starts_at,current_period_ends_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'active',$10,$11)`, row.id, mid, customer.UUID(), row.product, row.price, row.psp, old, row.rail, row.provider, time.Now().Add(-time.Hour), time.Now().Add(720*time.Hour))
			if err != nil {
				return err
			}
		}
		return nil
	}))
	t.Cleanup(func() {
		require.NoError(t, rt.DB.RunInMerchantConn(context.WithoutCancel(ctx), func(ctx context.Context) error {
			_, err := rt.DB.Qx(ctx).Exec(ctx, `UPDATE billing.psps SET archived=true WHERE merchant_id=$1`, mid)
			return err
		}))
	})
	target := func(id uuid.UUID) string {
		return f.hostURL + "/v1/me/subscriptions/" + openrails.SubscriptionIDPrefix + id.String() + "/payment-method"
	}
	for _, row := range []struct {
		name, bearer         string
		subscription, method uuid.UUID
		want                 int
		code                 string
	}{
		{"anonymous", "", sub, newMethod, 401, ""},
		{"other subject", otherToken, sub, newMethod, 403, ""},
		{"another customer method", token, sub, foreign, 403, ""},
		{"another account", token, sub, cross, 409, "payment_method_psp_mismatch"},
		{"custodian instrument", token, sub, custodial, 409, "payment_method_not_psp_vaulted"},
		{"zero subscription", token, uuid.Nil, newMethod, 400, ""},
		{"zero method", token, sub, uuid.Nil, 400, ""},
		{"unsupported rail", token, unsupported, newMethod, 400, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			status, raw := requestWorkflowJSON(t, http.MethodPut, target(row.subscription), row.bearer, map[string]string{"payment_method_id": openrails.PaymentMethodIDPrefix + row.method.String()})
			require.Equal(t, row.want, status, string(raw))
			if row.code != "" {
				require.Contains(t, string(raw), row.code)
			}
			require.Zero(t, calls.Load())
		})
	}
	// The same credential can see its own list but cannot change another payer's card.
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		status, raw := requestWorkflowJSON(t, method, f.hostURL+"/v1/me/payment-methods/"+openrails.PaymentMethodID(foreign).String(), token, map[string]string{"payment_token": "opaque"})
		require.Equal(t, http.StatusForbidden, status, string(raw))
		require.Zero(t, calls.Load())
	}
	status, raw := requestWorkflowJSON(t, http.MethodGet, f.hostURL+"/v1/me/payment-methods", token, nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	require.Contains(t, string(raw), openrails.PaymentMethodID(newMethod).String())
	require.False(t, strings.Contains(string(raw), openrails.PaymentMethodID(foreign).String()))
	require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var observed uuid.UUID
		if err := rt.DB.Qx(ctx).QueryRow(ctx, `SELECT payment_method_id FROM billing.subscriptions WHERE id=$1`, sub).Scan(&observed); err != nil {
			return err
		}
		require.Equal(t, old, observed)
		var pending int
		err := rt.DB.Qx(ctx).QueryRow(ctx, `SELECT count(*) FROM billing.rail_intents WHERE merchant_id=$1`, mid).Scan(&pending)
		require.Zero(t, pending)
		return err
	}))
	status, raw = requestWorkflowJSON(t, http.MethodPut, target(sub), token, map[string]string{"payment_method_id": openrails.PaymentMethodID(newMethod).String()})
	require.Equal(t, http.StatusOK, status, string(raw))
	require.EqualValues(t, 1, writes.Load())
	require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var observed uuid.UUID
		err := rt.DB.Qx(ctx).QueryRow(ctx, `SELECT payment_method_id FROM billing.subscriptions WHERE id=$1`, sub).Scan(&observed)
		require.Equal(t, newMethod, observed)
		return err
	}))
}
