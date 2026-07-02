//go:build integration

package handlers

import (
	"encoding/json"
	"fmt"
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
// cancel drain: view answers "rebilling", cancel answers success.
type fakeCCBillSMS struct {
	viewCalls   atomic.Int64
	cancelCalls atomic.Int64
	status      atomic.Value // string
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
// refund leg stays not-supported with a clear manual-portal error that leaves
// the finding OPEN with the completed cancel documented.
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
		Store:    intents.NewStore(fx.dbi),
		Registry: intents.NewRegistry(intents.NewCCBillCancelHandler(fx.dbi, client, nil)),
		Config:   fx.rt.Config,
		Breaker:  intents.NewVolumeBreaker(fx.dbi),
	}
	_, err := runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx,
		`SELECT status FROM openrails.rail_intents WHERE id = $1`, intentID).Scan(&intentStatus))
	assert.Equal(t, intents.StatusSucceeded, intentStatus)
	assert.EqualValues(t, 1, fake.cancelCalls.Load())

	// --- refund leg: not supported for ccbill, clear manual-portal error ---
	payID := uuid.New()
	fx.exec(`INSERT INTO openrails.payments
	          (id, price_id, rail, transaction_id, amount, list_amount, currency, status, subscription_id, customer_id, merchant_id)
	        VALUES ($1, $2, 'ccbill', $3, 10000000, 10000000, 'usd', 'completed', $4, $5, $6)`,
		payID, fx.price, "cctxn-"+uuid.NewString()[:8], subID, fx.customer, fx.merchant)
	refundFinding := fx.seedFinding("consistency.duplicate.ownership", "cc-refund-"+uuid.NewString()[:8], "critical",
		"refund the duplicate ccbill charge", &recommend.Recommendation{
			Action: recommend.ActionCancelAndRefund,
			Params: map[string]any{"subscription_id": subID.String(), "refund_payment_id": payID.String()},
		})
	rec = fx.do(AdminResolveFinding, http.MethodPost, "/findings/"+refundFinding.String()+"/resolve",
		map[string]any{"outcome": "approve", "notes": "dup charge"}, refundFinding.String())
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "CCBill admin portal", "error names the manual path")

	row := fx.findingRow(refundFinding)
	assert.Equal(t, "requires_review", row.Status, "unsupported refund leaves the finding OPEN")
	require.NotNil(t, row.OperatorNotes)
	assert.Contains(t, *row.OperatorNotes, "CCBill admin portal")
	assert.Contains(t, *row.OperatorNotes, `"cancel":"noop_already_cancelled"`, "compensation state documented")
}
