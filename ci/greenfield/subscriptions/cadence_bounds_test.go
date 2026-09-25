//go:build greenfield && integration

package subscriptions_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/modules/collection"
)

// graceFor is the documented renewal allowance (docs/operations.md): unpaid
// access past a period is min(24h, max(5m, period/10)).
func graceFor(hours int) time.Duration {
	period := time.Duration(hours) * time.Hour
	return min(24*time.Hour, max(5*time.Minute, period/10))
}

func forEachCadence(t *testing.T, run func(t *testing.T, hours int)) {
	for _, hours := range cadences {
		t.Run(fmt.Sprintf("%dh", hours), func(t *testing.T) {
			t.Parallel()
			run(t, hours)
		})
	}
}

// The renewal allowance is a bounded share of the period for every cadence:
// at most a tenth of it, never more than a day, and with the engine halted
// access ends exactly when the allowance does.
func TestEngineCadenceAccessEndsWithTheAllowance(t *testing.T) {
	t.Parallel()
	forEachCadence(t, func(t *testing.T, hours int) {
		period, grace := time.Duration(hours)*time.Hour, graceFor(hours)
		require.LessOrEqual(t, grace*10, period, "free fraction is at most 10%%")
		window, err := collection.Window(hours)
		require.NoError(t, err)
		require.Less(t, window, period, "the dunning window ends inside one cycle")

		w := newWorld(t)
		e := enrollEvery(t, w, "nmi", embedded, hours)
		end := e.periodEnd()
		w.cfg = func(c *config.Config) { c.EngineAdmissionHold = true }
		w.restart()
		w.advance(end.Sub(w.clock.Now()) + grace - time.Second)
		w.runRenewals()
		require.True(t, e.c.entitled(e.ent), "the allowance holds access while the renewal is pending")
		w.advance(2 * time.Second)
		require.False(t, e.c.entitled(e.ent), "with no renewal outcome access ends at period end + %s", grace)
		require.Equal(t, 1, e.providerAttempts(), "a halted engine charges nothing")
		require.True(t, w.subscription(embedded, e.sub).CurrentPeriodEndsAt.Equal(end))
	})
}

// A renewal challenged by the issuer waits for the member no longer than the
// period's allowance, and access never outlasts it while the challenge is open.
func TestEngineCadenceRenewalAuthenticationIsBounded(t *testing.T) {
	t.Parallel()
	forEachCadence(t, func(t *testing.T, hours int) {
		grace := graceFor(hours)
		w := newWorld(t)
		e := enrollEvery(t, w, "stripe", embedded, hours)
		e.setDecline(visa.Last4, "auth", "")
		end := e.periodEnd()
		e.toPeriodEnd()
		accepted := w.clock.Now()
		w.runRenewals()
		require.Equal(t, "active", w.subscription(embedded, e.sub).Status, "an open challenge is not a decline")
		require.True(t, e.c.entitled(e.ent))

		w.advance(accepted.Add(grace).Sub(w.clock.Now()) - time.Second)
		w.wake()
		require.Equal(t, "active", w.subscription(embedded, e.sub).Status, "the challenge stays open for the whole allowance")
		w.advance(2 * time.Second)
		require.False(t, e.c.entitled(e.ent), "access ends at period end + %s even while the challenge is open", grace)
		w.until(func() bool { return w.subscription(embedded, e.sub).Status == "awaiting_method" }, "the abandoned challenge waits for a new card")
		require.False(t, e.c.entitled(e.ent))
		require.Empty(t, e.providerLedger()[1:], "the challenged renewal never charged")
		require.True(t, w.subscription(embedded, e.sub).CurrentPeriodEndsAt.Equal(end))
	})
}

// Paul's decline policy: below 96h the first renewal decline is terminal; from
// 96h the cadence's retry tier runs (+1d for weekly, +2d from 28 days).
func TestEngineCadenceFirstDecline(t *testing.T) {
	t.Parallel()
	forEachCadence(t, func(t *testing.T, hours int) {
		w := newWorld(t)
		w.armDestructive()
		e := enrollEvery(t, w, "nmi", embedded, hours)
		e.setDecline(visa.Last4, "insufficient_funds", "202")
		e.toPeriodEnd()
		first := w.clock.Now()
		w.runRenewals()
		sub := w.subscription(embedded, e.sub)
		require.False(t, e.c.entitled(e.ent), "a decline ends the allowance")
		switch {
		case hours < collection.MinRetryCycleHours:
			require.Equal(t, "cancelled", sub.Status, "the first decline is terminal below 96h")
			require.Nil(t, sub.NextRetryAt)
		case hours < collection.MonthlyCycleHours:
			require.Equal(t, "past_due", sub.Status)
			require.NotNil(t, sub.NextRetryAt)
			require.Equal(t, 24*time.Hour, sub.NextRetryAt.Sub(first).Round(time.Hour), "weekly tier retries a day later")
		default:
			require.Equal(t, "past_due", sub.Status)
			require.NotNil(t, sub.NextRetryAt)
			require.Equal(t, 48*time.Hour, sub.NextRetryAt.Sub(first).Round(time.Hour), "monthly tier retries two days later")
		}
	})
}

// A membership whose price has lost its cadence is never dunned on a guessed
// month: the due pass refuses to rebill it, raises an operator finding and
// schedules, charges and ends nothing.
func TestUnknownCadenceFailsClosed(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	l := importLegacy(t, w, "nmi", embedded)
	w.converge()
	priceID, err := openrails.ParsePriceID(l.price.ID)
	require.NoError(t, err)
	schema := pgx.Identifier{w.schema}.Sanitize()
	_, err = w.pool.Exec(t.Context(), `UPDATE `+schema+`.prices SET auto_renew = false, access_duration_hours = NULL WHERE id = $1`, uuid.UUID(priceID))
	require.NoError(t, err)
	w.advance(l.periodEnd().Sub(w.clock.Now()) + time.Hour)
	// Mid-dunning when the cadence went missing: a retry is due now.
	_, err = w.pool.Exec(t.Context(), `UPDATE `+schema+`.subscriptions SET lifecycle_rev = lifecycle_rev + 1, status = 'past_due', retry_attempts = 1, next_retry_at = $2 WHERE id = $1`, uuid.UUID(l.sub), w.clock.Now())
	require.NoError(t, err)
	charges := l.engineCharges()
	w.runRenewals()
	sub := w.subscription(embedded, l.sub)
	require.Equal(t, "past_due", sub.Status)
	require.Nil(t, sub.CancelledAt, "nothing ends on a guessed schedule")
	require.Equal(t, charges, l.engineCharges(), "nothing is charged on a guessed schedule")
	require.Contains(t, w.openFindings(collection.FindingUnknownCycle), uuid.UUID(l.sub).String(), "the operator is told")
}
