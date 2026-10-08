//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/stretchr/testify/require"
)

type rosterForUndatedProbe struct{ snapshot *reconcile.RemoteSnapshot }

func (f rosterForUndatedProbe) Name() string                         { return "nmi" }
func (f rosterForUndatedProbe) Capabilities() reconcile.Capabilities { return f.snapshot.Capabilities }
func (f rosterForUndatedProbe) Fetch(context.Context, reconcile.FetchParams) (*reconcile.RemoteSnapshot, error) {
	return f.snapshot, nil
}

func TestUndatedNMIProbeCannotAdoptRosterPeriod(t *testing.T) {
	w := newWorld(t)
	require.NoError(t, w.jobs.Stop(t.Context()))
	rt := engine.Graph(w.rt).Runtime
	rt.Verifier.Close() // isolate this ordinary cohort pass from the import listener
	tier := w.bookTier("undated", 999, 30)
	paid := w.clock.Now().Add(-2 * day)
	book := w.newLegacyBook()
	row := book.add(&bookRow{source: "undated", tier: tier, c: w.newCustomer(), paid: paid, declared: true})
	result, err := w.client[embedded].ImportBilling(t.Context(), book.book)
	require.NoError(t, err)
	require.Len(t, result.Imported, 1)
	sub := row.sub(w, embedded)
	before := w.rowStates()[row.schedule]
	require.Equal(t, "unverified", before.status)
	mid := w.client[embedded].MerchantID()
	ctx := merchant.WithID(t.Context(), mid)
	psp := w.psp["nmi"].UUID()
	client, ok, err := rt.CollectionResolver.ResolveNMIClient(ctx, mid.UUID(), &psp)
	require.NoError(t, err)
	require.True(t, ok)
	w.nmi.Intercept(func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/query.php") }, func(r *http.Request, _ func() *http.Response) (*http.Response, error) {
		body := fmt.Sprintf(`<nm_response><transaction><transaction_id>undated-sale</transaction_id><order_id>%s</order_id><currency>USD</currency><action><amount>9.99</amount><action_type>sale</action_type><success>1</success><date>unreadable</date><response_code>100</response_code></action></transaction></nm_response>`, sub.ID.UUID())
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	t.Cleanup(w.nmi.ClearIntercepts)
	future := w.clock.Now().Add(28 * day)
	snapshot := &reconcile.RemoteSnapshot{Provider: reconcile.ProviderNMI, FetchedAt: w.clock.Now(), Capabilities: reconcile.Capabilities{Subscriptions: true}, Coverage: reconcile.SnapshotCoverage{SubscriptionsExhaustive: true}, Subscriptions: []reconcile.RemoteSubscription{{RailSubscriptionID: row.schedule, Status: reconcile.SubscriptionStatusActive, NextBillingAt: &future}}}
	prober := &reconcile.NMISubscriptionProber{Client: client}
	_, err = prober.ProbeSubscription(ctx, reconcile.ProbeSubject{LocalID: sub.ID.UUID(), RailSubscriptionID: row.schedule, PeriodEnd: &before.end, ObservedAt: w.clock.Now()})
	require.ErrorContains(t, err, "readable action time")
	lifecycle := subscriptions.NewSubscriptionLifecycleService(rt.DB, nil, nil, nil, nil, nil, w.clock)
	lifecycle.SetConfig(rt.Config)
	var report reconcile.UnknownReconcileResult
	err = rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var err error
		report, err = reconcile.ReconcileUnknownCohort(ctx, rt.DB, lifecycle, map[reconcile.Provider]reconcile.RailFetcher{reconcile.ProviderNMI: rosterForUndatedProbe{snapshot}}, map[reconcile.Provider]reconcile.SubscriptionProber{reconcile.ProviderNMI: prober}, mid, w.clock.Now(), reconcile.UnknownReconcileOptions{})
		return err
	})
	require.NoError(t, err)
	require.Equal(t, 1, report.Probed)
	require.Equal(t, 1, report.StillUnknown)
	after := w.rowStates()[row.schedule]
	require.Equal(t, before.status, after.status)
	require.True(t, before.end.Equal(after.end), "a future roster date is not a verified paid period")
	require.Empty(t, w.nmi.Attempts())
}
