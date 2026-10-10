//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/failpoint"
)

type solanaCrankPass struct{}

func (solanaCrankPass) Kind() string { return "openrails.solana_crank" }

// A refused recurring pull is one attempt: a crank that crashes after
// recording it and before failing the membership is retried, and the retry
// records nothing new.
func TestSolanaRefusedPullRecordedOnce(t *testing.T) {
	s := openSolanaShop(t)
	w := s.w
	b := s.buyer(t, false)
	c := s.checkout(t, b, b.wallet.PublicKey())
	sig := s.land(t, signAs(t, c.bundle, b.wallet), w.clock.Now())
	done, err := b.confirm(c, sig)
	require.NoError(t, err)
	require.NotNil(t, done.SubscriptionID)
	sub := *done.SubscriptionID

	// The wallet is empty when the period ends: the pull is refused on-chain.
	s.fake.Fund(b.wallet.PublicKey(), s.mint, 0)
	w.advance(monthHours*time.Hour + time.Hour)
	crashes := 0
	remove := failpoint.Set(func(_ context.Context, site failpoint.Site) error {
		if site.Point != failpoint.AfterAttempt || site.Subscription != sub.UUID() || crashes > 0 {
			return nil
		}
		crashes++
		return errors.New("crash after the attempt is recorded")
	})
	defer remove()

	crank := func() {
		res, err := w.jobs.Insert(t.Context(), solanaCrankPass{}, &river.InsertOpts{Queue: openrails.QueueBilling})
		require.NoError(t, err)
		w.waitJob(res.Job.ID)
		w.wake()
	}
	crank()
	require.Equal(t, 1, crashes, "the first crank crashed after recording")
	w.until(func() bool { return w.subscription(embedded, sub).Status == "past_due" }, "the retried pull fails the membership")

	var rebills []attempt
	for _, a := range w.attempts(b.id) {
		if a.Kind != "verify" && a.Kind != "initial" {
			rebills = append(rebills, a)
		}
	}
	require.Len(t, rebills, 1, "one refused pull, one attempt")
	require.Equal(t, []string{"rebill", "issuer_soft", "insufficient_funds"}, []string{rebills[0].Kind, rebills[0].Category, str(rebills[0].Reason)})
}
