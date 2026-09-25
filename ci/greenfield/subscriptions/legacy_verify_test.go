//go:build greenfield && integration

package subscriptions_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// rowStates is every NMI subscription's status and period end, by schedule.
func (w *world) rowStates() map[string]struct {
	status string
	end    time.Time
} {
	w.t.Helper()
	rows, err := w.pool.Query(w.t.Context(), `SELECT rail_subscription_id, status::text, current_period_ends_at FROM `+pgx.Identifier{w.schema}.Sanitize()+`.subscriptions WHERE rail = 'nmi'`)
	require.NoError(w.t, err)
	defer rows.Close()
	out := map[string]struct {
		status string
		end    time.Time
	}{}
	for rows.Next() {
		var id, status string
		var end *time.Time
		require.NoError(w.t, rows.Scan(&id, &status, &end))
		e := out[id]
		e.status = status
		if end != nil {
			e.end = end.UTC()
		}
		out[id] = e
	}
	require.NoError(w.t, rows.Err())
	return out
}

// verificationReads is the NMI reads made to verify rows: the recurring
// report, transaction queries and subscription reads.
func verificationReads(before, after map[string]int) map[string]int {
	out := map[string]int{}
	for _, kind := range []string{"query:recurring", "query:transaction", "v5:subscriptions", "v5:subscriptions/{id}"} {
		out[kind] = after[kind] - before[kind]
	}
	return out
}

// A legacy book of thousands of NMI members whose paid periods lapsed before
// the export lands unverified: NMI has since renewed most, ended some and
// billed nothing for a few. The import's commit wakes the verifier, which
// reads the account in bulk — one roster read and a few transaction pages,
// not one read per member — and resolves every row from NMI's own records.
// OpenRails charges nothing and access holds throughout; the few NMI never
// billed stay unverified, visible in the backlog finding.
func TestLegacyNMIImportVerifiesInBulk(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	tier := w.bookTier("monthly", 999, 30)
	const renewedN, goneN, silentN = 1960, 20, 20
	paid := w.clock.Now().Add(-2 * day)
	b := w.newLegacyBook()
	var renewed, gone, silent []*bookRow
	for i := range renewedN + goneN + silentN {
		r := b.add(&bookRow{source: fmt.Sprintf("book-%04d", i), tier: tier, paid: paid, declared: true})
		switch {
		case i < renewedN:
			w.nmi.providerRenew(r.schedule, true)
			renewed = append(renewed, r)
		case i < renewedN+goneN:
			w.nmi.providerCancel(r.schedule)
			gone = append(gone, r)
		default:
			silent = append(silent, r)
		}
	}
	reads, writes := w.nmi.readCounts(), len(w.nmiWrites())
	result, err := w.client[embedded].ImportBilling(t.Context(), b.book)
	require.NoError(t, err)
	require.Len(t, result.Imported, renewedN+goneN+silentN, "%+v", result.Reasons)

	require.Eventually(t, func() bool {
		states := w.rowStates()
		for _, r := range renewed {
			if states[r.schedule].status != "active" {
				return false
			}
		}
		for _, r := range gone {
			if states[r.schedule].status != "cancelled" {
				return false
			}
		}
		return true
	}, 3*time.Minute, 200*time.Millisecond, "the import is verified")

	got := verificationReads(reads, w.nmi.readCounts())
	t.Logf("NMI reads to verify %d imported members: %v", renewedN+goneN+silentN, got)
	require.Equal(t, 1, got["v5:subscriptions"], "one roster read")
	require.LessOrEqual(t, got["query:transaction"], 6, "a few transaction pages")
	require.LessOrEqual(t, got["query:recurring"]+got["v5:subscriptions/{id}"], 2, "no per-member reads")

	states := w.rowStates()
	for _, r := range renewed {
		next := w.nmi.scheduleState(r.schedule).NextBilling.UTC().Truncate(24 * time.Hour)
		require.True(t, next.Equal(states[r.schedule].end), "%s: renewed through NMI's next billing date (%s vs %s)", r.source, next, states[r.schedule].end)
	}
	for _, r := range silent {
		require.Equal(t, "unverified", states[r.schedule].status, "%s: nothing billed, nothing decided", r.source)
	}
	for _, r := range append(renewed, silent...) {
		require.True(t, r.c.entitled(tier.ent), "%s: access holds", r.source)
	}
	require.Zero(t, w.nmi.saleAttempts(), "OpenRails charges nobody")
	require.Len(t, w.nmiWrites(), writes, "verification only reads")

	w.converge()
	require.Len(t, w.openFindings("life.unverified.backlog"), 1, "the backlog of the account")
	var count int
	require.NoError(t, w.pool.QueryRow(t.Context(), `SELECT (evidence->'local'->>'count')::int FROM `+pgx.Identifier{w.schema}.Sanitize()+`.reconciliation_findings
		WHERE finding_type = 'life.unverified.backlog'`).Scan(&count))
	require.Equal(t, silentN, count)
}

// A lapsed NMI member parked unverified by a webhook's convergence is read
// at once, not at the next pass: the read runs as soon as the park commits
// and finds the renewal NMI charged meanwhile. A later lapse NMI never bills
// stays unverified with access held, and escalates to the operator.
func TestUnverifiedAfterWebhookIsReadAtOnce(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	tier := w.bookTier("monthly", 999, 30)
	l := w.mirrorBook(embedded, tier, 1)[0]
	end := *w.subscription(embedded, l.sub).CurrentPeriodEndsAt
	w.advance(end.Sub(w.clock.Now()) + 3*day)

	read := w.nmi.hold(newGate(func(r *http.Request) bool {
		form, err := url.ParseQuery(readBody(r))
		return err == nil && form.Get("report_type") == "recurring"
	}, false))
	require.Equal(t, http.StatusOK, w.deliver("nmi", l.staleNotice()))
	w.settle()
	select {
	case <-read.arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("the unverified row was not read")
	}
	require.Equal(t, "unverified", w.subscription(embedded, l.sub).Status, "no charge was visible: parked")
	require.True(t, l.c.entitled(l.ent), "access holds while unverified")
	l.providerRenewal(true) // NMI charges while the read is in flight
	close(read.release)
	require.Eventually(t, func() bool { return w.subscription(embedded, l.sub).Status == "active" }, 30*time.Second, 50*time.Millisecond)
	l.requireMirrored("read at once")

	// The next lapse NMI never bills: the row is read, stays unverified and
	// is escalated once it has been unverified for three days.
	w.nmi.unhold()
	next := *w.subscription(embedded, l.sub).CurrentPeriodEndsAt
	w.advance(next.Sub(w.clock.Now()) + 3*day)
	w.converge()
	require.Eventually(t, func() bool { return w.subscription(embedded, l.sub).Status == "unverified" }, 30*time.Second, 50*time.Millisecond)
	w.converge()
	backlog := w.openFindings("life.unverified.backlog")
	require.Len(t, backlog, 1)
	require.Contains(t, backlog[0], "psp:")
	require.Empty(t, w.openFindings("life.unverified.unresolved"))
	w.advance(4 * day)
	w.converge()
	require.Equal(t, []string{"subscription:" + l.sub.UUID().String()}, w.openFindings("life.unverified.unresolved"))
	require.Equal(t, "unverified", w.subscription(embedded, l.sub).Status)
	require.True(t, l.c.entitled(l.ent), "uncertainty never revokes access")
	require.Zero(t, w.nmi.saleAttempts())
}

// readBody reads a held request's form body and leaves it readable.
func readBody(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	raw, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(raw))
	return string(raw)
}
