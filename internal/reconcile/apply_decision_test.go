package reconcile

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// A provider roster's rebill date is no payment (#1089 §1): an unverified
// row it lists as alive is confirmed only while its paid period still runs,
// and its period never moves. Only a charge renews. Law for every
// provider-read rail; CCBill's roster reports the rebill date as next billing.
func TestRosterDateIsNoPayment(t *testing.T) {
	now := decideNow
	next := now.Add(25 * oneDay)
	for _, rail := range []struct {
		provider Provider
		rail     models.Rail
	}{{ProviderCCBill, models.RailCCBill}, {ProviderStripe, models.RailStripe}, {ProviderNMI, models.RailNMI}} {
		row := func(paidThrough time.Time) *models.Subscription {
			start, end := paidThrough.Add(-30*oneDay), paidThrough
			return &models.Subscription{ID: uuid.New(), Status: models.StatusUnverified, Rail: rail.rail, RailSubscriptionID: "rs",
				CollectionPolicy: models.CollectionPolicyProvider, CurrentPeriodStartsAt: &start, CurrentPeriodEndsAt: &end}
		}
		decide := func(sub *models.Subscription, next time.Time, txns ...RemoteTransaction) Decision {
			snap := &RemoteSnapshot{Provider: rail.provider, FetchedAt: now, Transactions: txns,
				Subscriptions: []RemoteSubscription{{RailSubscriptionID: "rs", Status: SubscriptionStatusActive, NextBillingAt: &next}}}
			return Decide(SubscriptionStateOf(sub), EvidenceBundle{Snapshot: snap}, now, 0)
		}
		apply := func(sub *models.Subscription, d Decision) {
			t.Helper()
			_, err := subscriptions.Transition(sub, eventFor(d, sub, now), now)
			require.NoError(t, err)
		}

		lapsed := row(now.Add(-5 * oneDay))
		if d := decide(lapsed, next); d.Kind != TransitionNone {
			apply(lapsed, d)
		}
		require.Equal(t, models.StatusUnverified, lapsed.Status, "%s: a lapsed row stays unverified", rail.rail)
		require.True(t, lapsed.CurrentPeriodEndsAt.Equal(now.Add(-5*oneDay)), "%s: the period never moves", rail.rail)

		running := row(now.Add(2 * oneDay))
		d := decide(running, running.CurrentPeriodEndsAt.Add(0))
		require.Equal(t, TransitionAdoptPeriodEnd, d.Kind, "%s", rail.rail)
		apply(running, d)
		require.Equal(t, models.StatusActive, running.Status, "%s: confirmed while its paid period runs", rail.rail)
		require.True(t, running.CurrentPeriodEndsAt.Equal(now.Add(2*oneDay)), "%s: confirmation extends nothing", rail.rail)

		paid := row(now.Add(-5 * oneDay))
		d = decide(paid, next, RemoteTransaction{TransactionID: "t1", SubscriptionID: "rs", Type: TransactionTypeSale, Success: true, OccurredAt: now.Add(-5 * oneDay)})
		require.Equal(t, TransitionRenew, d.Kind, "%s", rail.rail)
		apply(paid, d)
		require.Equal(t, models.StatusActive, paid.Status, "%s: a charge renews", rail.rail)
		require.True(t, paid.CurrentPeriodEndsAt.Equal(next), "%s: through the paid period (%s)", rail.rail, paid.CurrentPeriodEndsAt)
	}
}
