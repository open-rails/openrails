package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/shared/webhookutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// stubCCBillLiveProbe swaps the catalog probe for the duration of a test.
func stubCCBillLiveProbe(t *testing.T, hasLive bool, err error) {
	t.Helper()
	orig := ccbillLiveAccountsExist
	ccbillLiveAccountsExist = func(*httprequest.Request) (bool, error) { return hasLive, err }
	t.Cleanup(func() { ccbillLiveAccountsExist = orig })
}

func newCCBillWebhookRequest(t *testing.T, testMode bool, remoteAddr string) (*httprequest.Request, *httptest.ResponseRecorder) {
	t.Helper()
	w := httptest.NewRecorder()
	body := `{"eventType":"NewSaleSuccess","clientAccnum":"900000","clientSubacc":"0000","transactionId":"1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/ccbill?eventType=NewSaleSuccess", strings.NewReader(body))
	req.RemoteAddr = remoteAddr
	req.SetPathValue("provider", "ccbill")
	req = req.WithContext(merchant.WithID(req.Context(), merchant.ID(uuid.New())))
	rt := &app.Runtime{Config: &config.Config{TestMode: testMode}}
	return httprequest.NewHTTP(w, req, rt), w
}

// #668: CCBill has no HMAC — the source-IP allowlist is its only
// authentication. The test_mode bypass must be refused whenever a live CCBill
// provider account is declared: live posture + test_mode=true must still 403
// a webhook from a non-CCBill IP.
func TestCCBillWebhookLivePostureRejectsForgedIPDespiteTestMode(t *testing.T) {
	stubCCBillLiveProbe(t, true, nil)
	r, w := newCCBillWebhookRequest(t, true, "203.0.113.5:12345")

	Webhook(r)

	require.Equal(t, http.StatusForbidden, w.Code)
}

// Sandbox posture (test_mode, no live CCBill account anywhere) keeps the dev
// bypass: a non-CCBill IP passes IP auth and the request proceeds to enqueue
// (which 500s here only because no River producer is wired — the point is it
// is NOT a 403).
func TestCCBillWebhookSandboxPostureKeepsDevBypass(t *testing.T) {
	stubCCBillLiveProbe(t, false, nil)
	r, w := newCCBillWebhookRequest(t, true, "203.0.113.5:12345")

	Webhook(r)

	require.NotEqual(t, http.StatusForbidden, w.Code)
	require.Equal(t, http.StatusInternalServerError, w.Code) // enqueue fails: no River producer in test runtime
}

func TestCCBillWebhookIPAllowedMatrix(t *testing.T) {
	newReq := func(testMode bool) *httprequest.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/ccbill", nil)
		return httprequest.NewHTTP(httptest.NewRecorder(), req, &app.Runtime{Config: &config.Config{TestMode: testMode}})
	}

	// Allowlisted CCBill source IP always passes, regardless of posture.
	stubCCBillLiveProbe(t, true, nil)
	require.True(t, ccbillWebhookIPAllowed(newReq(false), "64.38.212.5"))
	require.True(t, ccbillWebhookIPAllowed(newReq(true), "64.38.212.5"))

	// test_mode off: non-allowlisted IP is always rejected.
	require.False(t, ccbillWebhookIPAllowed(newReq(false), "203.0.113.5"))

	// test_mode on + live CCBill account declared: bypass refused.
	require.False(t, ccbillWebhookIPAllowed(newReq(true), "203.0.113.5"))

	// Probe failure fails closed to the allowlist.
	stubCCBillLiveProbe(t, false, errors.New("catalog unavailable"))
	require.False(t, ccbillWebhookIPAllowed(newReq(true), "203.0.113.5"))

	// Sandbox posture (test_mode, no live account): dev bypass applies.
	stubCCBillLiveProbe(t, false, nil)
	require.True(t, ccbillWebhookIPAllowed(newReq(true), "203.0.113.5"))

	// Nil config / nil state never bypass.
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/ccbill", nil)
	require.False(t, ccbillWebhookIPAllowed(httprequest.NewHTTP(httptest.NewRecorder(), req, nil), "203.0.113.5"))
}

// #697: the composite CCBill account identity is dash-joined
// (clientAccnum-clientSubacc), matching CCBill's own convention and the declared
// rail_merchant_accounts.account_id format.
func TestCCBillWebhookAccountIDIsDashJoined(t *testing.T) {
	require.Equal(t, "900000-0000",
		ccbillWebhookAccountID([]byte(`{"clientAccnum":"900000","clientSubacc":"0000"}`)))
	// Numeric wire values coerce the same way.
	require.Equal(t, "900000-3",
		ccbillWebhookAccountID([]byte(`{"clientAccnum":900000,"clientSubacc":3}`)))
	// No subacc → bare accnum, no separator.
	require.Equal(t, "900000",
		ccbillWebhookAccountID([]byte(`{"clientAccnum":"900000"}`)))
	// No accnum → no identity.
	require.Empty(t, ccbillWebhookAccountID([]byte(`{"clientSubacc":"0000"}`)))
}

// #668: CCBill events carry no signature, so they must never claim
// SignatureValid — on the direct-dispatch path or the River path.
func TestCCBillWebhookMessageNeverClaimsSignatureValid(t *testing.T) {
	prepared, err := webhookutil.PrepareCCBill([]byte(`{"eventType":"NewSaleSuccess","clientAccnum":"900000","transactionId":"1"}`), "NewSaleSuccess")
	require.NoError(t, err)

	msg := ccbillWebhookMessage("64.38.212.5", prepared, "900000-0000")
	require.Nil(t, msg.SignatureValid)
	require.Equal(t, "900000-0000", msg.RailMerchantAccountID)

	// River path: an unverified Prepared must enqueue with SignatureValid nil.
	require.Nil(t, prepared.QueueArgs("64.38.212.5").SignatureValid)
}
