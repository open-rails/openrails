//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/internal/service"
)

func TestPriceIdentifierRetriesPreserveLegacyOrderReceipts(t *testing.T) {
	w := orderWorld(t)
	client := w.client[embedded]
	runtime := engine.Graph(w.rt).Runtime
	svc, err := service.New(runtime)
	require.NoError(t, err)
	ctx := merchant.WithID(t.Context(), client.MerchantID())
	price := w.lifetime("price-retry-history", 1_000_000)
	storageID := w.priceStorageID(price.ID)
	legacyID := "price_" + storageID.String()
	lines := []billing.OrderLineParams{{PriceID: price.ID}}
	type fixture struct {
		customer *customer
		key      string
		order    *orders.Order
		merchant bool
	}
	var fixtures []fixture
	for _, scenario := range []struct {
		key              string
		merchant, cached bool
	}{{"customer-row", false, false}, {"customer-receipt", false, true}, {"merchant-row", true, false}} {
		buyer := w.newCustomer()
		// This is precisely the pre-cutover JSON spelling and field order,
		// independent of the new fingerprint implementation.
		legacyRequest := fmt.Sprintf(`{"lines":[{"price_id":%q}]}`, legacyID)
		origin, rowKey, ttl := billing.OrderOriginCustomer, scenario.key, orders.CustomerTTL
		if scenario.merchant {
			legacyRequest = fmt.Sprintf(`{"customer_id":%q,"lines":[{"price_id":%q}]}`, buyer.id, legacyID)
			origin, rowKey, ttl = billing.OrderOriginMerchant, "app:"+scenario.key, orders.MerchantTTL
		}
		digest := sha256.Sum256([]byte(legacyRequest))
		original, err := runtime.Orders.Create(ctx, orders.CreateInput{CustomerID: buyer.cid().UUID(), Origin: origin, Lines: lines, IdempotencyKey: rowKey, RequestDigest: digest[:], TTL: ttl})
		require.NoError(t, err)
		if scenario.cached {
			wire, err := json.Marshal(original.View(nil, nil))
			require.NoError(t, err)
			wire = bytes.ReplaceAll(wire, []byte(price.ID.String()), []byte(legacyID))
			receipt, err := json.Marshal(struct {
				OrderID uuid.UUID       `json:"order_id"`
				Digest  []byte          `json:"digest"`
				Status  int             `json:"status"`
				Code    string          `json:"code,omitempty"`
				Order   json.RawMessage `json:"order,omitempty"`
			}{OrderID: original.ID, Digest: digest[:], Status: http.StatusCreated, Order: wire})
			require.NoError(t, err)
			_, err = w.pool.Exec(t.Context(), w.sql(`INSERT INTO billing.idempotency_keys (merchant_id,operation,idempotency_key,status,token,result,lease_expires_at,expires_at)
VALUES ($1,'order_create',$2,'succeeded',$3,$4,now(),now()+interval '1 day')`), client.MerchantID().UUID(), buyer.id+":"+scenario.key, uuid.New(), receipt)
			require.NoError(t, err)
		}
		fixtures = append(fixtures, fixture{buyer, scenario.key, original, scenario.merchant})
	}
	next, err := client.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: price.ProductID, Key: price.Key, UnitAmount: 2_000_000, Currency: "USD"})
	require.NoError(t, err)
	for _, f := range fixtures {
		t.Run(f.key, func(t *testing.T) {
			if f.merchant {
				params := billing.CreateOrderParams{CustomerID: f.customer.cid(), Lines: lines}
				read, replayed, err := svc.CreateMerchantOrder(ctx, params, f.key)
				require.NoError(t, err)
				require.True(t, replayed)
				require.Equal(t, billing.OrderID(f.order.ID), read.ID)
				require.Equal(t, price.ID, *read.Lines[0].PriceID)
				require.EqualValues(t, 1_000_000, read.Total)
				params.Lines = []billing.OrderLineParams{{PriceID: next.ID}}
				_, _, err = svc.CreateMerchantOrder(ctx, params, f.key)
				require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
			} else {
				actor := service.OrderActor{Customer: f.customer.cid()}
				read, err := svc.CreateOrder(ctx, actor, billing.CreateMyOrderParams{Lines: lines}, f.key)
				require.NoError(t, err)
				require.True(t, read.Replayed)
				require.Equal(t, billing.OrderID(f.order.ID), read.Order.ID)
				require.Equal(t, price.ID, *read.Order.Lines[0].PriceID)
				require.EqualValues(t, 1_000_000, read.Order.Total)
				_, err = svc.CreateOrder(ctx, actor, billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: next.ID}}}, f.key)
				require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
			}
			var count int
			require.NoError(t, w.pool.QueryRow(t.Context(), w.sql(`SELECT count(*) FROM billing.orders WHERE merchant_id=$1 AND customer_id=$2`), client.MerchantID().UUID(), f.customer.cid().UUID()).Scan(&count))
			require.Equal(t, 1, count, "a format change never manufactures another order")
		})
	}
}
