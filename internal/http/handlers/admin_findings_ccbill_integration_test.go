//go:build integration

package handlers

import (
	"encoding/json"
	"fmt"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/railresolve"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/reconcile/recommend"
)

// fakeCCBillSMS scripts DataLink's subscriptionManagement.cgi for the admin
// cancel + refund drain: view answers "rebilling", cancel/refund answer success.
type fakeCCBillSMS struct {
	viewCalls   atomic.Int64
	cancelCalls atomic.Int64
	refundCalls atomic.Int64
	status      atomic.Value // string
}

// #788: the intent handlers arm their DataLink clients from the armed rail
// state; the test rails carry the fake server's credentials.
func adminFindingsCCBillRails() railresolve.FixedSet {
	return railresolve.FixedSet{"ccbill": {
		Rail:      models.RailCCBill,
		AccountID: "900100-0000",
		CCBill:    &config.CCBillRailConfig{DataLinkUsername: "dluser", DataLinkPassword: "dlpass"},
	}}
}

func newAdminFindingsCCBillCancelHandler(dbi *db.DB, client *ccbill.DataLinkClient) *intents.CCBillCancelHandler {
	h := intents.NewCCBillCancelHandler(dbi, nil, adminFindingsCCBillRails(), nil)
	h.DataLinkBaseURL = client.BaseURL
	return h
}

func newAdminFindingsCCBillRefundHandler(dbi *db.DB, client *ccbill.DataLinkClient) *intents.CCBillRefundHandler {
	h := intents.NewCCBillRefundHandler(dbi, nil, adminFindingsCCBillRails(), nil)
	h.DataLinkBaseURL = client.BaseURL
	return h
}

func newFakeCCBillSMS(t *testing.T) (*fakeCCBillSMS, *ccbill.DataLinkClient) {
	t.Helper()
	f := &fakeCCBillSMS{}
	f.status.Store("2")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		switch r.PostForm.Get("action") {
		case "viewSubscriptionStatus":
			f.viewCalls.Add(1)
			fmt.Fprintf(w, `<results><subscriptionStatus>%s</subscriptionStatus></results>`, f.status.Load().(string))
		case "cancelSubscription":
			f.cancelCalls.Add(1)
			f.status.Store("1")
			fmt.Fprint(w, `<results>1</results>`)
		case "voidOrRefundTransaction":
			f.refundCalls.Add(1)
			fmt.Fprint(w, `<results>1</results>`)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return f, &ccbill.DataLinkClient{
		BaseURL: srv.URL, ClientAccNum: "900100", ClientSubAcc: "0000",
		Username: "u", Password: "p", HTTPClient: srv.Client(),
	}
}

func (fx *findingsFixture) seedActiveCCBillSubscription(psid string) uuid.UUID {
	fx.t.Helper()
	subID := uuid.New()
	now := time.Now().UTC()
	fx.exec(`INSERT INTO openrails.subscriptions
	          (id, price_id, product_id, status, rail, rail_subscription_id,
	           current_period_starts_at, current_period_ends_at, started_at, customer_id, merchant_id)
	        VALUES ($1, $2, $3, 'active', 'ccbill', $4, $5, $6, $5, $7, $8)`,
		subID, fx.price, fx.product, psid, now.Add(-24*time.Hour), now.Add(29*24*time.Hour), fx.customer, fx.merchant)
	return subID
}

// #696 admin path: approving a cancel_and_refund on a CCBill subscription
// executes the local cancel and queues the durable ccbill_cancel intent
// (admin-origin); the executor drains it through the fake DataLink server. The
// refund leg (#696 follow-up) now rides the same intent ledger — it queues a
// ccbill_refund intent instead of bouncing to the manual admin portal — and
// the executor drains that too against the fake DataLink server.
func TestFindingsQueueApproveCCBillCancelAndRefund(t *testing.T) {
	fx := newFindingsFixture(t)
	psid := "ccsub-" + uuid.NewString()[:8]
	subID := fx.seedActiveCCBillSubscription(psid)

	// --- cancel-only approve: fixed end-to-end ---
	cancelOnly := fx.seedFinding("consistency.duplicate.ownership", "cc-cancel-"+uuid.NewString()[:8], "critical",
		"duplicate ccbill sub; cancel it", &recommend.Recommendation{
			Action: recommend.ActionCancelAndRefund,
			Params: map[string]any{"subscription_id": subID.String()},
		})
	rec := fx.do(AdminResolveFinding, http.MethodPost, "/findings/"+cancelOnly.String()+"/resolve",
		map[string]any{"outcome": "approve", "notes": "confirmed duplicate"}, cancelOnly.String())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resolved resolveBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resolved))
	assert.Equal(t, "cancelled", resolved.Execution["cancel"])
	assert.Equal(t, "queued", resolved.Execution["cancel_intent"])
	assert.Equal(t, "fixed", resolved.Finding.Status)

	var subStatus string
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx,
		`SELECT status FROM openrails.subscriptions WHERE id = $1`, subID).Scan(&subStatus))
	assert.Equal(t, "cancelled", subStatus)

	var intentID uuid.UUID
	var intentStatus, origin string
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx,
		`SELECT id, status, origin FROM openrails.rail_intents
		 WHERE merchant_id = $1 AND intent_type = $2 AND subscription_id = $3`,
		fx.merchant, intents.TypeCCBillCancelSubscription, subID).Scan(&intentID, &intentStatus, &origin))
	assert.Equal(t, intents.StatusPending, intentStatus, "remote cancel rides the ledger (queue-always #679)")
	assert.Equal(t, string(intents.OriginAdmin), origin)

	// Drain through the choke point against the fake DataLink server.
	fake, client := newFakeCCBillSMS(t)
	runner := &intents.Runner{
		Store: intents.NewStore(fx.dbi),
		Registry: intents.NewRegistry(
			newAdminFindingsCCBillCancelHandler(fx.dbi, client),
			newAdminFindingsCCBillRefundHandler(fx.dbi, client),
		),
		Config:  fx.rt.Config,
		Breaker: intents.NewVolumeBreaker(fx.dbi),
	}
	_, err := runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx,
		`SELECT status FROM openrails.rail_intents WHERE id = $1`, intentID).Scan(&intentStatus))
	assert.Equal(t, intents.StatusSucceeded, intentStatus)
	assert.EqualValues(t, 1, fake.cancelCalls.Load())

	// --- refund leg: rides the ccbill_refund intent ledger (queue-always #679) ---
	payID := uuid.New()
	txnID := "cctxn-" + uuid.NewString()[:8]
	fx.exec(`INSERT INTO openrails.payments
	          (id, price_id, rail, transaction_id, amount, list_amount, currency, status, subscription_id, customer_id, merchant_id)
	        VALUES ($1, $2, 'ccbill', $3, 10000000, 10000000, 'usd', 'completed', $4, $5, $6)`,
		payID, fx.price, txnID, subID, fx.customer, fx.merchant)
	refundFinding := fx.seedFinding("consistency.duplicate.ownership", "cc-refund-"+uuid.NewString()[:8], "critical",
		"refund the duplicate ccbill charge", &recommend.Recommendation{
			Action: recommend.ActionCancelAndRefund,
			Params: map[string]any{"subscription_id": subID.String(), "refund_payment_id": payID.String()},
		})
	rec = fx.do(AdminResolveFinding, http.MethodPost, "/findings/"+refundFinding.String()+"/resolve",
		map[string]any{"outcome": "approve", "notes": "dup charge"}, refundFinding.String())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resolved))
	assert.Equal(t, "noop_already_cancelled", resolved.Execution["cancel"])
	assert.Equal(t, "queued", resolved.Execution["refund"], "the runtime's intent registry has no CCBill DataLink client wired -> parks, queued not inline (mirrors cancel_intent)")
	assert.Equal(t, "fixed", resolved.Finding.Status)

	var refundIntentID uuid.UUID
	var refundIntentStatus, refundOrigin string
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx,
		`SELECT id, status, origin FROM openrails.rail_intents
		 WHERE merchant_id = $1 AND intent_type = $2 AND payment_id = $3`,
		fx.merchant, intents.TypeCCBillRefund, payID).Scan(&refundIntentID, &refundIntentStatus, &refundOrigin))
	assert.Equal(t, intents.StatusPending, refundIntentStatus, "remote refund rides the ledger (queue-always #679)")
	assert.Equal(t, string(intents.OriginAdmin), refundOrigin)

	// The inline attempt during resolve already parked this intent 5m out
	// (ParkRetryInterval) since the runtime's registry has no client wired;
	// force it due so the test's runner (real client) drains it now.
	fx.exec(`UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1`, refundIntentID)

	// Drain the refund through the same choke point against the fake DataLink server.
	_, err = runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx,
		`SELECT status FROM openrails.rail_intents WHERE id = $1`, refundIntentID).Scan(&refundIntentStatus))
	assert.Equal(t, intents.StatusSucceeded, refundIntentStatus)
	assert.EqualValues(t, 1, fake.refundCalls.Load())

	var refundAmount int64
	var refundStatus, refundTxn string
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx,
		`SELECT amount, status, transaction_id FROM openrails.payments WHERE refunded_payment_id = $1`, payID).
		Scan(&refundAmount, &refundStatus, &refundTxn))
	assert.EqualValues(t, -10000000, refundAmount)
	assert.Equal(t, "completed", refundStatus)
	// CCBill has no discrete provider refund id; the recorded ref composes the
	// subscription + original transaction (ccbillRefundProviderRef).
	assert.Equal(t, "ccbill_refund:"+psid+":"+txnID, refundTxn)

	row := fx.findingRow(refundFinding)
	assert.Equal(t, "fixed", row.Status)
}
