//go:build e2e && integration

package subscriptions_test

import (
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// A refused Solana pull is retried on the merchant's dunning policy, as a
// card renewal is: at its offsets from the first decline, ending after its
// last one.
func TestSolanaPullRetriesFollowMerchantPolicy(t *testing.T) {
	s := openSolanaShop(t)
	w := s.w
	policy := &billing.DunningPolicy{Tiers: []billing.DunningTier{{MaxCycleHours: 96}, {RetryAfterHours: []int{24, 48}}}}
	require.NoError(t, w.applySettings(t.Context(), billing.MerchantSettings{DunningPolicy: policy}))
	b := s.buyer(t, false)
	c := s.checkout(t, b, b.wallet.PublicKey())
	sig := s.land(t, signAs(t, c.bundle, b.wallet), w.clock.Now())
	done, err := b.confirm(c, sig)
	require.NoError(t, err)
	sub := *done.SubscriptionID

	crank := func() {
		t.Helper()
		res, err := w.jobs.Insert(t.Context(), solanaCrankPass{}, &river.InsertOpts{Queue: openrails.QueueBilling})
		require.NoError(t, err)
		w.waitJob(res.Job.ID)
		w.wake()
	}
	pulls := func() int {
		n := 0
		for _, a := range w.attempts(b.id) {
			if a.Kind == "rebill" || a.Kind == "dunning_retry" {
				n++
			}
		}
		return n
	}
	s.fake.Fund(b.wallet.PublicKey(), s.mint, 0)
	w.advance(monthHours*time.Hour + time.Hour)
	first := w.clock.Now()
	crank()
	require.Equal(t, 1, pulls())
	require.Equal(t, billing.SubscriptionPastDue, w.subscription(embedded, sub).Status)
	for i, offset := range []time.Duration{24 * time.Hour, 48 * time.Hour} {
		w.advance(first.Add(offset + time.Minute).Sub(w.clock.Now()))
		crank()
		require.Equal(t, i+2, pulls(), "the retry %d runs at +%s", i+1, offset)
	}
	require.Equal(t, billing.SubscriptionCanceled, w.subscription(embedded, sub).Status, "the policy's last retry ends it")
}
