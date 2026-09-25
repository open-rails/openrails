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
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
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

// A legacy book of 1,000 NMI schedules whose paid periods lapsed before the
// export lands unverified: NMI has since renewed most, ended some and
// billed nothing for a few. Each import commit wakes the verifier, which
// reads the account in bulk — a roster read and a few transaction pages, not
// one read per member — and resolves every row from NMI's own records.
// OpenRails charges nothing and access holds throughout; the few NMI never
// billed stay unverified, visible in the backlog finding.
func TestLegacyNMIImportVerifiesInBulk(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	// 50 members holding 20 memberships each: 1,000 NMI schedules.
	const members, tiers = 50, 20
	var book []bookTier
	for i := range tiers {
		book = append(book, w.bookTier(fmt.Sprintf("tier%02d", i), 999, 30))
	}
	paid := w.clock.Now().Add(-2 * day)
	b := w.newLegacyBook()
	var renewed, gone, silent []*bookRow
	for range members {
		c := w.newCustomer()
		for _, tier := range book {
			r := b.add(&bookRow{source: fmt.Sprintf("book-%04d", len(b.rows)), tier: tier, c: c, paid: paid, declared: true})
			switch len(b.rows) % 100 {
			case 1:
				w.nmi.providerCancel(r.schedule)
				gone = append(gone, r)
			case 2:
				silent = append(silent, r)
			default:
				w.nmi.providerRenew(r.schedule, true)
				renewed = append(renewed, r)
			}
		}
	}
	w.refreshIdle() // the startup pull's reads are not the verifier's
	reads, writes := w.nmi.readCounts(), len(w.nmiWrites())
	// A legacy importer sends its book in request-sized batches; each commit
	// wakes the verifier.
	const batch = 250
	started := time.Now()
	for i := 0; i < len(b.book.Subscriptions); i += batch {
		j := min(i+batch, len(b.book.Subscriptions))
		part := b.book
		part.PaymentMethods, part.Customers = b.book.PaymentMethods[i:j], b.book.Customers[i:j]
		part.Subscriptions, part.Transactions = b.book.Subscriptions[i:j], b.book.Transactions[i:j]
		result, err := w.client[embedded].ImportBilling(t.Context(), part)
		require.NoError(t, err)
		require.Len(t, result.Imported, j-i, "%+v", result.Reasons)
	}
	batches := (len(b.book.Subscriptions) + batch - 1) / batch
	t.Logf("imported in %s", time.Since(started))

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

	t.Logf("verified %s after the first import", time.Since(started))
	got := verificationReads(reads, w.nmi.readCounts())
	t.Logf("NMI reads to verify %d imported schedules: %v", len(b.rows), got)
	require.LessOrEqual(t, got["v5:subscriptions"], batches, "at most one roster read per imported batch")
	require.LessOrEqual(t, got["query:transaction"], batches*5, "a few transaction pages per bulk read")
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
		require.True(t, r.c.entitled(r.tier.ent), "%s: access holds", r.source)
	}
	require.Zero(t, w.nmi.saleAttempts(), "OpenRails charges nobody")
	require.Len(t, w.nmiWrites(), writes, "verification only reads")

	w.converge()
	require.Len(t, w.openFindings("life.unverified.backlog"), 1, "the backlog of the account")
	var count int
	require.NoError(t, w.pool.QueryRow(t.Context(), `SELECT (evidence->'local'->>'count')::int FROM `+pgx.Identifier{w.schema}.Sanitize()+`.reconciliation_findings
		WHERE finding_type = 'life.unverified.backlog'`).Scan(&count))
	require.Equal(t, len(silent), count)
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

// refreshIdle waits for the startup provider refresh to finish, so its
// reads are not counted as the verifier's.
func (w *world) refreshIdle() {
	w.t.Helper()
	kinds := []string{"openrails.provider_refresh", "openrails.provider_refresh_merchant"}
	require.Eventually(w.t, func() bool {
		busy, err := w.jobs.JobList(w.t.Context(), river.NewJobListParams().Kinds(kinds...).
			States(rivertype.JobStateAvailable, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStatePending, rivertype.JobStateScheduled).First(10))
		if err != nil || len(busy.Jobs) > 0 {
			return false
		}
		done, err := w.jobs.JobList(w.t.Context(), river.NewJobListParams().Kinds(kinds[1]).States(rivertype.JobStateCompleted).First(1))
		return err == nil && len(done.Jobs) > 0
	}, time.Minute, 50*time.Millisecond, "the startup provider refresh")
}
