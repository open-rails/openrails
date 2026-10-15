//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestManualHyperSwitchCardRemoval(t *testing.T) {
	for _, scenario := range []string{"confirmed", "provider unavailable", "lost receipt", "wrong receipt"} {
		t.Run(scenario, func(t *testing.T) {
			w, m := newHyperSwitchMember(t)
			id := billing.PaymentMethodID(m.method).String()
			m.c.must(http.MethodPost, "/subscriptions/"+m.sub.String()+"/cancel", "", map[string]any{"reason": "ending"})
			// The reusable HS fixture inserts its card directly. Give it an
			// existing holder observation so removal must preserve that history.
			_, err := w.pool.Exec(t.Context(), w.q(`INSERT INTO billing.payment_method_versions
                (merchant_id,customer_id,payment_method_id,source,kind,event_ref,custodian_id,
                 rail_customer_ref,rail_method_ref,card_brand,card_last4,card_exp_month,card_exp_year,effective_at)
                SELECT merchant_id,customer_id,id,'provider_read','saved','manual-delete-fixture',custodian_id,
                       rail_customer_ref,rail_method_ref,card_brand,card_last4,card_exp_month,card_exp_year,$2
                FROM billing.payment_methods WHERE id=$1`), m.method, w.clock.Now())
			require.NoError(t, err)
			var orderRefs, versions int
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.orders WHERE payment_method_id=$1`), m.method).Scan(&orderRefs))
			require.Positive(t, orderRefs, "the fixture must reproduce referenced card history")
			m.hs.mu.Lock()
			m.hs.deleteCapability = true
			m.hs.deleteLoseResponse = scenario == "lost receipt"
			m.hs.deleteMismatch = scenario == "wrong receipt"
			if scenario == "provider unavailable" {
				m.hs.deleteStatus = http.StatusServiceUnavailable
			}
			m.hs.mu.Unlock()
			status, out := m.c.call(http.MethodDelete, "/payment-methods/"+id, "", nil)
			if scenario != "confirmed" {
				require.Equal(t, http.StatusServiceUnavailable, status, "%v", out)
				m.hs.mu.Lock()
				m.hs.deleteStatus, m.hs.deleteLoseResponse, m.hs.deleteMismatch = 0, false, false
				m.hs.mu.Unlock()
				w.advance(time.Hour)
				w.wake()
				m.hs.mu.Lock()
				require.Equal(t, []string{"pm_hs_e2e"}, m.hs.deletedMethods, "workers cannot retry a user's removal")
				m.hs.mu.Unlock()
				status, out = m.c.call(http.MethodDelete, "/payment-methods/"+id, "", nil)
			}
			require.Equal(t, http.StatusNoContent, status, "%v", out)
			require.Empty(t, m.c.must(http.MethodGet, "/payment-methods", "", nil)["data"])
			var state, marker string
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT status,park_reason FROM billing.payment_methods WHERE id=$1`), m.method).Scan(&state, &marker))
			require.Equal(t, "removed", state)
			require.Contains(t, marker, "removed:")
			var remainingRefs, jobs int
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.orders WHERE payment_method_id=$1`), m.method).Scan(&remainingRefs))
			require.Equal(t, orderRefs, remainingRefs)
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.payment_method_versions WHERE payment_method_id=$1`), m.method).Scan(&versions))
			require.Positive(t, versions)
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.river_job j JOIN billing.provider_intents i ON i.id::text=j.args->>'intent_id' WHERE i.intent_type='hyperswitch_method_delete' AND i.payload->>'payment_method_id'=$1`), m.method.String()).Scan(&jobs))
			require.Zero(t, jobs)
			m.hs.mu.Lock()
			want := []string{"pm_hs_e2e"}
			if scenario != "confirmed" {
				want = append(want, "pm_hs_e2e")
			}
			require.Equal(t, want, m.hs.deletedMethods, "qualified repeatable DELETE always names the same accepted method")
			m.hs.mu.Unlock()
			status, out = m.c.call(http.MethodDelete, "/payment-methods/"+id, "", nil)
			require.Equal(t, http.StatusNoContent, status, "%v", out)
			events, err := w.client[embedded].ListHostEvents(t.Context(), billing.HostEventListParams{})
			require.NoError(t, err)
			for _, event := range events.Items {
				_, err = w.client[embedded].AcknowledgeHostEvents(t.Context(), []billing.HostEventID{event.ID})
				require.NoError(t, err)
			}
			require.NoError(t, w.exportArchive(), "retained removal history must remain exportable")
		})
	}
}
