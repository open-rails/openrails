//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/writeposture"
)

func setRecoveryPosture(t *testing.T, w *world, mode writeposture.Mode) {
	t.Helper()
	mid := w.client[embedded].MerchantID()
	database, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	require.NoError(t, database.RunInMerchantScope(t.Context(), mid, "recovery view test", func(ctx context.Context) error {
		return writeposture.Set(ctx, database.Gen(ctx), mid.UUID(), mode, writeposture.ReasonOperator, "recovery view test", w.clock.Now())
	}))
}

// The same saved schedule is visible in every mode. Automatic collection and
// customer payment have different gates; neither a read nor clearing a hold
// changes the schedule or attempts a payment.
func TestSubscriptionRecoveryPausePreservesSchedule(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	e.refreshBeforePeriodEnd()
	e.setDecline(visa.Last4, "insufficient_funds", "202")
	e.toPeriodEnd()
	w.runRenewals()
	w.settle()
	before := *w.subscription(embedded, e.sub).Dunning
	require.NotNil(t, before.NextRetryAt)
	attempts := e.providerAttempts()
	for _, tc := range []struct {
		name, process, reason string
		merchant              writeposture.Mode
		paused, retryable     bool
	}{
		{"full", config.ProviderWriteModeFull, "", writeposture.Full, false, true},
		{"process limited", config.ProviderWriteModeLimited, "automatic_collection_paused", writeposture.Full, true, true},
		{"process readonly", config.ProviderWriteModeReadOnly, billing.CodeProviderWritesDisabled, writeposture.Full, true, false},
		{"merchant limited", config.ProviderWriteModeFull, "automatic_collection_paused", writeposture.Limited, true, true},
		{"merchant readonly", config.ProviderWriteModeFull, billing.CodeProviderWritesDisabled, writeposture.ReadOnly, true, false},
		{"restored", config.ProviderWriteModeFull, "", writeposture.Full, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setRecoveryPosture(t, w, tc.merchant)
			w.cfg = func(c *config.Config) { c.ProviderWriteMode = tc.process }
			w.restart()
			want := before
			want.Paused, want.PauseReason = tc.paused, tc.reason
			for _, tp := range []topology{embedded, remote} {
				require.Equal(t, &want, w.subscription(tp, e.sub).Dunning)
				page, err := w.client[tp].ListSubscriptions(t.Context(), billing.SubscriptionListParams{IDs: []billing.SubscriptionID{e.sub}})
				require.NoError(t, err)
				require.Len(t, page.Items, 1)
				require.Equal(t, &want, page.Items[0].Dunning)
			}
			own := unwrap(e.c.must(http.MethodGet, "/subscriptions/"+e.sub.String(), "", nil))
			dunning := own["dunning"].(map[string]any)
			require.Equal(t, tc.paused, dunning["paused"])
			if tc.paused {
				require.Equal(t, tc.reason, dunning["pause_reason"])
			} else {
				require.NotContains(t, dunning, "pause_reason")
			}
			planned, err := time.Parse(time.RFC3339Nano, dunning["next_retry_at"].(string))
			require.NoError(t, err)
			require.True(t, before.NextRetryAt.Equal(planned))
			recovery := own["recovery"].(map[string]any)
			require.Equal(t, tc.retryable, recovery["retryable"])
			require.NotEmpty(t, recovery["order_id"])
			require.Equal(t, "insufficient_funds", recovery["last_failure_reason"])
			if !tc.retryable {
				require.Equal(t, billing.CodeProviderWritesDisabled, recovery["blocked_reason"])
				refused := e.payRenewal(e.method)
				require.Equal(t, http.StatusServiceUnavailable, refused.status, "%v", refused.body)
				require.Equal(t, billing.CodeProviderWritesDisabled, orderError(refused))
			} else {
				require.NotContains(t, recovery, "blocked_reason")
			}
			require.Equal(t, attempts, e.providerAttempts(), "reads and refused payments never charge")
		})
	}
}

func TestNMIScheduleRecoveryPause(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	l := importLegacy(t, w, "nmi", embedded, declareRecurringAnchor)
	w.converge()
	setRecoveryPosture(t, w, writeposture.ReadOnly)
	w.advance(l.periodEnd().Sub(w.clock.Now()) + time.Hour)
	require.Equal(t, http.StatusOK, w.deliver("nmi", l.providerRenewal(false)))
	w.settle()
	view := unwrap(l.c.must(http.MethodGet, "/subscriptions/"+l.sub.String(), "", nil))
	recovery := view["recovery"].(map[string]any)
	require.Equal(t, false, recovery["retryable"])
	require.Equal(t, billing.CodeProviderWritesDisabled, recovery["blocked_reason"])
	planned := view["dunning"].(map[string]any)["next_retry_at"]

	setRecoveryPosture(t, w, writeposture.Limited)
	view = unwrap(l.c.must(http.MethodGet, "/subscriptions/"+l.sub.String(), "", nil))
	require.Equal(t, true, view["recovery"].(map[string]any)["retryable"])
	dunning := view["dunning"].(map[string]any)
	require.Equal(t, true, dunning["paused"])
	require.Equal(t, "automatic_collection_paused", dunning["pause_reason"])
	require.Equal(t, planned, dunning["next_retry_at"])
	before := len(w.nmi.Attempts())
	l.c.must(http.MethodPost, "/subscriptions/"+l.sub.String()+"/retry-now", "limited-recovery", map[string]any{})
	require.Equal(t, before+1, len(w.nmi.Attempts()), "limited mode permits a customer payment")
}

func TestUnresolvedRecoveryKeepsReasonWhileReadonly(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	e.refreshBeforePeriodEnd()
	w.loseSubmissions("nmi", 1)
	w.readUnavailable("nmi", true)
	e.toPeriodEnd()
	w.runRenewals()
	setRecoveryPosture(t, w, writeposture.ReadOnly)
	before := e.providerAttempts()
	view := unwrap(e.c.must(http.MethodGet, "/subscriptions/"+e.sub.String(), "", nil))
	recovery := view["recovery"].(map[string]any)
	require.Equal(t, false, recovery["retryable"])
	require.Equal(t, "payment_in_progress", recovery["blocked_reason"])
	require.NotContains(t, recovery, "last_failure_reason", "an unknown outcome is not a decline")
	require.Equal(t, before, e.providerAttempts())
}
