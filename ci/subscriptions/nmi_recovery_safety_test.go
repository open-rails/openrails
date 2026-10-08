//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/config"
	"github.com/stretchr/testify/require"
)

func TestEngineNMIWholePeriodOutageNeverStartsFreshCharge(t *testing.T) {
	t.Parallel()
	for _, admitted := range []bool{false, true} {
		t.Run(map[bool]string{false: "not_admitted", true: "accepted_not_submitted"}[admitted], func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			e := enroll(t, w, "nmi", embedded)
			end := e.periodEnd()
			e.toPeriodEnd()
			if admitted {
				w.nmi.QueryUnavailable(true)
				w.runRenewals()
			}
			w.stop()
			w.advance(35 * day)
			w.nmi.QueryUnavailable(false)
			w.start()
			w.runRenewals()
			w.wake()
			w.runRenewals()
			require.Len(t, e.providerLedger(), 1)
			require.Equal(t, 1, e.providerAttempts(), "no fresh charge for a wholly missed period")
			require.True(t, e.periodEnd().Equal(end), "never reset the original billing boundary")
			w.until(func() bool {
				w.runRenewals()
				return len(w.openFindings("life.due_pass.refused")) == 1
			}, "the missed period is an operator finding")
		})
	}
}

func TestEngineNMIExpiredPeriodStillRecoversPaidReceipt(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	end := e.periodEnd()
	e.toPeriodEnd()
	w.nmi.HideSales(1)
	w.nmi.DropSaleResponses(1)
	w.runRenewals()
	w.stop()
	w.advance(35 * day)
	w.cfg = func(cfg *config.Config) { cfg.EngineAdmissionHold = true }
	w.nmi.Reveal()
	w.start()
	w.until(func() bool { return e.periodEnd().After(end) }, "accepted payment completes even after its period expired")
	require.True(t, e.periodEnd().Equal(end.Add(monthHours*time.Hour)))
	require.Equal(t, 2, e.providerAttempts())
	require.Len(t, completed(w.payments(embedded, e.c.id)), 2)
}

func TestEngineNMIKnownPreDispatchFailureRecovers(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	end := e.periodEnd()
	e.toPeriodEnd()
	// The second vault read happens after the submission fence. Its failure
	// proves this live caller has not made the sale, so another attempt is safe.
	var reads atomic.Int32
	w.nmi.FailRequests(func(r *http.Request) bool {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/v5/customers/") {
			return reads.Add(1) == 2
		}
		return false
	}, http.StatusServiceUnavailable, 1)
	w.runRenewals()
	require.Equal(t, 1, e.providerAttempts())
	w.runRenewals()
	w.until(func() bool { return e.periodEnd().After(end) }, "a proven unsent failure releases the attempt")
	require.Equal(t, 2, e.providerAttempts())
	require.Len(t, completed(w.payments(embedded, e.c.id)), 2)
}

func TestEngineNMIEarlierDeclineCannotReleaseLaterAttempt(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	end := e.periodEnd()
	e.toPeriodEnd()
	w.nmi.SetDecline(visa.Last4, "202")
	w.runRenewals()
	w.nmi.SetDecline(visa.Last4, "")
	w.nmi.UnsupportedDuplicateChecking(true)
	w.nmi.HideSales(1)
	w.nmi.DropSaleResponses(1)
	e.c.must(http.MethodPost, "/subscriptions/"+e.sub.String()+"/retry-now", "first-customer-retry", map[string]any{})
	w.settle()
	w.advance(time.Minute)
	w.wake()
	// Every attempt shares the obligation's order. The previous decline remains
	// visible while this retry's successful payment is absent from Query API.
	var status string
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT status FROM billing.provider_intents WHERE subscription_id = $1 AND intent_type = 'subscription_collection' AND (payload->>'attempt')::int = 1`), e.sub.UUID()).Scan(&status))
	require.Equal(t, "unknown_needs_verify", status, "another attempt's decline cannot close an uncertain payment")
	code, _ := e.c.call(http.MethodPost, "/subscriptions/"+e.sub.String()+"/retry-now", "second-customer-retry", map[string]any{})
	require.NotEqual(t, http.StatusOK, code, "another customer action cannot replace the uncertain attempt")
	require.Len(t, e.providerLedger(), 2)
	w.nmi.Reveal()
	w.until(func() bool { return e.periodEnd().After(end) }, "recover this attempt's actual successful payment")
	require.Len(t, e.providerLedger(), 2)
}

// NMI does not promise bounded Query API visibility or duplicate checking on
// every processor. An empty lookup cannot release an uncertain submission.
func TestEngineNMIHiddenChargeNeverResubmitted(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	end := e.periodEnd()
	e.toPeriodEnd()
	w.nmi.UnsupportedDuplicateChecking(true)
	w.nmi.HideSales(1)
	w.nmi.DropSaleResponses(1)
	w.runRenewals()
	require.Len(t, e.providerLedger(), 2, "the provider committed the renewal but lost its response")
	for range 6 {
		w.advance(time.Hour)
		w.wake()
	}
	require.Len(t, e.providerLedger(), 2, "empty order and vault reads must never authorize another sale")
	require.Equal(t, 2, e.providerAttempts(), "only initial enrollment and one renewal submission")
	require.True(t, e.periodEnd().Equal(end), "the unknown payment remains pending")
	require.Len(t, w.openFindings("life.submission.unresolved"), 1)
	w.nmi.Reveal()
	w.until(func() bool { return e.periodEnd().After(end) }, "the visible provider receipt resumes local completion")
	require.Len(t, e.providerLedger(), 2)
	require.Len(t, completed(w.payments(embedded, e.c.id)), 2)
	require.Empty(t, w.openFindings("life.submission.unresolved"))
}
