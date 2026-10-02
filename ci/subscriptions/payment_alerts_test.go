//go:build greenfield && integration

package subscriptions_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/embed"
)

// attemptSeed is a batch of recorded answers: the alerting reads what the
// recorders wrote, so the scenarios below write that history directly rather
// than driving hundreds of charges.
type attemptSeed struct {
	psp, rail, kind, owner, cardEntry, category, reason, code string
	n                                                         int
	at                                                        time.Time
	// cycles gives each attempt its own rebill cycle, due a minute earlier.
	cycles bool
	// via is how OpenRails learned a provider schedule's charge (webhook or
	// pull); empty is OpenRails' own attempt, answered in the response.
	via string
}

func (s attemptSeed) source() (string, string) {
	if s.via == "" {
		return "openrails", "response"
	}
	return "provider_schedule", s.via
}

func (w *world) seedAttempts(customerID string, s attemptSeed) {
	w.t.Helper()
	source, via := s.source()
	if s.cycles {
		_, err := w.pool.Exec(w.t.Context(), w.q(`WITH m AS (SELECT id FROM openrails.merchants WHERE slug = $1),
			c AS (INSERT INTO openrails.rebill_cycles (merchant_id, subscription_id, customer_id, psp_id, rail, owner, due_at, amount, currency)
				SELECT m.id, gen_random_uuid(), $2::uuid, $3::uuid, $4, $5, $6::timestamptz - (g * interval '1 second') - interval '1 minute', 9990000, 'USD'
				FROM m, generate_series(1, $7::int) g RETURNING id, merchant_id, subscription_id, due_at)
			INSERT INTO openrails.payment_attempts (merchant_id, customer_id, psp_id, rail, kind, owner, card_entry, source, observed_via, category, reason, response_code, amount, currency, attempted_at, cycle_id, subscription_id)
			SELECT c.merchant_id, $2::uuid, $3::uuid, $4, 'rebill', $5, 'saved', $11, $12, $8, NULLIF($9, ''), NULLIF($10, ''), 9990000, 'USD', c.due_at + interval '1 minute', c.id, c.subscription_id FROM c`),
			w.slug, customerID, s.psp, s.rail, s.owner, s.at, s.n, s.category, s.reason, s.code, source, via)
		require.NoError(w.t, err)
		return
	}
	_, err := w.pool.Exec(w.t.Context(), w.q(`INSERT INTO openrails.payment_attempts (merchant_id, customer_id, psp_id, rail, kind, owner, card_entry, source, observed_via, category, reason, response_code, amount, currency, attempted_at)
		SELECT m.id, $2::uuid, $3::uuid, $4, $5, $6, $7, $13, $14, $8, NULLIF($9, ''), NULLIF($10, ''), 9990000, 'USD', $11::timestamptz - (g * interval '1 second')
		FROM openrails.merchants m, generate_series(1, $12::int) g WHERE m.slug = $1`),
		w.slug, customerID, s.psp, s.rail, s.kind, s.owner, s.cardEntry, s.category, s.reason, s.code, s.at, s.n, source, via)
	require.NoError(w.t, err)
}

// A burst of rebill declines on one PSP raises one finding for that PSP and
// owner and none for the other; it resolves once the PSP recovers. Below the
// volume gate nothing fires.
func TestRebillFailureSpikeAlerts(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	now := w.clock.Now()
	declined := attemptSeed{rail: "nmi", owner: "engine", category: "issuer_soft", reason: "insufficient_funds", code: "201", cycles: true}
	collected := attemptSeed{rail: "nmi", owner: "engine", category: "approved", cycles: true}
	// Four weeks at 5% for both PSPs, then a day at 80% on NMI only.
	for _, psp := range []string{w.psp["nmi"], w.psp["stripe"]} {
		base := now.Add(-3 * 24 * time.Hour)
		ok, bad := collected, declined
		ok.psp, bad.psp, ok.at, bad.at, ok.n, bad.n = psp, psp, base, base, 190, 10
		w.seedAttempts(c.id, ok)
		w.seedAttempts(c.id, bad)
	}
	ok, bad := collected, declined
	ok.psp, bad.psp, ok.at, bad.at, ok.n, bad.n = w.psp["nmi"], w.psp["nmi"], now, now, 12, 48
	w.seedAttempts(c.id, ok)
	w.seedAttempts(c.id, bad)
	healthy := collected
	healthy.psp, healthy.at, healthy.n = w.psp["stripe"], now, 60
	w.seedAttempts(c.id, healthy)

	w.converge()
	spikes := w.findings("life.payments.rebill_failure_spike")
	require.Len(t, spikes, 1, "one PSP and owner")
	require.Equal(t, "psp:greenfield-nmi:owner:engine", spikes[0].subject)
	require.Equal(t, "high", spikes[0].severity)
	require.Contains(t, spikes[0].action, "80.0%")
	require.Empty(t, w.findings("life.payments.new_card_decline_spike"))

	// The next day NMI is back to normal: the episode resolves.
	w.advance(25 * time.Hour)
	ok.at, ok.n = w.clock.Now(), 60
	w.seedAttempts(c.id, ok)
	w.converge()
	require.Empty(t, w.findings("life.payments.rebill_failure_spike"))

	// Below the volume gate a bad day raises nothing.
	w.advance(25 * time.Hour)
	bad.at, bad.n = w.clock.Now(), 40
	w.seedAttempts(c.id, bad)
	w.converge()
	require.Empty(t, w.findings("life.payments.rebill_failure_spike"), "40 cycles are under the gate")
}

// New-card declines ten points above their baseline raise a spike; system
// errors and a refused merchant account raise their own findings; an unmapped
// decline code is named.
func TestPaymentHealthAlerts(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	now := w.clock.Now()
	nmi := func(kind, category, reason, code string, n int, at time.Time) attemptSeed {
		return attemptSeed{psp: w.psp["nmi"], rail: "nmi", kind: kind, owner: "engine", cardEntry: "new", category: category, reason: reason, code: code, n: n, at: at}
	}
	week := now.Add(-7 * 24 * time.Hour)
	w.seedAttempts(c.id, nmi("initial", "approved", "", "", 90, week))
	w.seedAttempts(c.id, nmi("initial", "issuer_soft", "insufficient_funds", "202", 10, week))
	w.seedAttempts(c.id, nmi("verify", "approved", "", "", 35, now))
	w.seedAttempts(c.id, nmi("verify", "card_data", "incorrect_cvc", "225", 25, now))
	w.converge()
	spikes := w.findings("life.payments.new_card_decline_spike")
	require.Len(t, spikes, 1)
	require.Equal(t, "psp:greenfield-nmi:owner:engine", spikes[0].subject)
	require.Contains(t, spikes[0].action, "41.7%")
	require.Empty(t, w.findings("life.payments.system_errors"), "no system errors yet")

	// Within the hour: system errors, then the PSP refusing our account.
	w.seedAttempts(c.id, nmi("initial", "system_error", "processing_error", "400", 2, w.clock.Now()))
	w.converge()
	errs := w.findings("life.payments.system_errors")
	require.Len(t, errs, 1, "2 of 62 attempts in the hour")
	require.Equal(t, "high", errs[0].severity)
	w.seedAttempts(c.id, nmi("initial", "system_error", "merchant_configuration_error", "410", 1, w.clock.Now()))
	w.converge()
	errs = w.findings("life.payments.system_errors")
	require.Len(t, errs, 1)
	require.Equal(t, "critical", errs[0].severity, "our credentials are refused")

	w.seedAttempts(c.id, nmi("initial", "unknown", "unknown", "999", 1, w.clock.Now()))
	w.converge()
	unmapped := w.findings("life.decline.unmapped")
	require.Len(t, unmapped, 1)
	require.Equal(t, "decline:nmi:999", unmapped[0].subject)
}

// Payment attempts and rebill cycles are kept 25 months; the cleanup worker
// removes older ones, and a cycle only once its attempts are gone.
func TestPaymentAttemptRetention(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	now := w.clock.Now()
	old := now.AddDate(0, -26, 0)
	seed := attemptSeed{psp: w.psp["nmi"], rail: "nmi", owner: "engine", category: "approved", cycles: true}
	seed.at, seed.n = old, 3
	w.seedAttempts(c.id, seed)
	seed.at, seed.n = now, 2
	w.seedAttempts(c.id, seed)
	// An old cycle whose retry is recent stays until that retry ages out.
	_, err := w.pool.Exec(t.Context(), w.q(`UPDATE openrails.payment_attempts SET attempted_at = $1
		WHERE id = (SELECT id FROM openrails.payment_attempts WHERE attempted_at < $2 ORDER BY attempted_at LIMIT 1)`), now, now.AddDate(0, -25, 0))
	require.NoError(t, err)

	res, err := w.jobs.Insert(t.Context(), cleanupPass{}, &river.InsertOpts{Queue: embed.QueueBilling})
	require.NoError(t, err)
	w.waitJob(res.Job.ID)
	count := func(table string) int {
		var n int
		require.NoError(t, w.pool.QueryRow(t.Context(), w.q(fmt.Sprintf(`SELECT count(*) FROM openrails.%s`, table))).Scan(&n))
		return n
	}
	require.Equal(t, 3, count("payment_attempts"), "the two recent attempts and the recent retry")
	require.Equal(t, 3, count("rebill_cycles"), "the recent cycles and the old one its retry keeps")
}

type cleanupPass struct{}

func (cleanupPass) Kind() string { return "openrails.cleanup_expired_data" }

// A PSP whose provider charges normally arrive by webhook goes a day with
// none while pulls still find its charges: one finding for that PSP, none for
// the one still delivering or the one that never did; it resolves when a
// webhook arrives again (#1112).
func TestWebhookSilenceAlerts(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	now := w.clock.Now()
	charge := func(psp, via string, n int, at time.Time) attemptSeed {
		return attemptSeed{psp: psp, rail: "nmi", kind: "rebill", owner: "nmi_schedule", cardEntry: "saved", category: "approved", n: n, at: at, via: via, cycles: true}
	}
	earlier := now.Add(-3 * 24 * time.Hour)
	w.seedAttempts(c.id, charge(w.psp["nmi"], "webhook", 20, earlier))
	w.seedAttempts(c.id, charge(w.psp["nmi"], "pull", 4, now))
	w.seedAttempts(c.id, charge(w.psp["stripe"], "webhook", 20, earlier))
	w.seedAttempts(c.id, charge(w.psp["stripe"], "webhook", 4, now))
	w.seedAttempts(c.id, charge(w.psp["ccbill"], "pull", 20, earlier))
	w.seedAttempts(c.id, charge(w.psp["ccbill"], "pull", 4, now))
	w.converge()
	silent := w.findings("life.webhooks.silent")
	require.Len(t, silent, 1)
	require.Equal(t, "psp:greenfield-nmi", silent[0].subject)
	require.Equal(t, "high", silent[0].severity)
	require.Contains(t, silent[0].action, "pulls found 4")

	w.advance(time.Hour)
	w.seedAttempts(c.id, charge(w.psp["nmi"], "webhook", 1, w.clock.Now()))
	w.converge()
	require.Empty(t, w.findings("life.webhooks.silent"), "webhooks resumed")
}
