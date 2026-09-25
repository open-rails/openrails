//go:build greenfield && integration

package subscriptions_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

// A merchant's declared dunning policy replaces the built-in schedule: retries
// follow its offsets from the first decline and it ends after its last one.
// An invalid policy is refused whole.
func TestMerchantDunningPolicy(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	bad := &openrails.DunningPolicy{Tiers: []openrails.DunningTier{{RetryAfterHours: []int{24, 900}}}}
	require.Error(t, w.client[embedded].SetMerchantSettings(t.Context(), openrails.MerchantSettings{DunningPolicy: bad}), "retries past the cycle are refused")
	policy := &openrails.DunningPolicy{Tiers: []openrails.DunningTier{{MaxCycleHours: 96}, {RetryAfterHours: []int{24, 48}}}}
	require.NoError(t, w.client[embedded].SetMerchantSettings(t.Context(), openrails.MerchantSettings{DunningPolicy: policy}))
	got, err := w.client[embedded].GetMerchantSettings(t.Context())
	require.NoError(t, err)
	require.Equal(t, policy, got.DunningPolicy)

	e := enroll(t, w, "nmi", embedded)
	e.setDecline(visa.Last4, "insufficient_funds", "202")
	e.toPeriodEnd()
	first := w.clock.Now()
	w.runRenewals()
	var offsets []time.Duration
	for range 5 {
		sub := w.subscription(embedded, e.sub)
		if sub.NextRetryAt == nil {
			break
		}
		offsets = append(offsets, sub.NextRetryAt.Sub(first).Round(time.Hour))
		w.advance(sub.NextRetryAt.Sub(w.clock.Now()) + time.Second)
		w.runRenewals()
	}
	require.Equal(t, []time.Duration{24 * time.Hour, 48 * time.Hour}, offsets)
	require.Equal(t, "cancelled", w.subscription(embedded, e.sub).Status, "the policy's last retry ends it")
	require.Equal(t, 4, e.providerAttempts(), "initial charge plus three renewal attempts")
}

// Access during dunning is policy: by default a member keeps access while a
// declined renewal is retried; "suspend" ends it with the paid period and a
// recovered renewal restores it.
func TestDunningAccessPolicy(t *testing.T) {
	t.Parallel()
	for _, access := range []string{openrails.DunningAccessKeep, openrails.DunningAccessSuspend} {
		t.Run(access, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			policy := &openrails.DunningPolicy{Tiers: []openrails.DunningTier{{MaxCycleHours: 96}, {MaxCycleHours: 672, RetryAfterHours: []int{24, 48}}, {RetryAfterHours: []int{48, 120, 216, 312}}}, AccessDuringDunning: access}
			require.NoError(t, w.client[embedded].SetMerchantSettings(t.Context(), openrails.MerchantSettings{DunningPolicy: policy}))
			e := enroll(t, w, "stripe", embedded)
			e.setDecline(visa.Last4, "insufficient_funds", "202")
			e.toPeriodEnd()
			w.runRenewals()
			sub := w.subscription(embedded, e.sub)
			require.Equal(t, "past_due", sub.Status)
			w.advance(24 * time.Hour)
			require.Equal(t, access == openrails.DunningAccessKeep, e.c.entitled(e.ent), "access during dunning follows the policy")

			e.setDecline(visa.Last4, "", "")
			w.advance(sub.NextRetryAt.Sub(w.clock.Now()) + time.Second)
			w.runRenewals()
			require.Equal(t, "active", w.subscription(embedded, e.sub).Status)
			require.True(t, e.c.entitled(e.ent), "the recovered renewal restores access")
		})
	}
}
