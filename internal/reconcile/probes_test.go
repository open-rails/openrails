package reconcile

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// Probes are snapshot SOURCES: each verdict below is the one decider's answer
// for an `unknown` row fed that snapshot (the #633 resolution path).
func decideUnknown(railSub string, periodEnd *time.Time, snap *RemoteSnapshot, now time.Time) Decision {
	return Decide(SubscriptionState{Status: "unverified", RailSubscriptionID: railSub, PeriodEnd: periodEnd}, EvidenceBundle{Snapshot: snap}, now, 0)
}

func TestNMISubscriptionProber(t *testing.T) {
	now := time.Date(2040, time.June, 12, 15, 30, 0, 0, time.UTC)
	periodEnd := now.Add(-3 * oneDay)
	localID := uuid.New()
	saleXML := func(success string, at time.Time) string {
		return fmt.Sprintf(`<?xml version="1.0"?><nm_response><transaction><transaction_id>txn_1</transaction_id><order_id>%s</order_id><currency>usd</currency>
<action><amount>9.99</amount><action_type>sale</action_type><success>%s</success><date>%s</date><response_code>202</response_code><response_text>Insufficient funds</response_text></action>
</transaction></nm_response>`, localID, success, at.UTC().Format("20060102150405"))
	}
	probe := func(t *testing.T, txnXML, subJSON string, status int) (*RemoteSnapshot, error) {
		mutations := 0
		client := newNMITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if status != 0 {
				w.WriteHeader(status)
				return
			}
			if r.Method == http.MethodGet {
				if subJSON == "" {
					w.WriteHeader(http.StatusNotFound)
					fmt.Fprint(w, `{"type":"notFound","error_code":"E_NOT_FOUND","message":"subscription not found"}`)
					return
				}
				fmt.Fprint(w, subJSON)
				return
			}
			_ = r.ParseForm()
			if r.Form.Get("report_type") == "" {
				mutations++
				fmt.Fprint(w, "response=1")
				return
			}
			fmt.Fprint(w, txnXML)
		}))
		t.Cleanup(func() { require.Zero(t, mutations, "probes never mutate the provider") })
		return (&NMISubscriptionProber{Client: client}).ProbeSubscription(context.Background(),
			ProbeSubject{LocalID: localID, RailSubscriptionID: "psub_1", PeriodEnd: &periodEnd, ObservedAt: now})
	}
	schedule := func(next time.Time) string {
		return fmt.Sprintf(`{"object":"subscription","id":"psub_1","next_billing_date":%q}`, next.Format("2006-01-02"))
	}
	empty := `<?xml version="1.0"?><nm_response></nm_response>`

	t.Run("a charged period renews and backfills the charge", func(t *testing.T) {
		chargedAt := periodEnd.Add(2 * time.Hour)
		snap, err := probe(t, saleXML("1", chargedAt), schedule(now.Add(27*oneDay)), 0)
		require.NoError(t, err)
		require.Equal(t, now, snap.FetchedAt, "evidence is dated by the owning reconcile clock")
		require.True(t, snap.Coverage.SubscriptionsExhaustive, "the per-subscription GET is authoritative for its subject")
		require.Len(t, snap.Transactions, 1)
		require.Equal(t, RemoteTransaction{TransactionID: "txn_1", SubscriptionID: "psub_1", Type: TransactionTypeSale, Success: true,
			AmountCents: 999, Currency: "usd", OccurredAt: chargedAt.Truncate(time.Second)}, snap.Transactions[0])
		require.Equal(t, SubscriptionStatusActive, snap.Subscriptions[0].Status)
		d := decideUnknown("psub_1", &periodEnd, snap, now)
		require.Equal(t, TransitionRenew, d.Kind)
		require.Len(t, d.Backfill, 1)
	})
	t.Run("a declined period on a wedged schedule dunning", func(t *testing.T) {
		snap, err := probe(t, saleXML("0", periodEnd.Add(time.Hour)), schedule(periodEnd), 0)
		require.NoError(t, err)
		require.Equal(t, SubscriptionStatusPastDue, snap.Subscriptions[0].Status)
		require.Equal(t, TransactionTypeDecline, snap.Transactions[0].Type)
		require.Equal(t, "202", snap.Transactions[0].DeclineCode)
		require.Equal(t, TransitionPastDue, decideUnknown("psub_1", &periodEnd, snap, now).Kind)
	})
	t.Run("a 404 is NMI's authoritative absence", func(t *testing.T) {
		snap, err := probe(t, empty, "", 0)
		require.NoError(t, err)
		require.Empty(t, snap.Subscriptions)
		d := decideUnknown("psub_1", &periodEnd, snap, now)
		require.Equal(t, TransitionCancel, d.Kind)
		require.True(t, d.RemoteGone)
	})
	t.Run("an unreachable provider propagates and the row stays unknown", func(t *testing.T) {
		_, err := probe(t, empty, "", http.StatusInternalServerError)
		require.Error(t, err)
	})
}

func TestStripeSnapshotFromLiveness(t *testing.T) {
	now := time.Date(2040, time.July, 13, 16, 30, 0, 0, time.UTC)
	periodEnd := now.Add(-3 * oneDay)
	remoteEnd := periodEnd.Add(30 * oneDay)
	earlier := periodEnd.Add(-30 * oneDay)
	cases := []struct {
		name  string
		rec   subscriptions.StripeLivenessRecord
		want  TransitionKind
		gone  bool
		txns  int
		check func(t *testing.T, snap *RemoteSnapshot, d Decision)
	}{
		{name: "paid invoice advancing the period renews", want: TransitionRenew, txns: 1,
			rec: subscriptions.StripeLivenessRecord{Found: true, Status: "active", CurrentPeriodStart: periodEnd, CurrentPeriodEnd: remoteEnd,
				LatestInvoicePaid: true, LatestInvoiceTransactionID: "ch_1", LatestInvoiceAmountPaid: 999, LatestInvoiceCurrency: "USD"},
			check: func(t *testing.T, _ *RemoteSnapshot, d Decision) {
				require.Equal(t, "ch_1", d.Backfill[0].TransactionID)
				require.True(t, d.NewPeriodEnd.Equal(remoteEnd))
				require.True(t, d.NewPeriodStart.Equal(periodEnd))
			}},
		{name: "a future period without a paid invoice only adopts", want: TransitionAdoptPeriodEnd,
			rec: subscriptions.StripeLivenessRecord{Found: true, Status: "active", CurrentPeriodStart: earlier, CurrentPeriodEnd: now.Add(10 * oneDay)}},
		{name: "canceled at Stripe is gone", want: TransitionCancel, gone: true,
			rec: subscriptions.StripeLivenessRecord{Found: true, Status: "canceled"}},
		{name: "a 404 is an answer", want: TransitionCancel, gone: true, rec: subscriptions.StripeLivenessRecord{Found: false}},
		{name: "past_due inside the window dunning", want: TransitionPastDue,
			rec: subscriptions.StripeLivenessRecord{Found: true, Status: "past_due", CurrentPeriodStart: earlier, CurrentPeriodEnd: periodEnd}},
		{name: "active with a stale remote period waits", want: TransitionNone,
			rec: subscriptions.StripeLivenessRecord{Found: true, Status: "active", CurrentPeriodStart: earlier, CurrentPeriodEnd: periodEnd}},
		{name: "a failed collection is dated decline evidence", want: TransitionPastDue, txns: 1,
			rec: subscriptions.StripeLivenessRecord{Found: true, Status: "past_due", CurrentPeriodStart: earlier, CurrentPeriodEnd: periodEnd,
				LatestInvoiceCollectionFailed: true, LatestInvoiceAmountDue: 999, LatestInvoiceTransactionID: "ch_x", LatestInvoiceCreated: periodEnd.Add(time.Hour)},
			check: func(t *testing.T, snap *RemoteSnapshot, _ Decision) {
				require.Equal(t, "failed:ch_x", snap.Transactions[0].TransactionID, "never collides with the eventual success")
				require.Equal(t, TransactionTypeDecline, snap.Transactions[0].Type)
			}},
		{name: "an undated failed collection is not evidence", want: TransitionPastDue,
			rec: subscriptions.StripeLivenessRecord{Found: true, Status: "past_due", CurrentPeriodStart: earlier, CurrentPeriodEnd: periodEnd,
				LatestInvoiceCollectionFailed: true, LatestInvoiceAmountDue: 999, LatestInvoiceTransactionID: "ch_x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			snap := StripeSnapshotFromLiveness("sub_1", c.rec, now)
			require.Equal(t, now, snap.FetchedAt)
			require.Len(t, snap.Transactions, c.txns)
			d := decideUnknown("sub_1", &periodEnd, snap, now)
			require.Equal(t, c.want, d.Kind, d.Reason)
			require.Equal(t, c.gone, d.RemoteGone)
			if c.check != nil {
				c.check(t, snap, d)
			}
		})
	}
}

// #696: viewSubscriptionStatus is the per-record read; probes only ever send it.
func TestCCBillSubscriptionProber(t *testing.T) {
	now := time.Date(2040, time.August, 14, 17, 30, 0, 0, time.UTC)
	periodEnd := now.Add(-5 * oneDay)
	future := now.Add(25 * oneDay).Format("20060102")
	statusXML := func(status, expiry string) string {
		return fmt.Sprintf(`<results><subscriptionStatus>%s</subscriptionStatus><expirationDate>%s</expirationDate></results>`, status, expiry)
	}
	cases := []struct {
		name     string
		body     string
		status   int
		wantErr  bool
		remote   SubscriptionStatus
		noExpiry bool
		want     TransitionKind
		gone     bool
	}{
		{name: "recurring active adopts the provider clock", body: statusXML("2", future), remote: SubscriptionStatusActive, want: TransitionAdoptPeriodEnd},
		{name: "active non-recurring is cancelled", body: statusXML("1", future), remote: SubscriptionStatusCancelled, want: TransitionCancel, gone: true},
		{name: "inactive is expired without a fabricated boundary", body: statusXML("0", ""), remote: SubscriptionStatusExpired, noExpiry: true, want: TransitionCancel, gone: true},
		{name: "an unrecognized status is never guessed", body: statusXML("7", ""), remote: SubscriptionStatusUnknown, noExpiry: true, want: TransitionNone},
		{name: "a provider error propagates", status: http.StatusInternalServerError, wantErr: true},
		{name: "an error-code answer propagates", body: `<results>-3</results>`, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var subject string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, r.ParseForm())
				if r.PostForm.Get("action") != "viewSubscriptionStatus" {
					t.Errorf("probe sent a mutation: %v", r.PostForm)
				}
				subject = r.PostForm.Get("subscriptionId")
				if c.status != 0 {
					w.WriteHeader(c.status)
					return
				}
				fmt.Fprint(w, c.body)
			}))
			t.Cleanup(srv.Close)
			prober := &CCBillSubscriptionProber{Client: &ccbill.DataLinkClient{BaseURL: srv.URL, ClientAccNum: "900100", ClientSubAcc: "0000",
				Username: "u", Password: "p", HTTPClient: srv.Client()}}

			snap, err := prober.ProbeSubscription(context.Background(), ProbeSubject{LocalID: uuid.New(), RailSubscriptionID: "0123456789", PeriodEnd: &periodEnd, ObservedAt: now})
			if c.wantErr {
				require.Error(t, err, "absence is never fabricated")
				return
			}
			require.NoError(t, err)
			require.Equal(t, "0123456789", subject)
			require.Equal(t, now, snap.FetchedAt)
			require.Len(t, snap.Subscriptions, 1)
			require.Equal(t, c.remote, snap.Subscriptions[0].Status)
			require.Equal(t, c.noExpiry, snap.Subscriptions[0].NextBillingAt == nil)
			d := decideUnknown("0123456789", &periodEnd, snap, now)
			require.Equal(t, c.want, d.Kind, d.Reason)
			require.Equal(t, c.gone, d.RemoteGone)
		})
	}
}
