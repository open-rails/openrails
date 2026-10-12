//go:build e2e && integration

package ci_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/server/internal/operator"
	"github.com/stretchr/testify/require"
)

func TestNotificationConsumersProjectLegacyPriceIDs(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	graph, _ := operator.Of(w.srv)
	database := graph.Runtime.DB
	from, to, customer := uuid.New(), uuid.New(), uuid.New()
	kinds := []string{"subscription_changed", "subscription_reprice_scheduled", "subscription_plan_change_scheduled", "future_kind", "future_array", "subscription_changed"}
	ids := make([]uuid.UUID, len(kinds))
	for i := range ids {
		ids[i] = uuid.New()
	}
	type fact struct {
		raw       []byte
		from, to  string
		projected bool
	}
	facts := map[billing.NotificationEventRef]fact{}
	for i, mid := range []billing.MerchantID{w.shop, w.rival} {
		product, err := handle(t, w.srv, mid).CreateProduct(ctx, billing.CreateProductParams{Key: fmt.Sprintf("legacy-%d", i), DisplayName: "Legacy"})
		require.NoError(t, err)
		wantFrom, err := billing.NewPriceID(product.Key, "buy", 1)
		require.NoError(t, err)
		wantTo, err := billing.NewPriceID(product.Key, "buy", 2)
		require.NoError(t, err)
		// The same private UUIDs deliberately belong to different merchants.
		// A global consumer must resolve each fact in its own merchant.
		require.NoError(t, database.MerchantTx(merchant.WithID(ctx, mid), func(ctx context.Context, tx pgx.Tx) error {
			q := gen.New(tx)
			if _, err := q.EnsureCustomer(ctx, gen.EnsureCustomerParams{MerchantID: mid.UUID(), ID: customer}); err != nil {
				return err
			}
			for j, id := range []uuid.UUID{from, to} {
				if _, err := q.CreatePrice(ctx, gen.CreatePriceParams{MerchantID: mid.UUID(), ID: id, ProductID: product.ID.UUID(), Key: "buy", Amount: int64(j+1) * 1_000_000, Currency: "USD", Archived: j == 0}); err != nil {
					return err
				}
			}
			for j, kind := range kinds {
				fromID, toID := "price_"+from.String(), "price_"+to.String()
				if j == 5 {
					fromID, toID = wantFrom.String(), wantTo.String()
				}
				raw := fmt.Sprintf(`{"from_price_id":%q,"to_price_id":%q,"old_amount":"9007199254740993","extension":{"from_price_id":"price_%s","number":9007199254740993},"metadata":{"id":"price_%s","number":9007199254740993}}`, fromID, toID, from, to)
				if kind == "future_array" {
					raw = fmt.Sprintf(`[{"from_price_id":"price_%s"},9007199254740993]`, from)
				}
				if err := q.CreateNotificationIfAbsent(ctx, gen.CreateNotificationIfAbsentParams{ID: ids[j], MerchantID: mid.UUID(), CustomerID: customer, EventType: kind, Data: []byte(raw)}); err != nil {
					return err
				}
				var stored []byte
				if err := tx.QueryRow(ctx, `SELECT data FROM billing.notification_events WHERE merchant_id=$1 AND id=$2`, mid.UUID(), ids[j]).Scan(&stored); err != nil {
					return err
				}
				facts[billing.NotificationEventRef{MerchantID: mid, ID: billing.NotificationID(ids[j])}] = fact{raw: stored, from: wantFrom.String(), to: wantTo.String(), projected: j < 3}
			}
			return nil
		}))
	}
	check := func(t *testing.T, events []billing.NotificationEvent) {
		t.Helper()
		decodeExact := func(raw []byte) any {
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.UseNumber()
			var value any
			require.NoError(t, decoder.Decode(&value))
			return value
		}
		for _, event := range events {
			f, ok := facts[billing.NotificationEventRef{MerchantID: event.MerchantID, ID: event.ID}]
			require.True(t, ok)
			if !f.projected {
				require.Equal(t, decodeExact(f.raw), decodeExact(event.Data), "unknown kinds and current IDs pass through unchanged")
				continue
			}
			var before, after map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(f.raw, &before))
			require.NoError(t, json.Unmarshal(event.Data, &after))
			require.Len(t, after, len(before))
			for key, value := range before {
				switch key {
				case "from_price_id":
					require.Equal(t, `"`+f.from+`"`, string(after[key]))
				case "to_price_id":
					require.Equal(t, `"`+f.to+`"`, string(after[key]))
				default:
					require.Equal(t, decodeExact(value), decodeExact(after[key]), "opaque fields retain exact values")
				}
			}
		}
	}
	for i, key := range []string{w.shopKey.Secret, w.rivalKey.Secret} {
		t.Run(fmt.Sprintf("merchant-%d", i), func(t *testing.T) {
			path := "/v1/app/notification-consumers/legacy-prices"
			r := call(t, w.handler, key, http.MethodPost, path, nil)
			require.Equal(t, http.StatusNoContent, r.Code, r.Body.String())
			r = call(t, w.handler, key, http.MethodGet, path+"/events", nil)
			require.Equal(t, http.StatusOK, r.Code, r.Body.String())
			events := decode[billing.NotificationEvents](t, r).Data
			require.Len(t, events, len(kinds))
			check(t, events)
		})
	}
	t.Run("global", func(t *testing.T) {
		global, err := w.srv.NotificationEvents("legacy-prices")
		require.NoError(t, err)
		require.NoError(t, global.Register(ctx))
		events, err := global.Pending(ctx, 100)
		require.NoError(t, err)
		require.Len(t, events, len(facts))
		check(t, events)
	})
	for ref, f := range facts {
		var stored []byte
		require.NoError(t, database.DataPool().QueryRow(ctx, `SELECT data FROM billing.notification_events WHERE merchant_id=$1 AND id=$2`, ref.MerchantID.UUID(), ref.ID.UUID()).Scan(&stored))
		require.Equal(t, string(f.raw), string(stored), "delivery projection never rewrites immutable source facts")
	}
}
