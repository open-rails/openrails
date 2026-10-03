//go:build e2e && integration

package subscriptions_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// The portable billing archive's schema check classifies every subscription
// column the lifecycle added: an export is never refused for the schema
// itself (count 0), only for live rows it cannot move yet.
func TestBillingArchiveClassifiesLifecycleColumns(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "stripe", embedded)
	e.toPeriodEnd()
	w.runRenewals()
	status, body := w.staff(http.MethodGet, "/v1/merchant/billing-archive")
	if status == http.StatusOK {
		return
	}
	var refusal struct {
		Error struct {
			Code     string `json:"code"`
			Metadata struct {
				Table string `json:"table"`
				Count int    `json:"count"`
			} `json:"metadata"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &refusal), body)
	require.Positive(t, refusal.Error.Metadata.Count, "a refusal names live rows, never an unclassified schema: %s", body)
}

// #1099: idempotency claims are not moved by the archive, but a request still
// running under a live claim holds the export back; settled and lapsed claims
// never do.
func TestBillingArchiveWaitsForALiveClaim(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	archiveRefusal := func() (string, int) {
		status, body := w.staff(http.MethodGet, "/v1/merchant/billing-archive")
		if status == http.StatusOK {
			return "", 0
		}
		var refusal struct {
			Error struct {
				Metadata struct {
					Table string `json:"table"`
					Count int    `json:"count"`
				} `json:"metadata"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &refusal), body)
		return refusal.Error.Metadata.Table, refusal.Error.Metadata.Count
	}
	_, err := w.pool.Exec(t.Context(), w.q(`INSERT INTO billing.idempotency_keys (merchant_id, operation, idempotency_key, status, token, result, lease_expires_at, expires_at)
		SELECT id, 'checkout_session_create', k, s, gen_random_uuid(), CASE WHEN s = 'succeeded' THEN '{}'::jsonb END, now() + interval '1 hour', now() + interval '1 day'
		FROM billing.merchants, (VALUES ('running', 'processing'), ('done', 'succeeded')) AS v(k, s) WHERE slug = $1`), w.slug)
	require.NoError(t, err)
	table, count := archiveRefusal()
	require.Equal(t, "idempotency_keys", table)
	require.Equal(t, 1, count, "only the live claim holds the export back")

	_, err = w.pool.Exec(t.Context(), w.q(`UPDATE billing.idempotency_keys SET lease_expires_at = now() - interval '1 second' WHERE idempotency_key = 'running'`))
	require.NoError(t, err)
	table, _ = archiveRefusal()
	require.NotEqual(t, "idempotency_keys", table, "a lapsed claim does not block the export")
}

// A membership refused before declines became payment attempts (#1111) left a
// failed payments row under its payment id. The archive keeps accepting that
// record; any other payment under a refused enrollment is refused.
func TestBillingArchiveKeepsPreCutDeclineRecords(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	price := w.membership("content:members", 9_990_000)
	h := hostedPay{w: w, c: w.newCustomer(), tp: embedded, price: price.ID}
	_, err := h.pay("pay-nsf", billing.CheckoutPaymentOptions{PaymentToken: w.nmi.Tokenize(card{Brand: "visa", Last4: "0002", Decline: "202"})})
	require.ErrorIs(t, err, billing.ErrPaymentRefused)
	w.settle()
	refusedTable := func() string {
		status, body := w.staff(http.MethodGet, "/v1/merchant/billing-archive")
		if status == http.StatusOK {
			return ""
		}
		var refusal struct {
			Error struct {
				Metadata struct {
					Table string `json:"table"`
				} `json:"metadata"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &refusal), body)
		return refusal.Error.Metadata.Table
	}
	require.NotEqual(t, "rail_intents", refusedTable())

	// The decline record the checkout wrote before #1111.
	record, err := w.pool.Exec(t.Context(), w.q(`INSERT INTO billing.payments (merchant_id, id, customer_id, price_id, psp_id, rail, transaction_id, amount, list_amount, currency, status, money_movement)
		SELECT merchant_id, (payload->'terms'->>'payment_id')::uuid, (payload->'terms'->>'customer_id')::uuid, (payload->'terms'->>'price_id')::uuid,
		       psp_id, rail, rail || '_sub_declined:' || id, (payload->'terms'->>'amount')::bigint, (payload->'terms'->>'recurring_amount')::bigint,
		       payload->'terms'->>'currency', 'failed', 'none'
		FROM billing.rail_intents WHERE intent_type = 'initial_membership' AND status = 'failed_terminal'`))
	require.NoError(t, err)
	require.EqualValues(t, 1, record.RowsAffected())
	require.NotEqual(t, "rail_intents", refusedTable(), "a pre-#1111 decline record is not a payment")

	_, err = w.pool.Exec(t.Context(), w.q(`UPDATE billing.payments SET status = 'completed' WHERE transaction_id LIKE '%_sub_declined:%'`))
	require.NoError(t, err)
	require.Equal(t, "rail_intents", refusedTable(), "a completed payment under a refused enrollment")
	_, err = w.pool.Exec(t.Context(), w.q(`DELETE FROM billing.payments WHERE transaction_id LIKE '%_sub_declined:%'`))
	require.NoError(t, err)
}
