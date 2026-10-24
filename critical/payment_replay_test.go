//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

func saleInput(t *testing.T, a *authorityWorld, p *providerLedger) billing.CreateMyOrderParams {
	t.Helper()
	product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "sale", DisplayName: "Sale", Ownership: catalog.OwnershipConsumable, Entitlements: []string{"sale:owned"}})
	require.NoError(t, err)
	price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "sale", UnitAmount: 30_000_000, Currency: "USD"})
	require.NoError(t, err)
	return billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price.ID}}, ExpectedTotal: ptr(int64(30_000_000)), Reusable: ptr(true), Payment: &billing.OrderPaymentParams{Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}
}

func TestPaymentReplay(t *testing.T) {
	t.Run("OR-J10-S01/concurrent-identical-purchase-has-one-economic-debit", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		input := saleInput(t, a, p)
		type result struct {
			status int
			raw    []byte
			err    error
		}
		start := make(chan struct{})
		results := make(chan result, 2)
		for range 2 {
			go func() {
				<-start
				s, b, e := a.do(t.Context(), http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "concurrent-purchase"})
				results <- result{s, b, e}
			}()
		}
		close(start)
		var accepted []billing.Order
		observed := []result{<-results, <-results}
		for _, r := range observed {
			require.NoError(t, r.err)
			if r.status == http.StatusConflict {
				require.Contains(t, string(r.raw), `"code":"idempotency_key_in_use"`)
				continue
			}
			require.Equal(t, http.StatusCreated, r.status, string(r.raw))
			var order billing.Order
			require.NoError(t, json.Unmarshal(r.raw, &order))
			accepted = append(accepted, order)
		}
		require.NotEmpty(t, accepted)
		for _, order := range accepted {
			require.Equal(t, accepted[0].ID, order.ID)
		}
		status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "concurrent-purchase"})
		require.Equal(t, http.StatusCreated, status, "idempotent replay preserves the original status and body: "+string(raw))
		var replay billing.Order
		require.NoError(t, json.Unmarshal(raw, &replay))
		require.Equal(t, accepted[0].ID, replay.ID)
		require.Equal(t, billing.OrderComplete, replay.Status)
		effects := p.effects(t)
		require.Len(t, effects, 1)
		require.EqualValues(t, 3000, effects[0].Amount)
		payments, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID))})
		require.NoError(t, err)
		require.Len(t, payments.Items, 1)
		require.EqualValues(t, 30_000_000, payments.Items[0].Amount)
	})
	t.Run("OR-J10-S03/accepted-key-refuses-changed-purchase", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		input := saleInput(t, a, p)
		status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "meaning-of-purchase"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var first billing.Order
		require.NoError(t, json.Unmarshal(raw, &first))
		require.Equal(t, billing.OrderComplete, first.Status)
		input.Lines[0].Quantity = ptr(2)
		input.ExpectedTotal = ptr(int64(60_000_000))
		status, raw = a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "meaning-of-purchase"})
		require.Equal(t, http.StatusUnprocessableEntity, status, string(raw))
		require.Contains(t, string(raw), `"code":"idempotency_key_reused"`)
		effects := p.effects(t)
		require.Len(t, effects, 1)
		require.EqualValues(t, 3000, effects[0].Amount)
		persisted, err := a.alpha.GetOrder(t.Context(), first.ID)
		require.NoError(t, err)
		require.EqualValues(t, 30_000_000, persisted.Total)
	})
}
