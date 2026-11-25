//go:build integration

package critical_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

// callbackEvidenceFeed is CCBill's DataLink: the roster lists sub as active
// through expires when active, and each export names the rebills added to it
// that fall inside its window.
type callbackEvidenceFeed struct {
	*httptest.Server
	mu      sync.Mutex
	rebills map[string]time.Time
}

func newCallbackEvidenceFeed(t *testing.T, sub string, active bool, expires time.Time) *callbackEvidenceFeed {
	t.Helper()
	feed := &callbackEvidenceFeed{rebills: map[string]time.Time{}}
	mst := time.FixedZone("MST", -7*60*60)
	feed.Server = httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil || r.URL.Path != "/data/main.cgi" {
			http.Error(out, "unexpected DataLink request", http.StatusBadRequest)
			return
		}
		if r.Form.Get("transactionTypes") == "ACTIVEMEMBERS" {
			if active {
				fmt.Fprintf(out, "\"ACTIVEMEMBERS\",\"945280\",\"critical-flex\",\"%s\",\"%s\",\"member\",\"member@critical.invalid\",\"1\",\"%s\",\"\"\n",
					sub, expires.Add(-720*time.Hour).Format(time.DateOnly), expires.Format(time.DateOnly))
			}
			return
		}
		from, errFrom := time.ParseInLocation("20060102150405", r.Form.Get("startTime"), mst)
		until, errUntil := time.ParseInLocation("20060102150405", r.Form.Get("endTime"), mst)
		if errFrom != nil || errUntil != nil || until.Sub(from) > 24*time.Hour {
			http.Error(out, "unbounded DataLink export", http.StatusBadRequest)
			return
		}
		feed.mu.Lock()
		defer feed.mu.Unlock()
		for txn, at := range feed.rebills {
			if !at.Before(from) && !at.After(until) {
				fmt.Fprintf(out, "REBILL,945280,0001,%s,%s,%s,10.00\n", sub, at.UTC().Format(time.DateTime), txn)
			}
		}
	}))
	t.Cleanup(feed.Close)
	return feed
}

func (f *callbackEvidenceFeed) addRebill(txn string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rebills[txn] = at
}

// startCallbackEvidenceWorld is alpha with a CCBill member imported paid
// through paidThrough, its webhooks from loopback, its DataLink the feed and
// the provider refresh running; it answers the refresh.
func startCallbackEvidenceWorld(t *testing.T, active bool, paidThrough func(now time.Time) time.Time) (*authorityWorld, billing.CustomerID, *callbackEvidenceFeed, func()) {
	t.Helper()
	var feed *callbackEvidenceFeed
	var paid time.Time
	a := ccbillWebhookWorld(t, func(w *world) {
		paid = paidThrough(w.clock.Now().UTC().Truncate(time.Second))
		feed = newCallbackEvidenceFeed(t, ccbillProvider, active, paid)
		declareCCBill(w, feed.URL)
		w.deps.ProviderEndpoints.CCBillWebhookSources = []string{"127.0.0.1/32"}
	})
	price := ccbillMemberPrice(t, a.alpha)
	alice := customerOf(a.alice)
	importLegacyCCBill(t, a.alpha, a.clock.Now().UTC().Truncate(time.Second))(legacyCCBillMember(alice, price, ccbillStored, "0812233445566778801", paid))
	armDestructive(t, a, true)
	fleet := hostFleet(t, a.world)
	fleet.PeriodicJobs().Clear()
	require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
	require.NoError(t, fleet.Start(t.Context()))
	return a, alice, feed, func() { refreshMerchant(t, fleet, a.alpha.MerchantID().UUID()) }
}

// ccbillRebillNotice posts CCBill's RenewalSuccess for txn at at.
func ccbillRebillNotice(t *testing.T, a *authorityWorld, txn string, at, next time.Time) {
	t.Helper()
	status, body := ccbillNoticeTo(t, a, "945280", "0001", "RenewalSuccess", map[string]any{"subscriptionId": ccbillProvider, "transactionId": txn,
		"billedAmount": "10.00", "billedCurrencyCode": "840", "timestamp": at.UTC().Format(time.DateTime), "nextRenewalDate": next.Format(time.DateOnly),
		"cardType": "VISA", "last4": "1111"})
	require.Equal(t, http.StatusOK, status, body)
}

func unconfirmedCharges(t *testing.T, client *openrails.Client) []billing.Finding {
	t.Helper()
	page, err := client.ListFindings(t.Context(), billing.FindingListParams{Type: "pull.charge.unconfirmed"})
	require.NoError(t, err)
	return page.Items
}

func TestCCBillCallbackEvidence(t *testing.T) {
	const forged = "0999999999999999999"

	t.Run("OR-J38-S86/a-rebill-callback-datalink-never-lists-is-raised-once-and-changes-nothing", func(t *testing.T) {
		a, alice, feed, refresh := startCallbackEvidenceWorld(t, false, func(now time.Time) time.Time { return now.Add(-time.Hour) })
		now := a.clock.Now().UTC().Truncate(time.Second)
		ccbillRebillNotice(t, a, forged, now.Add(-30*time.Minute), now.Add(719*time.Hour))
		renewed := onlySubscription(t, a.alpha, alice)
		require.Equal(t, billing.SubscriptionActive, renewed.Status)

		refresh()
		found := unconfirmedCharges(t, a.alpha)
		require.Len(t, found, 1, "the rebill DataLink never lists is raised")
		f := found[0]
		require.Equal(t, forged, f.SubjectKey)
		require.Equal(t, "critical", f.Severity)
		require.Equal(t, billing.FindingStatus("requires_review"), f.Status)
		local, _ := f.Evidence["local"].(map[string]any)
		require.NotEmpty(t, local["payment_id"], "%v", f.Evidence)
		require.Equal(t, renewed.ID.UUID().String(), local["subscription_id"])
		require.Equal(t, alice.UUID().String(), local["customer_id"])
		require.Equal(t, "10000000", fmt.Sprint(local["amount"]))
		require.Equal(t, "USD", local["currency"])
		remote, _ := f.Evidence["remote"].(map[string]any)
		require.NotEmpty(t, remote["window_since"])
		require.NotEmpty(t, remote["window_until"])

		after := onlySubscription(t, a.alpha, alice)
		require.Equal(t, billing.SubscriptionActive, after.Status, "detection only: the subscription is not changed")
		require.Equal(t, renewed.CurrentPeriodEndsAt, after.CurrentPeriodEndsAt)
		require.True(t, entitledTo(t, a.alpha, alice, "ccbill:member"))
		require.EqualValues(t, 10_000_000, ccbillPayments(t, a.world, alice)[forged], "the payment is kept")

		// A later refresh and a second read of the same window raise it once.
		a.clock.Advance(time.Hour)
		refresh()
		ccbillPull(t, a.world, feed.Server, now.Add(-2*time.Hour), "advisory", nil)
		again := unconfirmedCharges(t, a.alpha)
		require.Len(t, again, 1)
		require.Equal(t, f.ID, again[0].ID)
	})

	t.Run("OR-J38-S87/a-rebill-callback-datalink-lists-raises-nothing", func(t *testing.T) {
		a, alice, feed, refresh := startCallbackEvidenceWorld(t, false, func(now time.Time) time.Time { return now.Add(-time.Hour) })
		now := a.clock.Now().UTC().Truncate(time.Second)
		at := now.Add(-30 * time.Minute)
		feed.addRebill(forged, at)
		ccbillRebillNotice(t, a, forged, at, now.Add(719*time.Hour))
		refresh()
		require.Empty(t, unconfirmedCharges(t, a.alpha))
		require.EqualValues(t, 10_000_000, ccbillPayments(t, a.world, alice)[forged])
	})

	t.Run("OR-J38-S88/a-later-read-that-lists-the-rebill-resolves-it", func(t *testing.T) {
		a, _, feed, _ := startCallbackEvidenceWorld(t, false, func(now time.Time) time.Time { return now.Add(-time.Hour) })
		now := a.clock.Now().UTC().Truncate(time.Second)
		at := now.Add(-30 * time.Minute)
		ccbillRebillNotice(t, a, forged, at, now.Add(719*time.Hour))
		ccbillPull(t, a.world, feed.Server, now.Add(-2*time.Hour), "advisory", nil)
		found := unconfirmedCharges(t, a.alpha)
		require.Len(t, found, 1)

		// DataLink lists it late, without the leading zero.
		feed.addRebill(forged[1:], at)
		ccbillPull(t, a.world, feed.Server, now.Add(-2*time.Hour), "advisory", nil)
		require.Empty(t, unconfirmedCharges(t, a.alpha), "the read that lists the rebill resolves it")
		resolved, err := a.alpha.GetFinding(t.Context(), found[0].ID)
		require.NoError(t, err)
		require.NotNil(t, resolved.ResolvedAt)
	})

	t.Run("OR-J38-S89/a-rebill-callback-outside-the-read-window-is-not-checked", func(t *testing.T) {
		a, _, _, refresh := startCallbackEvidenceWorld(t, false, func(now time.Time) time.Time { return now.Add(-time.Hour) })
		now := a.clock.Now().UTC().Truncate(time.Second)
		// Inside the refresh's safety lag: no read covers it yet.
		ccbillRebillNotice(t, a, forged, now.Add(-time.Minute), now.Add(719*time.Hour))
		refresh()
		require.Empty(t, unconfirmedCharges(t, a.alpha))
	})

	t.Run("OR-J38-S90/a-cancel-callback-of-a-member-the-roster-lists-active-is-critical", func(t *testing.T) {
		a, alice, _, refresh := startCallbackEvidenceWorld(t, true, func(now time.Time) time.Time { return now.Add(20 * 24 * time.Hour) })
		status, body := ccbillNoticeTo(t, a, "945280", "0001", "Cancellation", map[string]any{"subscriptionId": ccbillProvider, "reason": "forged", "source": "failedRB"})
		require.Equal(t, http.StatusOK, status, body)
		require.Equal(t, billing.SubscriptionStatus("canceled"), onlySubscription(t, a.alpha, alice).Status)
		require.False(t, entitledTo(t, a.alpha, alice, "ccbill:member"))

		refresh()
		page, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "pull.subscription.mismatch"})
		require.NoError(t, err)
		require.Len(t, page.Items, 1)
		require.Equal(t, "critical", page.Items[0].Severity)
		require.Equal(t, billing.FindingStatus("requires_review"), page.Items[0].Status)
		require.Equal(t, billing.SubscriptionStatus("canceled"), onlySubscription(t, a.alpha, alice).Status, "alert only: nothing is restored")
		require.False(t, entitledTo(t, a.alpha, alice, "ccbill:member"))
	})
}
