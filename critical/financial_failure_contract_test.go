//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

type capturedLogs struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *capturedLogs) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(data)
}
func (b *capturedLogs) text() string { b.mu.Lock(); defer b.mu.Unlock(); return b.Buffer.String() }

func TestFinancialFailures(t *testing.T) {
	t.Run("OR-J09-S04/database-abort-rolls-back-money-access-and-event-together", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		fleet := hostFleet(t, a.world)
		input := saleInput(t, a, p)
		// Real PostgreSQL aborts the settlement transaction at its retained-event
		// write. This is schema-local storage fault injection, not seeded money
		// or a replacement internal transaction/store implementation.
		fn, table := pgx.Identifier{a.schema, "critical_refuse_settlement"}.Sanitize(), pgx.Identifier{a.schema, "eventlog_events"}.Sanitize()
		_, err := a.pool.Exec(t.Context(), "CREATE FUNCTION "+fn+"() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'critical storage refusal'; END $$; CREATE TRIGGER critical_refuse_settlement BEFORE INSERT ON "+table+" FOR EACH ROW WHEN (NEW.event_type='payment.settled') EXECUTE FUNCTION "+fn+"()")
		require.NoError(t, err)
		remove := func(ctx context.Context) error {
			_, err := a.pool.Exec(ctx, "DROP TRIGGER IF EXISTS critical_refuse_settlement ON "+table+"; DROP FUNCTION IF EXISTS "+fn+"()")
			return err
		}
		t.Cleanup(func() { require.NoError(t, remove(context.Background())) })
		status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "storage-abort-sale"})
		if status < 400 {
			var response billing.Order
			require.NoError(t, json.Unmarshal(raw, &response))
			require.NotEqual(t, billing.OrderComplete, response.Status, "a processing receipt cannot report rolled-back settlement as complete")
			require.NotEqual(t, billing.OrderPaymentSucceeded, response.Payment.Status)
		}
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		orders, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Len(t, orders.Items, 1)
		require.NotEqual(t, billing.OrderComplete, orders.Items[0].Status)
		payments, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: customer})
		require.NoError(t, err)
		for _, payment := range payments.Items {
			require.NotEqual(t, billing.PaymentSucceeded, payment.Status)
		}
		facts, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"payment.settled", "order.completed"}, Limit: 100})
		require.NoError(t, err)
		require.Empty(t, facts.Events)
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{"sale:owned"}})
		require.NoError(t, err)
		require.False(t, access.Entitlements["sale:owned"])
		require.Len(t, p.effects(t), 1, "the storage abort occurs after the actual external debit")
		require.NoError(t, remove(t.Context()))
		recoverSale(t, a, p, fleet, orders.Items[0].ID)
	})
	t.Run("OR-J12-S05/capture-values-and-private-provider-secrets-never-escape", func(t *testing.T) {
		logs := &capturedLogs{}
		previous, level := log.StandardLogger().Out, log.GetLevel()
		log.SetOutput(logs)
		log.SetLevel(log.DebugLevel)
		t.Cleanup(func() { log.SetOutput(previous); log.SetLevel(level) })
		a, p := newPaymentWorld(t)
		psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		require.Len(t, psps.Items, 1)
		const pan, cvc = "4111111111111111", "867"
		status, refused := a.request(t, http.MethodPost, "/v1/me/payment-methods", a.aliceToken, map[string]any{"psp_id": psps.Items[0].ID, "pan": pan, "cvc": cvc})
		require.Equal(t, http.StatusBadRequest, status, string(refused))
		input := saleInput(t, a, p)
		status, purchase := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "redacted-purchase"})
		require.Equal(t, http.StatusCreated, status, string(purchase))
		var outputs bytes.Buffer
		outputs.Write(refused)
		outputs.Write(purchase)
		for _, path := range []string{"/v1/config", "/v1/me/config", "/v1/me/payment-methods", "/v1/me/orders", "/v1/me/agreements"} {
			status, raw := a.request(t, http.MethodGet, path, a.aliceToken, nil)
			require.Equal(t, http.StatusOK, status, string(raw))
			outputs.Write(raw)
		}
		for _, value := range []any{psps, func() any {
			p, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{})
			require.NoError(t, err)
			return p
		}(), func() any {
			e, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Limit: 100})
			require.NoError(t, err)
			return e
		}()} {
			data, err := json.Marshal(value)
			require.NoError(t, err)
			outputs.Write(data)
		}
		for _, secret := range []string{pan, "sk_test_critical", "whsec_critical_fixture"} {
			require.NotContains(t, outputs.String(), secret)
			require.NotContains(t, logs.text(), secret)
		}
		require.NotContains(t, outputs.String(), `"cvc":"`+cvc+`"`)
		require.NotContains(t, logs.text(), `"cvc":"`+cvc+`"`)
		require.Len(t, p.effects(t), 1)
		require.EqualValues(t, 3000, p.effects(t)[0].Amount)
	})
}
