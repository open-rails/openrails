//go:build e2e && integration

package subscriptions_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/stretchr/testify/require"
)

// A visible transaction need not be successful yet. Neither an unknown result
// nor a different attempt marker makes that transaction safe to ignore.
func TestEngineNMIUnsubmittedAttemptInspectsVisibleOrder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, code, condition     string
		current, declined, submit bool
	}{
		{name: "current communication failure", code: "420", current: true},
		{name: "other processor error", code: "400"},
		{name: "other issuer communication failure", code: "421"},
		{name: "unreadable outcome", code: "unknown"},
		{name: "apparently declined but still executable", code: "202", condition: "in_progress", current: true},
		{name: "other attempt is still executable", code: "202", condition: "in_progress"},
		{name: "unknown gateway state", code: "202", condition: "unknown", current: true},
		{name: "recover this attempt's decline", code: "202", current: true, declined: true},
		{name: "old definitive decline permits new attempt", code: "202", submit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			e := enroll(t, w, "nmi", embedded)
			end := e.periodEnd()
			e.refreshBeforePeriodEnd()
			operation := subscriptions.SubscriptionCollectionOperationID(w.client[embedded].MerchantID().UUID(), w.psp["nmi"].UUID(), subscriptions.SubscriptionCollectionPayload{
				Renewal: subscriptions.RenewalTerms{SubscriptionID: e.sub.UUID()}, PreviousPeriodEnd: end,
			})
			description := subscriptions.SubscriptionCollectionDescription(uuid.New())
			if tc.current {
				description = subscriptions.SubscriptionCollectionDescription(operation)
			}
			initial := w.nmi.LastSale()
			remote := w.nmi.AddSale(nmimock.Sale{
				OrderID: subscriptions.ObligationOrderReference(e.sub.UUID(), end), OrderDescription: description,
				Vault: initial.Vault, BillingID: initial.BillingID, Amount: initial.Amount,
				Declined: tc.code, Condition: tc.condition, At: end,
			})
			before := e.providerAttempts()
			e.toPeriodEnd()
			w.runRenewals()
			if tc.submit {
				w.until(func() bool { return e.periodEnd().After(end) }, "a previous definitive refusal does not settle this attempt")
				require.Equal(t, before+1, e.providerAttempts())
				return
			}
			require.Equal(t, before, e.providerAttempts(), "visible unresolved or exact declined provider work must not trigger another sale")
			if tc.declined {
				w.until(func() bool { return w.subscription(embedded, e.sub).Status == billing.SubscriptionPastDue }, "recover the exact provider decline without another submission")
			} else {
				w.until(func() bool { return len(w.openFindings("life.submission.unresolved")) == 1 }, "hold the executable or uncertain provider transaction")
			}
			var fenced bool
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT coalesce(result_evidence, '{}'::jsonb) ? 'submitted_at' FROM billing.provider_intents WHERE id=$1`), operation).Scan(&fenced))
			require.False(t, fenced, "preflight observation never fabricates local submission")
			require.Equal(t, before, e.providerAttempts())
			require.True(t, e.periodEnd().Equal(end))
			if !tc.declined {
				w.nmi.EditSale(remote.TransactionID, func(sale *nmimock.Sale) {
					sale.Declined, sale.Condition = "", "complete"
				})
				w.until(func() bool { return e.periodEnd().After(end) }, "the now-approved provider transaction resolves the held obligation")
				require.Equal(t, before, e.providerAttempts(), "resolution reads the provider instead of submitting a replacement")
				require.Len(t, completed(w.payments(embedded, e.c.id)), 2)
			}
		})
	}
}
