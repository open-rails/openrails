//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/reconcile"
)

func TestStripeWonDisputeClosesOnlyObservedWarning(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	l := importLegacy(t, w, "stripe", embedded)
	w.converge()
	w.refreshProviders()
	paid := completed(w.payments(embedded, l.c.id))
	require.Len(t, paid, 1)
	payment := paid[0]
	// Start with a membership restored after a won dispute. Its historical
	// chargeback must remain recorded while later provider reads are checked.
	require.Equal(t, http.StatusOK, w.deliver("stripe", stripeDisputeEvent("charge.dispute.created", "dp_pull", "needs_response", payment)))
	require.Equal(t, billing.SubscriptionCanceled, w.subscription(embedded, l.sub).Status)
	require.Equal(t, http.StatusOK, w.deliver("stripe", stripeDisputeEvent("charge.dispute.closed", "dp_pull", "won", payment)))
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, l.sub).Status)
	database, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	ctx := merchant.WithID(t.Context(), w.client[embedded].MerchantID())
	psp := w.psp["stripe"].UUID()

	pull := func(id, status string) *reconcile.RunResult {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodGet, r.Method, "a provider pull must only read")
			switch r.URL.Path {
			case "/v1/subscriptions", "/v1/charges", "/v1/refunds":
				fmt.Fprint(rw, `{"data":[],"has_more":false}`)
			case "/v1/disputes":
				if id == "" {
					fmt.Fprint(rw, `{"data":[],"has_more":false}`)
					return
				}
				fmt.Fprintf(rw, `{"data":[{"id":%q,"charge":%q,"status":%q,"amount":%d,"currency":"usd","created":%d}],"has_more":false}`,
					id, payment.TransactionID, status, payment.Amount/10_000, w.clock.Now().Unix())
			default:
				t.Errorf("unexpected provider read: %s", r.URL.Path)
				rw.WriteHeader(http.StatusNotFound)
			}
		}))
		defer server.Close()
		fetcher := reconcile.NewStripeFetcher("sk_test_disputes")
		fetcher.BaseURL = server.URL
		reader := reconcile.NewEngine(database, nil, nil, map[reconcile.Provider]reconcile.RailFetcher{reconcile.ProviderStripe: fetcher}, nil)
		reader.Now = w.clock.Now
		var result *reconcile.RunResult
		require.NoError(t, database.RunInMerchantConn(ctx, func(ctx context.Context) error {
			var err error
			result, err = reader.Run(ctx, reconcile.RunParams{Mode: reconcile.ModeAdvisory,
				Since: w.clock.Now().Add(-day), Until: w.clock.Now(),
				PSPs: map[reconcile.Provider]reconcile.PSPBinding{reconcile.ProviderStripe: {ID: psp, Rail: "stripe", AccountID: stripeAcct}},
			})
			return err
		}))
		return result
	}
	warning := func() (uuid.UUID, string, string) {
		t.Helper()
		var id uuid.UUID
		var status, resolution string
		require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT id,status,COALESCE(resolution,'') FROM billing.reconciliation_findings WHERE psp_id=$1 AND finding_type='pull.dispute.chargeback' AND subject_key='dp_pull'`), psp).Scan(&id, &status, &resolution))
		return id, status, resolution
	}

	result := pull("dp_pull", "needs_response")
	assert.Equal(t, 1, result.Summary.Providers["stripe"].FindingsByType[string(reconcile.FindingChargebackActiveSub)])
	id, status, _ := warning()
	assert.Equal(t, "requires_review", status)
	for _, observation := range []struct{ id, status string }{{"", ""}, {"dp_other", "won"}, {"dp_pull", "under_review"}, {"dp_pull", "lost"}} {
		pull(observation.id, observation.status)
		gotID, status, _ := warning()
		assert.Equal(t, id, gotID)
		assert.Equal(t, "requires_review", status, "only an observed win for this dispute closes its warning")
	}

	history := func() string {
		t.Helper()
		var rows string
		require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT jsonb_agg(to_jsonb(p) ORDER BY id)::text FROM billing.payments p WHERE customer_id=$1`), payment.CustomerID.UUID()).Scan(&rows))
		return rows
	}
	var reversals int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.payments WHERE refunded_payment_id=$1 AND reversal_kind='chargeback'`), payment.ID.UUID()).Scan(&reversals))
	require.Equal(t, 1, reversals, "the history check includes an actual chargeback")
	before := history()
	result = pull("dp_pull", "won")
	assert.Zero(t, result.Summary.Providers["stripe"].FindingsByType[string(reconcile.FindingChargebackActiveSub)])
	gotID, status, resolution := warning()
	assert.Equal(t, id, gotID, "keep the warning's history")
	assert.Equal(t, "fixed", status)
	assert.Equal(t, "auto_vanished", resolution)
	assert.EqualValues(t, 1, result.Summary.Providers["stripe"].AutoResolved)
	pull("dp_pull", "won")
	assert.JSONEq(t, before, history(), "closing a warning changes no payments or reversals")
	assert.Equal(t, billing.SubscriptionActive, w.subscription(embedded, l.sub).Status)
	assert.True(t, l.c.entitled(l.ent))
}
