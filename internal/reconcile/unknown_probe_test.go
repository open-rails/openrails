package reconcile

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #665: probe-source → snapshot mapping. The per-subscription probers are pure
// snapshot SOURCES: these tests assert the snapshot fields verbatim, then feed
// each snapshot to ResolveUnknownFromSnapshot to pin the end-to-end verdicts the
// retired #367 liveness worker used to decide itself.

// nmiProbeFixture fakes the NMI HTTP surface: query.php transaction reports,
// the v5 GET (subscription roster), and a direct-post counter that must stay
// zero — probes are reads, never mutations.
type nmiProbeFixture struct {
	prober           *NMISubscriptionProber
	directPostHits   *atomic.Int64
	transactionXML   string
	subscriptionJSON string // "" = 404 (gone at NMI)
	queryStatus      int    // != 0 forces an HTTP error
}

func newNMIProbeFixture(t *testing.T) *nmiProbeFixture {
	t.Helper()
	f := &nmiProbeFixture{
		directPostHits: &atomic.Int64{},
		transactionXML: `<?xml version="1.0"?><nm_response></nm_response>`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet { // v5 GET /subscriptions/{id}
			if f.queryStatus != 0 {
				w.WriteHeader(f.queryStatus)
				return
			}
			if f.subscriptionJSON == "" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"type":"notFound","error_code":"E_NOT_FOUND","message":"subscription not found"}`))
				return
			}
			_, _ = w.Write([]byte(f.subscriptionJSON))
			return
		}
		_ = r.ParseForm()
		if r.Form.Get("report_type") == "" { // Direct Post (mutations)
			f.directPostHits.Add(1)
			_, _ = w.Write([]byte("response=1"))
			return
		}
		if f.queryStatus != 0 {
			w.WriteHeader(f.queryStatus)
			return
		}
		_, _ = w.Write([]byte(f.transactionXML))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		assert.Zero(t, f.directPostHits.Load(), "probes must never send a provider mutation")
	})

	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{SecurityKey: "k", WebhookSecret: "s"}, true)
	require.NoError(t, err)
	client.DirectPostURL = srv.URL
	client.QueryURL = srv.URL
	client.V5BaseURL = srv.URL
	f.prober = &NMISubscriptionProber{Client: client}
	return f
}

func nmiSaleXML(txnID, orderID, actionType, success string, at time.Time, amount string) string {
	return fmt.Sprintf(`<?xml version="1.0"?>
<nm_response>
  <transaction>
    <transaction_id>%s</transaction_id>
    <order_id>%s</order_id>
    <currency>usd</currency>
    <action><amount>%s</amount><action_type>%s</action_type><success>%s</success><date>%s</date><response_code>202</response_code><response_text>Insufficient funds</response_text></action>
  </transaction>
</nm_response>`, txnID, orderID, amount, actionType, success, at.UTC().Format("20060102150405"))
}

func TestNMISubscriptionProber_ChargedPeriodResolvesRenewed(t *testing.T) {
	f := newNMIProbeFixture(t)
	now := time.Now().UTC()
	periodEnd := now.Add(-3 * 24 * time.Hour)
	localID := uuid.New()
	railSub := "psub_1"
	chargedAt := periodEnd.Add(2 * time.Hour)
	f.transactionXML = nmiSaleXML("txn_ok", localID.String(), "sale", "1", chargedAt, "9.99")
	nextCharge := now.Add(27 * 24 * time.Hour).Format("2006-01-02")
	f.subscriptionJSON = fmt.Sprintf(`{"object":"subscription","id":"%s","next_billing_date":"%s"}`, railSub, nextCharge)

	snap, err := f.prober.ProbeSubscription(context.Background(), ProbeSubject{LocalID: localID, RailSubscriptionID: railSub, PeriodEnd: &periodEnd})
	require.NoError(t, err)

	require.Len(t, snap.Transactions, 1)
	txn := snap.Transactions[0]
	assert.Equal(t, "txn_ok", txn.TransactionID)
	assert.Equal(t, railSub, txn.SubscriptionID)
	assert.True(t, txn.Success)
	assert.Equal(t, int64(999), txn.AmountCents)
	assert.Equal(t, "usd", txn.Currency)
	assert.True(t, txn.OccurredAt.Equal(chargedAt.Truncate(time.Second)), "verbatim action date")
	require.Len(t, snap.Subscriptions, 1)
	assert.Equal(t, SubscriptionStatusActive, snap.Subscriptions[0].Status)
	assert.True(t, snap.Coverage.SubscriptionsExhaustive, "the per-sub v5 GET is authoritative for this subject")

	v := decideUnknown(railSub, &periodEnd, snap, now, 14*24*time.Hour)
	assert.Equal(t, TransitionRenew, v.Kind)
	require.Len(t, v.Backfill, 1, "the verified charge must be backfilled")
}

func TestNMISubscriptionProber_DeclineWithStalledRemoteResolvesPastDue(t *testing.T) {
	f := newNMIProbeFixture(t)
	now := time.Now().UTC()
	periodEnd := now.Add(-2 * 24 * time.Hour)
	localID := uuid.New()
	railSub := "psub_2"
	f.transactionXML = nmiSaleXML("txn_decl", localID.String(), "sale", "0", periodEnd.Add(time.Hour), "9.99")
	// Remote record stalled: next charge date wedged in the past.
	f.subscriptionJSON = fmt.Sprintf(`{"object":"subscription","id":"%s","next_billing_date":"%s"}`, railSub, periodEnd.Format("2006-01-02"))

	snap, err := f.prober.ProbeSubscription(context.Background(), ProbeSubject{LocalID: localID, RailSubscriptionID: railSub, PeriodEnd: &periodEnd})
	require.NoError(t, err)

	require.Len(t, snap.Transactions, 1)
	assert.Equal(t, TransactionTypeDecline, snap.Transactions[0].Type)
	assert.Equal(t, "txn_decl", snap.Transactions[0].TransactionID, "decline id carried for failed-payment backfill")
	assert.Equal(t, "Insufficient funds", snap.Transactions[0].DeclineReason)
	require.Len(t, snap.Subscriptions, 1)
	assert.Equal(t, SubscriptionStatusPastDue, snap.Subscriptions[0].Status, "stalled next charge = past_due (bulk fetcher convention)")

	v := decideUnknown(railSub, &periodEnd, snap, now, 14*24*time.Hour)
	assert.Equal(t, TransitionPastDue, v.Kind, "verified decline within the window routes into dunning")
	require.Len(t, v.Backfill, 1, "the decline is backfilled as a failed payment")
}

func TestNMISubscriptionProber_RemoteGoneResolvesCancelled(t *testing.T) {
	f := newNMIProbeFixture(t)
	now := time.Now().UTC()
	periodEnd := now.Add(-5 * 24 * time.Hour)
	railSub := "psub_3"
	// No charges, v5 GET 404s: gone at NMI IS terminal.

	snap, err := f.prober.ProbeSubscription(context.Background(), ProbeSubject{LocalID: uuid.New(), RailSubscriptionID: railSub, PeriodEnd: &periodEnd})
	require.NoError(t, err)
	assert.Empty(t, snap.Subscriptions)
	assert.True(t, snap.Coverage.SubscriptionsExhaustive)

	v := decideUnknown(railSub, &periodEnd, snap, now, 14*24*time.Hour)
	assert.Equal(t, TransitionCancel, v.Kind)
	assert.True(t, v.RemoteGone, "roster-gone cancel must NOT queue a delete intent")
}

func TestNMISubscriptionProber_MisalignmentResolvesAdopted(t *testing.T) {
	f := newNMIProbeFixture(t)
	now := time.Now().UTC()
	periodEnd := now.Add(-36 * time.Hour)
	railSub := "psub_4"
	nextCharge := now.Add(48 * time.Hour)
	f.subscriptionJSON = fmt.Sprintf(`{"object":"subscription","id":"%s","next_billing_date":"%s"}`, railSub, nextCharge.Format("2006-01-02"))

	snap, err := f.prober.ProbeSubscription(context.Background(), ProbeSubject{LocalID: uuid.New(), RailSubscriptionID: railSub, PeriodEnd: &periodEnd})
	require.NoError(t, err)

	v := decideUnknown(railSub, &periodEnd, snap, now, 14*24*time.Hour)
	assert.Equal(t, TransitionAdoptPeriodEnd, v.Kind, "alive remote with future billing and no charge adopts the provider clock")
	require.NotNil(t, v.NewPeriodEnd)
	wantEnd, err := time.ParseInLocation("2006-01-02", nextCharge.Format("2006-01-02"), time.UTC)
	require.NoError(t, err)
	assert.True(t, v.NewPeriodEnd.Equal(wantEnd))
}

// NULL period end (legacy import): the charge probe is skipped — there is no
// period boundary to ask about — and the recurring roster alone answers.
func TestNMISubscriptionProber_NilPeriodEndSkipsChargeProbe(t *testing.T) {
	f := newNMIProbeFixture(t)
	now := time.Now().UTC()
	railSub := "psub_5"
	f.transactionXML = `PARSE ERROR IF FETCHED` // proves query.php is never hit
	f.subscriptionJSON = fmt.Sprintf(`{"object":"subscription","id":"%s","next_billing_date":"%s"}`, railSub, now.Add(72*time.Hour).Format("2006-01-02"))

	snap, err := f.prober.ProbeSubscription(context.Background(), ProbeSubject{LocalID: uuid.New(), RailSubscriptionID: railSub, PeriodEnd: nil})
	require.NoError(t, err)
	assert.Empty(t, snap.Transactions)
	assert.False(t, snap.Capabilities.Transactions)

	// The NULL-period legacy row: the decider defaults the boundary to now.
	v := decideUnknown(railSub, nil, snap, now, 14*24*time.Hour)
	assert.Equal(t, TransitionAdoptPeriodEnd, v.Kind, "missing-period legacy row adopts the provider next billing date")
}

func TestNMISubscriptionProber_ProviderErrorPropagates(t *testing.T) {
	f := newNMIProbeFixture(t)
	f.queryStatus = http.StatusInternalServerError
	periodEnd := time.Now().UTC().Add(-2 * 24 * time.Hour)
	_, err := f.prober.ProbeSubscription(context.Background(), ProbeSubject{LocalID: uuid.New(), RailSubscriptionID: "psub_6", PeriodEnd: &periodEnd})
	require.Error(t, err, "unreachable provider must propagate — the row stays unknown and retries")
}

// fakeStripeLivenessProber returns a canned record.
type fakeStripeLivenessProber struct {
	rec subscriptions.StripeLivenessRecord
	err error
}

func (p *fakeStripeLivenessProber) ProbeSubscription(ctx context.Context, id string) (subscriptions.StripeLivenessRecord, error) {
	return p.rec, p.err
}

func TestStripeSubscriptionProber_Verdicts(t *testing.T) {
	now := time.Now().UTC()
	periodEnd := now.Add(-3 * 24 * time.Hour)
	railSub := "sub_stripe_1"
	remoteStart := periodEnd // Stripe advanced: new period opened at the old end
	remoteEnd := periodEnd.Add(30 * 24 * time.Hour)

	cases := []struct {
		name     string
		rec      subscriptions.StripeLivenessRecord
		wantKind TransitionKind
		wantGone bool
	}{
		{
			name: "paid invoice advanced the period → renewed",
			rec: subscriptions.StripeLivenessRecord{
				Found: true, Status: "active",
				CurrentPeriodStart: remoteStart, CurrentPeriodEnd: remoteEnd,
				LatestInvoicePaid: true, LatestInvoiceTransactionID: "ch_1",
				LatestInvoiceAmountPaid: 999, LatestInvoiceCurrency: "usd",
			},
			wantKind: TransitionRenew,
		},
		{
			name: "active, future period end, no paid invoice → adopted (no access from adoption)",
			rec: subscriptions.StripeLivenessRecord{
				Found: true, Status: "active",
				CurrentPeriodStart: periodEnd.Add(-30 * 24 * time.Hour), CurrentPeriodEnd: now.Add(10 * 24 * time.Hour),
			},
			wantKind: TransitionAdoptPeriodEnd,
		},
		{
			name:     "canceled at stripe → cancelled, remote gone",
			rec:      subscriptions.StripeLivenessRecord{Found: true, Status: "canceled"},
			wantKind: TransitionCancel,
			wantGone: true,
		},
		{
			name:     "not found (404 is an answer) → cancelled, remote gone",
			rec:      subscriptions.StripeLivenessRecord{Found: false},
			wantKind: TransitionCancel,
			wantGone: true,
		},
		{
			name: "past_due at stripe within window → past_due (stripe owns retries)",
			rec: subscriptions.StripeLivenessRecord{
				Found: true, Status: "past_due",
				CurrentPeriodStart: periodEnd.Add(-30 * 24 * time.Hour), CurrentPeriodEnd: periodEnd,
			},
			wantKind: TransitionPastDue,
		},
		{
			name: "active but remote period also stale → unreachable (leave for next pass)",
			rec: subscriptions.StripeLivenessRecord{
				Found: true, Status: "active",
				CurrentPeriodStart: periodEnd.Add(-30 * 24 * time.Hour), CurrentPeriodEnd: periodEnd,
			},
			wantKind: TransitionNone,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prober := &StripeSubscriptionProber{Prober: &fakeStripeLivenessProber{rec: c.rec}}
			snap, err := prober.ProbeSubscription(context.Background(), ProbeSubject{LocalID: uuid.New(), RailSubscriptionID: railSub, PeriodEnd: &periodEnd})
			require.NoError(t, err)
			v := decideUnknown(railSub, &periodEnd, snap, now, 14*24*time.Hour)
			assert.Equal(t, c.wantKind, v.Kind)
			assert.Equal(t, c.wantGone, v.RemoteGone)
			if c.wantKind == TransitionRenew {
				require.Len(t, v.Backfill, 1, "the paid invoice charge must be backfilled")
				assert.Equal(t, "ch_1", v.Backfill[0].TransactionID)
				assert.True(t, v.NewPeriodEnd.Equal(remoteEnd), "remote period end travels with the renewal")
			}
		})
	}
}

// decideUnknown feeds a probe snapshot to the ONE decider as an `unknown` row
// (the #633 resolution path).
func decideUnknown(railSub string, periodEnd *time.Time, snap *RemoteSnapshot, now time.Time, window time.Duration) Decision {
	return Decide(SubscriptionState{Status: "unknown", RailSubscriptionID: railSub, PeriodEnd: periodEnd},
		EvidenceBundle{Snapshot: snap}, now, window)
}

// ccbillProbeFixture fakes the DataLink SMS HTTP surface. The mutation guard
// asserts probes only ever send action=viewSubscriptionStatus.
type ccbillProbeFixture struct {
	prober      *CCBillSubscriptionProber
	answer      *atomic.Value // string response body
	httpStatus  *atomic.Int64 // 0 = 200
	cancelHits  *atomic.Int64
	lastSubject *atomic.Value // string subscriptionId
}

func newCCBillProbeFixture(t *testing.T) *ccbillProbeFixture {
	t.Helper()
	f := &ccbillProbeFixture{
		answer: &atomic.Value{}, httpStatus: &atomic.Int64{},
		cancelHits: &atomic.Int64{}, lastSubject: &atomic.Value{},
	}
	f.answer.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		if r.PostForm.Get("action") != "viewSubscriptionStatus" {
			f.cancelHits.Add(1)
		}
		f.lastSubject.Store(r.PostForm.Get("subscriptionId"))
		if st := f.httpStatus.Load(); st != 0 {
			w.WriteHeader(int(st))
			return
		}
		_, _ = w.Write([]byte(f.answer.Load().(string)))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		assert.Zero(t, f.cancelHits.Load(), "probes must never send a provider mutation")
	})
	f.prober = &CCBillSubscriptionProber{Client: &ccbill.DataLinkClient{
		BaseURL: srv.URL, ClientAccNum: "900100", ClientSubAcc: "0000",
		Username: "u", Password: "p", HTTPClient: srv.Client(),
	}}
	return f
}

func ccbillStatusXML(status, expiry string) string {
	return fmt.Sprintf(`<results><subscriptionStatus>%s</subscriptionStatus><expirationDate>%s</expirationDate></results>`, status, expiry)
}

// #696: viewSubscriptionStatus snapshots + the ONE decider's verdicts.
func TestCCBillSubscriptionProber_Verdicts(t *testing.T) {
	now := time.Now().UTC()
	periodEnd := now.Add(-5 * 24 * time.Hour)
	railSub := "0123456789"
	futureExpiry := now.Add(25 * 24 * time.Hour)

	t.Run("recurring active with future expiry adopts the provider clock", func(t *testing.T) {
		f := newCCBillProbeFixture(t)
		f.answer.Store(ccbillStatusXML("2", futureExpiry.Format("20060102")))
		snap, err := f.prober.ProbeSubscription(context.Background(), ProbeSubject{LocalID: uuid.New(), RailSubscriptionID: railSub, PeriodEnd: &periodEnd})
		require.NoError(t, err)
		require.Len(t, snap.Subscriptions, 1)
		assert.Equal(t, SubscriptionStatusActive, snap.Subscriptions[0].Status)
		assert.Equal(t, "2", snap.Subscriptions[0].RawStatus)
		require.NotNil(t, snap.Subscriptions[0].NextBillingAt)
		assert.True(t, snap.Coverage.SubscriptionsExhaustive)
		assert.Equal(t, railSub, f.lastSubject.Load(), "probe targets the rail subscription id")

		v := decideUnknown(railSub, &periodEnd, snap, now, 14*24*time.Hour)
		assert.Equal(t, TransitionAdoptPeriodEnd, v.Kind)
	})

	t.Run("active non-recurring (status 1) resolves cancelled", func(t *testing.T) {
		f := newCCBillProbeFixture(t)
		f.answer.Store(ccbillStatusXML("1", futureExpiry.Format("20060102")))
		snap, err := f.prober.ProbeSubscription(context.Background(), ProbeSubject{LocalID: uuid.New(), RailSubscriptionID: railSub, PeriodEnd: &periodEnd})
		require.NoError(t, err)
		require.Len(t, snap.Subscriptions, 1)
		assert.Equal(t, SubscriptionStatusCancelled, snap.Subscriptions[0].Status)

		v := decideUnknown(railSub, &periodEnd, snap, now, 14*24*time.Hour)
		assert.Equal(t, TransitionCancel, v.Kind)
		assert.True(t, v.RemoteGone, "provider-declared dead: no delete intent needed")
	})

	t.Run("inactive (status 0) resolves cancelled", func(t *testing.T) {
		f := newCCBillProbeFixture(t)
		f.answer.Store(ccbillStatusXML("0", ""))
		snap, err := f.prober.ProbeSubscription(context.Background(), ProbeSubject{LocalID: uuid.New(), RailSubscriptionID: railSub, PeriodEnd: &periodEnd})
		require.NoError(t, err)
		require.Len(t, snap.Subscriptions, 1)
		assert.Equal(t, SubscriptionStatusExpired, snap.Subscriptions[0].Status)
		assert.Nil(t, snap.Subscriptions[0].NextBillingAt, "no expiry field => no fabricated boundary")

		v := decideUnknown(railSub, &periodEnd, snap, now, 14*24*time.Hour)
		assert.Equal(t, TransitionCancel, v.Kind)
		assert.True(t, v.RemoteGone)
	})

	t.Run("unrecognized status stays unknown (never a guessed transition)", func(t *testing.T) {
		f := newCCBillProbeFixture(t)
		f.answer.Store(ccbillStatusXML("7", ""))
		snap, err := f.prober.ProbeSubscription(context.Background(), ProbeSubject{LocalID: uuid.New(), RailSubscriptionID: railSub, PeriodEnd: &periodEnd})
		require.NoError(t, err)
		require.Len(t, snap.Subscriptions, 1)
		assert.Equal(t, SubscriptionStatusUnknown, snap.Subscriptions[0].Status)
		assert.Equal(t, "7", snap.Subscriptions[0].RawStatus, "verbatim status preserved")

		v := decideUnknown(railSub, &periodEnd, snap, now, 14*24*time.Hour)
		assert.Equal(t, TransitionNone, v.Kind)
	})

	t.Run("provider error propagates (row stays unknown, retried)", func(t *testing.T) {
		f := newCCBillProbeFixture(t)
		f.httpStatus.Store(http.StatusInternalServerError)
		_, err := f.prober.ProbeSubscription(context.Background(), ProbeSubject{LocalID: uuid.New(), RailSubscriptionID: railSub, PeriodEnd: &periodEnd})
		require.Error(t, err)
	})

	t.Run("error-code answer propagates (absence is never fabricated)", func(t *testing.T) {
		f := newCCBillProbeFixture(t)
		f.answer.Store(`<results>-3</results>`)
		_, err := f.prober.ProbeSubscription(context.Background(), ProbeSubject{LocalID: uuid.New(), RailSubscriptionID: railSub, PeriodEnd: &periodEnd})
		require.Error(t, err)
	})
}
