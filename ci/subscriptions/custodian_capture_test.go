//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/stretchr/testify/require"
)

// A custodian save is an owned pending payment method, not a purchase or a
// payment. Only the later order charges the verified permanent instrument.
func TestCustodianCaptureUsesThePaymentMethodResource(t *testing.T) {
	hs, hsURL := newHSFake(t, "hs-capture-e2e")
	w := prepareWorld(t, 12, func(cfg *openrails.Config) {
		cfg.HyperSwitch = &openrails.HyperSwitchConfig{APIBaseURL: hsURL, SDKURL: hsURL, AllowLoopbackHTTP: true}
	})
	w.custodians = map[string]openrails.CustodianConfig{"hs": {Kind: "hyperswitch", AccountID: hs.account, Settings: map[string]any{"public_api_key": "pk_capture", "profile_id": "pro_capture"}, Secrets: map[string]string{"api_key": "hs_capture_key"}}}
	w.declare = func(psps map[string]openrails.PSPConfig) {
		psps["nmi"] = openrails.NMIPSP{Environment: openrails.EnvironmentSandbox, AccountID: "nmi-capture", SecurityKey: "nmi-capture-key", WebhookSigningSecret: "capture-webhook-key", Custodian: "hs"}.PSPConfig()
	}
	w.start()
	vault := w.nmi.AddVault(nmimock.Card{Brand: "visa", Last4: "1111"})
	hs.gateway = func(form url.Values) string {
		return w.nmi.AddSale(nmimock.Sale{Vault: vault, OrderID: form.Get("orderid"), OrderDescription: form.Get("order_description"), Amount: form.Get("amount"), Currency: strings.ToUpper(form.Get("currency")), InitiatedBy: form.Get("initiated_by"), Indicator: form.Get("stored_credential_indicator"), Initial: form.Get("initial_transaction_id")}).TransactionID
	}
	c, other := w.newCustomer(), w.newCustomer()
	request := map[string]any{"psp_id": w.psp["nmi"], "billing_details": map[string]any{"name": "Capture Buyer", "email": "capture@example.test"}}
	code, out := c.call(http.MethodPost, "/payment-methods", "", request)
	require.Equal(t, http.StatusBadRequest, code, "%v", out)
	created := c.must(http.MethodPost, "/payment-methods", "capture-1", request)
	id := created["id"].(string)
	require.Equal(t, "requires_action", created["status"])
	require.Nil(t, created["psp_id"], "the custodian holds the card; the PSP only routes later charges")
	var attempts int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.order_attempts WHERE customer_id=$1`), c.cid().UUID()).Scan(&attempts))
	require.Zero(t, attempts, "card capture creates no purchase or payable")
	action := created["next_action"].(map[string]any)
	require.Equal(t, "capture", action["type"])
	payload := action["payload"].(map[string]any)
	require.Equal(t, "private-sdk-authorization", payload["sdk_authorization"])
	require.NotContains(t, w.methodRow(id, "capture::text"), "private-sdk-authorization", "SDK authority is encrypted at rest")
	require.Zero(t, hs.sales(), "saving cannot charge")
	replay := c.must(http.MethodPost, "/payment-methods", "capture-1", request)
	require.Equal(t, created, replay)
	hs.mu.Lock()
	capture := hs.captures[payload["session_id"].(string)]
	require.Len(t, hs.captures, 1, "a replay opens no second vendor session")
	hs.mu.Unlock()
	confirm := map[string]any{"capture": map[string]any{"custodian_id": payload["custodian_id"], "session_id": payload["session_id"], "token": capture.Token}}
	code, out = other.call(http.MethodPost, "/payment-methods/"+id+"/confirm", "", confirm)
	require.Equal(t, http.StatusNotFound, code, "%v", out)
	bad := map[string]any{"capture": map[string]any{"custodian_id": payload["custodian_id"], "session_id": payload["session_id"], "token": "another-token"}}
	code, out = c.call(http.MethodPost, "/payment-methods/"+id+"/confirm", "", bad)
	require.Equal(t, http.StatusConflict, code, "%v", out)
	saved := c.must(http.MethodPost, "/payment-methods/"+id+"/confirm", "", confirm)
	require.Equal(t, id, saved["id"])
	require.Equal(t, "active", saved["status"])
	require.Nil(t, saved["next_action"])
	require.NotContains(t, w.methodRow(id, "capture::text"), "secret_ciphertext")
	require.Equal(t, saved, c.must(http.MethodPost, "/payment-methods/"+id+"/confirm", "", confirm))
	require.Equal(t, []string{"customer_save/saved"}, w.methodVersions(id))
	require.Zero(t, hs.sales())
	// A second token of this same owned instrument resolves to its canonical
	// method and leaves the pending id as replaced, without another version.
	second := c.must(http.MethodPost, "/payment-methods", "capture-2", request)
	secondID := second["id"].(string)
	secondPayload := second["next_action"].(map[string]any)["payload"].(map[string]any)
	hs.mu.Lock()
	token := hs.captures[secondPayload["session_id"].(string)].Token
	hs.mu.Unlock()
	duplicate := c.must(http.MethodPost, "/payment-methods/"+secondID+"/confirm", "", map[string]any{"capture": map[string]any{"custodian_id": secondPayload["custodian_id"], "session_id": secondPayload["session_id"], "token": token}})
	require.Equal(t, id, duplicate["id"])
	require.Equal(t, "replaced", w.methodRow(secondID, "status"))
	price := w.membershipEvery("capture:member", 9_990_000, monthHours)
	c.mustPurchase(embedded, order{price: price.ID, rail: "nmi", method: id})
	require.Equal(t, 1, hs.sales(), "only the order charges")
	expiring := c.must(http.MethodPost, "/payment-methods", "capture-expiry", request)
	w.clock.Advance(16 * time.Minute)
	code, out = c.call(http.MethodPost, "/payment-methods", "capture-expiry", request)
	require.Equal(t, http.StatusPaymentRequired, code, "%v", out)
	require.Equal(t, string(billing.PaymentMethodRemoved), w.methodRow(expiring["id"].(string), "status"))
	require.NotContains(t, w.methodRow(expiring["id"].(string), "capture::text"), "secret_ciphertext")
}
