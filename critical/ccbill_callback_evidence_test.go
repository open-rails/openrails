//go:build integration

package critical_test

import (
	"fmt"
	"math/rand/v2"
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

// addRebill makes DataLink list txn, dated at.
func (f *callbackEvidenceFeed) addRebill(txn string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rebills[txn] = at
}

// callbackEvidence is alpha with a CCBill member imported paid through
// paid, its webhooks from loopback on account, its DataLink the feed and
// its workers running unless the refresh is run by hand.
type callbackEvidence struct {
	*authorityWorld
	account string
	alice   billing.CustomerID
	feed    *callbackEvidenceFeed
	refresh func()
}

func startCallbackEvidence(t *testing.T, account string, active bool, paidThrough func(now time.Time) time.Time) *callbackEvidence {
	t.Helper()
	c := prepareCallbackEvidence(t, account, active, paidThrough)
	fleet := hostFleet(t, c.world)
	fleet.PeriodicJobs().Clear()
	require.NoError(t, c.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
	require.NoError(t, fleet.Start(t.Context()))
	c.refresh = func() { refreshMerchant(t, fleet, c.alpha.MerchantID().UUID()) }
	return c
}

func prepareCallbackEvidence(t *testing.T, account string, active bool, paidThrough func(now time.Time) time.Time) *callbackEvidence {
	t.Helper()
	c := &callbackEvidence{account: account}
	var paid time.Time
	c.authorityWorld = ccbillWebhookWorld(t, func(w *world) {
		paid = paidThrough(w.clock.Now().UTC().Truncate(time.Second))
		c.feed = newCallbackEvidenceFeed(t, ccbillProvider, active, paid)
		declareCCBillAccount(w, account, c.feed.URL)
		w.deps.ProviderEndpoints.CCBillWebhookSources = []string{"127.0.0.1/32"}
	})
	price := ccbillMemberPrice(t, c.alpha)
	c.alice = customerOf(c.a().alice)
	importLegacyCCBill(t, c.alpha, c.clock.Now().UTC().Truncate(time.Second))(legacyCCBillMember(c.alice, price, ccbillStored, "0812233445566778801", paid))
	armDestructive(t, c.authorityWorld, true)
	return c
}

func (c *callbackEvidence) a() *authorityWorld { return c.authorityWorld }

// rebill posts CCBill's RenewalSuccess for txn, stamped at.
func (c *callbackEvidence) rebill(t *testing.T, txn string, at time.Time) {
	t.Helper()
	status, body := ccbillNoticeTo(t, c.a(), c.account, "0001", "RenewalSuccess", map[string]any{"subscriptionId": ccbillProvider, "transactionId": txn,
		"billedAmount": "10.00", "billedCurrencyCode": "840", "timestamp": at.UTC().Format(time.DateTime), "nextRenewalDate": at.Add(720 * time.Hour).Format(time.DateOnly),
		"cardType": "VISA", "last4": "1111"})
	require.Equal(t, http.StatusOK, status, body)
}

// later moves the clock on by d and refreshes.
func (c *callbackEvidence) later(d time.Duration) {
	c.clock.Advance(d)
	c.refresh()
}

func (c *callbackEvidence) unconfirmed(t *testing.T) []billing.Finding {
	t.Helper()
	page, err := c.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "pull.charge.unconfirmed"})
	require.NoError(t, err)
	return page.Items
}

func (c *callbackEvidence) mismatches(t *testing.T) []billing.Finding {
	t.Helper()
	page, err := c.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "pull.subscription.mismatch"})
	require.NoError(t, err)
	return page.Items
}

func TestCCBillCallbackEvidence(t *testing.T) {
	const forged, genuine = "0999999999999999999", "0912233445566778801"
	dueAgo := func(now time.Time) time.Time { return now.Add(-3 * time.Hour) }

	t.Run("OR-J38-S86/a-back-dated-rebill-callback-no-read-lists-is-raised-once-after-36-hours", func(t *testing.T) {
		c := startCallbackEvidence(t, "945280", false, dueAgo)
		c.refresh()
		// Stamped before the read already made: no later window covers it.
		c.rebill(t, forged, c.clock.Now().UTC().Truncate(time.Second).Add(-2*time.Hour))
		renewed := onlySubscription(t, c.alpha, c.alice)

		c.later(37 * time.Hour)
		found := c.unconfirmed(t)
		require.Len(t, found, 1, "the rebill no read lists is raised")
		f := found[0]
		require.Equal(t, forged, f.SubjectKey)
		require.Equal(t, "critical", f.Severity)
		require.Equal(t, billing.FindingStatus("requires_review"), f.Status)
		local, _ := f.Evidence["local"].(map[string]any)
		require.NotEmpty(t, local["payment_id"], "%v", f.Evidence)
		require.Equal(t, renewed.ID.UUID().String(), local["subscription_id"])
		require.Equal(t, c.alice.UUID().String(), local["customer_id"])
		require.Equal(t, ccbillProvider, local["rail_subscription_id"])
		require.Equal(t, "10000000", fmt.Sprint(local["amount"]))
		require.Equal(t, "USD", local["currency"])
		require.EqualValues(t, 10_000_000, ccbillPayments(t, c.world, c.alice)[forged], "detection only: the payment is kept")

		c.later(time.Hour)
		again := c.unconfirmed(t)
		require.Len(t, again, 1, "a later refresh raises it once")
		require.Equal(t, f.ID, again[0].ID)
	})

	t.Run("OR-J38-S87/a-rebill-callback-a-read-lists-raises-nothing", func(t *testing.T) {
		c := startCallbackEvidence(t, "945280", false, dueAgo)
		at := c.clock.Now().UTC().Truncate(time.Second).Add(-time.Hour)
		c.feed.addRebill(genuine, at)
		c.rebill(t, genuine, at)
		c.refresh()
		c.later(37 * time.Hour)
		require.Empty(t, c.unconfirmed(t))
	})

	t.Run("OR-J38-S88/a-rebill-listed-late-raises-nothing-and-a-listing-resolves-a-raised-one", func(t *testing.T) {
		c := startCallbackEvidence(t, "945280", false, dueAgo)
		now := c.clock.Now().UTC().Truncate(time.Second)
		c.rebill(t, genuine, now.Add(-30*time.Minute))
		c.rebill(t, forged, now.Add(-20*time.Minute))
		c.refresh()
		// DataLink exports the genuine rebill late, dated into a later window,
		// spelled without its leading zero.
		c.feed.addRebill(genuine[1:], now.Add(2*time.Hour))
		c.later(3 * time.Hour)
		c.later(35 * time.Hour)
		found := c.unconfirmed(t)
		require.Len(t, found, 1, "only the rebill no read lists")
		require.Equal(t, forged, found[0].SubjectKey)

		// A later read that lists it resolves it.
		c.feed.addRebill(forged, c.clock.Now().UTC().Add(30*time.Minute))
		c.later(time.Hour)
		require.Empty(t, c.unconfirmed(t))
		resolved, err := c.alpha.GetFinding(t.Context(), found[0].ID)
		require.NoError(t, err)
		require.NotNil(t, resolved.ResolvedAt)
	})

	t.Run("OR-J38-S89/an-unlisted-rebill-callback-waits-36-hours", func(t *testing.T) {
		c := startCallbackEvidence(t, "945280", false, dueAgo)
		c.rebill(t, forged, c.clock.Now().UTC().Truncate(time.Second).Add(-time.Hour))
		c.later(time.Hour)
		require.Empty(t, c.unconfirmed(t))
		c.later(34 * time.Hour)
		require.Empty(t, c.unconfirmed(t), "35 hours after it was received")
		c.later(2 * time.Hour)
		require.Len(t, c.unconfirmed(t), 1, "37 hours after it was received")
	})

	t.Run("OR-J38-S90/a-failed-rebill-cancel-callback-of-a-member-the-roster-lists-is-critical", func(t *testing.T) {
		c := startCallbackEvidence(t, "945280", true, func(now time.Time) time.Time { return now.Add(20 * 24 * time.Hour) })
		status, body := ccbillNoticeTo(t, c.a(), c.account, "0001", "Cancellation", map[string]any{"subscriptionId": ccbillProvider, "reason": "forged", "source": "failedRB"})
		require.Equal(t, http.StatusOK, status, body)
		require.Equal(t, billing.SubscriptionStatus("canceled"), onlySubscription(t, c.alpha, c.alice).Status)
		require.False(t, entitledTo(t, c.alpha, c.alice, "ccbill:member"))

		c.refresh()
		found := c.mismatches(t)
		require.Len(t, found, 1)
		require.Equal(t, "critical", found[0].Severity, "the cancel cut paid access short")
		require.Equal(t, billing.FindingStatus("requires_review"), found[0].Status)
		require.Equal(t, billing.SubscriptionStatus("canceled"), onlySubscription(t, c.alpha, c.alice).Status, "alert only: nothing is restored")
		require.False(t, entitledTo(t, c.alpha, c.alice, "ccbill:member"))
	})

	t.Run("OR-J38-S91/a-rebill-datalink-dates-before-its-callback-raises-nothing", func(t *testing.T) {
		c := startCallbackEvidence(t, "945280", false, dueAgo)
		now := c.clock.Now().UTC().Truncate(time.Second)
		c.rebill(t, genuine, now.Add(-time.Minute))
		// DataLink dates the rebill hours before the callback's stamp.
		c.feed.addRebill(genuine, now.Add(-3*time.Hour))
		c.refresh()
		c.later(37 * time.Hour)
		require.Empty(t, c.unconfirmed(t))
	})

	t.Run("OR-J38-S92/an-ordinary-ccbill-cancellation-the-roster-still-lists-is-not-critical", func(t *testing.T) {
		paidThrough := func(now time.Time) time.Time { return now.Add(24 * time.Hour) }
		c := startCallbackEvidence(t, "945280", true, paidThrough)
		status, body := ccbillNoticeTo(t, c.a(), c.account, "0001", "Cancellation", map[string]any{"subscriptionId": ccbillProvider, "reason": "user", "source": "webAdmin"})
		require.Equal(t, http.StatusOK, status, body)
		ended := onlySubscription(t, c.alpha, c.alice)
		require.Equal(t, billing.SubscriptionStatus("canceled"), ended.Status)
		require.True(t, entitledTo(t, c.alpha, c.alice, "ccbill:member"), "the paid day is kept")

		// CCBill keeps it on the roster for hours after its own expiry.
		c.later(36 * time.Hour)
		for _, f := range c.mismatches(t) {
			require.NotEqual(t, "critical", f.Severity, "an ordinary lapse keeps its severity")
		}
		// Still listed two days after access ended: in doubt.
		c.later(48 * time.Hour)
		found := c.mismatches(t)
		require.Len(t, found, 1)
		require.Equal(t, "critical", found[0].Severity)
	})

	t.Run("OR-J38-S93/a-paced-ccbill-refresh-raises-only-the-rebill-no-read-lists", func(t *testing.T) {
		account := fmt.Sprintf("%06d", 100000+rand.IntN(900000))
		c := prepareCallbackEvidence(t, account, false, dueAgo)
		now := c.clock.Now().UTC().Truncate(time.Second)
		c.feed.addRebill(genuine, now.Add(-time.Hour))
		c.rebill(t, genuine, now.Add(-time.Hour))
		c.rebill(t, forged, now.Add(-time.Hour))
		_, merchantPass, run := liveCCBillRefresh(t, c.world)
		require.NoError(t, merchantPass())
		// A day of transactions an hour from 90 days back moves the clock past 36 hours.
		require.Positive(t, drainPaced(t, c.world, run))
		require.Greater(t, c.clock.Now().Sub(now), 37*time.Hour)
		found := c.unconfirmed(t)
		require.Len(t, found, 1)
		require.Equal(t, forged, found[0].SubjectKey)
		require.Equal(t, "critical", found[0].Severity)
	})
}
