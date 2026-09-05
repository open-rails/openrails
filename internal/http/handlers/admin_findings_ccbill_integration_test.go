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
// cancel drain: view answers "rebilling" and cancel answers success.
type fakeCCBillSMS struct {
	viewCalls   atomic.Int64
	cancelCalls atomic.Int64
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
	           current_period_starts_at, current_period_ends_at, started_at, customer_id, merchant_id, psp_id)
	        VALUES ($1, $2, $3, 'active', 'ccbill', $4, $5, $6, $5, $7, $8, $9)`,
		subID, fx.price, fx.product, psid, now.Add(-24*time.Hour), now.Add(29*24*time.Hour), fx.customer, fx.merchant, fx.pspFor("ccbill"))
	return subID
}

// Cancellation-only remains supported independently of refund qualification.
func TestFindingsQueueApproveCCBillCancelOnly(t *testing.T) {
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

}

func TestFindingsCCBillRefundRefusesBeforeCancellation(t *testing.T) {
	fx := newFindingsFixture(t)
	subID := fx.seedActiveCCBillSubscription("sub_" + uuid.NewString())
	paymentID := uuid.New()
	fx.exec(`INSERT INTO openrails.payments (id,price_id,rail,transaction_id,amount,list_amount,currency,status,subscription_id,customer_id,merchant_id,money_movement,psp_id) VALUES ($1,$2,'ccbill',$3,10000000,10000000,'USD','completed',$4,$5,$6,'rail',$7)`, paymentID, fx.price, "txn_"+uuid.NewString(), subID, fx.customer, fx.merchant, fx.pspFor("ccbill"))
	finding := fx.seedFinding("consistency.duplicate.ownership", "cc-refund-"+uuid.NewString(), "critical", "unqualified refund", &recommend.Recommendation{Action: recommend.ActionCancelAndRefund, Params: map[string]any{"subscription_id": subID.String(), "refund_payment_id": paymentID.String()}})
	rec := fx.do(AdminResolveFinding, http.MethodPost, "/findings/"+finding.String()+"/resolve", map[string]any{"outcome": "approve"}, finding.String())
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "automatic CCBill refunds are unavailable")
	var status string
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx, `SELECT status FROM openrails.subscriptions WHERE merchant_id=$1 AND id=$2`, fx.merchant, subID).Scan(&status))
	require.Equal(t, "active", status)
	var count int
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM openrails.rail_intents WHERE merchant_id=$1`, fx.merchant).Scan(&count))
	require.Zero(t, count, "neither cancellation nor refund may be enqueued")
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM openrails.payments WHERE merchant_id=$1 AND refunded_payment_id=$2`, fx.merchant, paymentID).Scan(&count))
	require.Zero(t, count)
}
