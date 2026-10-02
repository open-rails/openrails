//go:build greenfield && integration

package subscriptions_test

import (
	"net/http"
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

// "suspend" holds for provider-scheduled members too: a declined provider
// renewal bounds access at the paid period, and the provider's recovery
// reopens it. Stripe retries itself; an NMI schedule is retried by OpenRails.
func TestProviderDunningAccessSuspend(t *testing.T) {
	t.Parallel()
	for _, rail := range []string{"stripe", "nmi"} {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			require.NoError(t, w.client[embedded].SetMerchantSettings(t.Context(), openrails.MerchantSettings{DunningPolicy: &openrails.DunningPolicy{AccessDuringDunning: openrails.DunningAccessSuspend}}))
			l := importLegacy(t, w, rail, embedded)
			w.converge()
			w.advance(l.periodEnd().Sub(w.clock.Now()) + time.Hour)
			require.Equal(t, http.StatusOK, w.deliver(rail, l.providerRenewal(false)))
			w.settle()
			require.Equal(t, "past_due", w.subscription(embedded, l.sub).Status)
			require.False(t, l.c.entitled(l.ent), "suspend ends access with the paid period")

			if rail == "stripe" {
				require.Equal(t, http.StatusOK, w.deliver(rail, l.providerRenewal(true)))
			} else {
				// NMI never retries: OpenRails dunning's first retry recovers it.
				next := w.subscription(embedded, l.sub).NextRetryAt
				require.NotNil(t, next)
				w.advance(next.Sub(w.clock.Now()) + time.Second)
				w.runRenewals()
			}
			w.settle()
			require.Equal(t, "active", w.subscription(embedded, l.sub).Status)
			require.True(t, l.c.entitled(l.ent), "the recovered renewal reopens access")
		})
	}
}

// A dunning case runs under the policy it opened with (#1102): editing the
// merchant's policy mid-case changes the next case, never this one.
func TestDunningCaseKeepsItsPolicy(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	opened := &openrails.DunningPolicy{Tiers: []openrails.DunningTier{{MaxCycleHours: 96}, {RetryAfterHours: []int{24, 48}}}}
	require.NoError(t, w.client[embedded].SetMerchantSettings(t.Context(), openrails.MerchantSettings{DunningPolicy: opened}))
	e := enroll(t, w, "nmi", embedded)
	e.setDecline(visa.Last4, "insufficient_funds", "202")
	e.toPeriodEnd()
	first := w.clock.Now()
	w.runRenewals()
	sub := w.subscription(embedded, e.sub)
	require.Equal(t, "past_due", sub.Status)
	require.Equal(t, 24*time.Hour, sub.NextRetryAt.Sub(first).Round(time.Hour))

	edited := &openrails.DunningPolicy{Tiers: []openrails.DunningTier{{MaxCycleHours: 96}, {RetryAfterHours: []int{36, 60}}}}
	require.NoError(t, w.client[embedded].SetMerchantSettings(t.Context(), openrails.MerchantSettings{DunningPolicy: edited}))
	w.advance(sub.NextRetryAt.Sub(w.clock.Now()) + time.Second)
	w.runRenewals()
	sub = w.subscription(embedded, e.sub)
	require.NotNil(t, sub.NextRetryAt)
	require.Equal(t, 48*time.Hour, sub.NextRetryAt.Sub(first).Round(time.Hour), "the case keeps the policy it opened with")
}
