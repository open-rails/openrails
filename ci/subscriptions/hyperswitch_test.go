//go:build e2e && integration

package subscriptions_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/nmimock"
)

// hsFake is a HyperSwitch deployment: the persistent cards it holds and the
// proxy that sends each NMI sale to the gateway with the card filled in.
type hsFake struct {
	mu      sync.Mutex
	account string
	methods map[string]string // method id -> HyperSwitch customer id
	proxied []url.Values
	// gateway charges the filled-in card and answers its transaction id.
	gateway func(form url.Values) string
	// decline is the issuer's answer to the next sale; reissue is the card
	// HyperSwitch's updater holds after it.
	decline string
	reissue map[string]string
	card    map[string]string
}

func newHSFake(t *testing.T, account string) (*hsFake, string) {
	f := &hsFake{account: account, methods: map[string]string{}}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	return f, server.URL
}

func (f *hsFake) sales() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.proxied)
}

func (f *hsFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v2/proxy":
		var routes []map[string]string
		for _, destination := range []string{nmi.DefaultDirectPostURL, nmi.GatewayDirectPostURL, nmi.SandboxDirectPostURL} {
			routes = append(routes, map[string]string{"destination_url": destination, "method": http.MethodPost, "response_profile": "nmi_classic"})
		}
		reply(http.StatusOK, map[string]any{"contract": "openrails-nmi-form-v2", "strict": true, "max_response_bytes": 65536, "routes": routes})
	case r.Method == http.MethodPost && r.URL.Path == "/v2/proxy":
		var req struct {
			Body  map[string]string `json:"request_body"`
			Token string            `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			reply(http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if _, ok := f.methods[req.Token]; !ok || req.Body["ccnumber"] != "{{$card_number}}" {
			reply(http.StatusBadRequest, map[string]any{"error": "unknown payment method"})
			return
		}
		form := url.Values{}
		for k, v := range req.Body {
			form.Set(k, v)
		}
		if code := f.decline; code != "" {
			f.decline = ""
			if f.reissue != nil {
				f.card = f.reissue
			}
			reply(http.StatusOK, map[string]any{"status_code": 200, "response_headers": map[string]any{},
				"response": map[string]string{"response": "2", "responsetext": "Declined", "response_code": code, "transactionid": "hs_declined_" + uuid.NewString()[:8]}})
			return
		}
		f.proxied = append(f.proxied, form)
		reply(http.StatusOK, map[string]any{"status_code": 200, "response_headers": map[string]any{},
			"response": map[string]string{"response": "1", "responsetext": "Approved", "response_code": "100", "authcode": "123456", "transactionid": f.gateway(form)}})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v2/payment-methods/"):
		id := strings.TrimPrefix(r.URL.Path, "/v2/payment-methods/")
		customer, ok := f.methods[id]
		if !ok {
			reply(http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		card := f.card
		if card == nil {
			card = map[string]string{"last4_digits": "1111", "expiry_month": "12", "expiry_year": "2035", "card_network": "Visa"}
		}
		reply(http.StatusOK, map[string]any{"id": id, "merchant_id": f.account, "customer_id": customer, "storage_type": "persistent",
			"payment_method_data": map[string]any{"card": card}})
	default:
		reply(http.StatusNotFound, map[string]any{"error": r.Method + " " + r.URL.Path})
	}
}

// hyperSwitchMember is a membership paid with a card HyperSwitch holds,
// charged through its proxy into the merchant's NMI account.
type hyperSwitchMember struct {
	hs     *hsFake
	keyed  string
	c      *customer
	method uuid.UUID
	sub    billing.SubscriptionID
	end    time.Time
}

func newHyperSwitchMember(t *testing.T) (*world, *hyperSwitchMember) {
	t.Helper()
	hs, hsURL := newHSFake(t, "hs-merchant-e2e")
	key := make([]byte, 32)
	w := prepareWorld(t, 12, func(cfg *openrails.Config) {
		cfg.Encryption = &openrails.EncryptionConfig{MasterKey: base64.StdEncoding.EncodeToString(key)}
		cfg.HyperSwitch = &openrails.HyperSwitchConfig{APIBaseURL: hsURL, SDKURL: hsURL, AllowLoopbackHTTP: true}
	})
	w.custodians = map[string]openrails.CustodianConfig{"hs": {Kind: "hyperswitch", AccountID: hs.account,
		Settings: map[string]any{"public_api_key": "pk_hs_e2e", "profile_id": "pro_e2e"}, Secrets: map[string]string{"api_key": "hs_api_key_e2e"}}}
	w.declare = func(psps map[string]openrails.PSPConfig) {
		// The merchant's one NMI account takes its cards through HyperSwitch.
		psps["nmi"] = openrails.NMIPSP{AccountID: "e2e-nmi-hs", SecurityKey: "e2e-nmi-hs-key", WebhookSigningSecret: "nmi_hs_webhook_e2e", Custodian: "hs"}.PSPConfig()
	}
	w.start()
	m := &hyperSwitchMember{hs: hs, keyed: w.nmi.AddVault(nmimock.Card{Brand: "visa", Last4: "1111"})}
	hs.gateway = func(form url.Values) string {
		return w.nmi.AddSale(nmimock.Sale{Vault: m.keyed, OrderID: form.Get("orderid"), OrderDescription: form.Get("order_description"),
			Amount: form.Get("amount"), Currency: strings.ToUpper(form.Get("currency")), InitiatedBy: form.Get("initiated_by"),
			Indicator: form.Get("stored_credential_indicator"), Initial: form.Get("initial_transaction_id")}).TransactionID
	}

	mid := engine.Graph(w.rt).Runtime.ConfiguredMerchant()
	m.c = w.newCustomer()
	hs.methods["pm_hs_e2e"] = "cus_hs_e2e"
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`INSERT INTO billing.payment_methods
		(merchant_id, customer_id, psp_id, rail, custodian, custodian_id, rail_customer_ref, rail_method_ref, card_brand, card_last4, card_exp_month, card_exp_year, charge_via, status)
		SELECT $1, $2, NULL, 'nmi', 'hyperswitch', id, 'cus_hs_e2e', 'pm_hs_e2e', 'visa', '1111', 12, 2035, 'pan_proxy', 'active'
		FROM billing.custodians WHERE merchant_id = $1 AND kind = 'hyperswitch' RETURNING id`), mid.UUID(), m.c.cid().UUID()).Scan(&m.method))

	price := w.membershipEvery("content:hs", 9_990_000, monthHours)
	// Checkout sessions offer no custodian PSP; the engine's checkout sells
	// through it.
	got, err := w.engineCheckout(m.c, checkout.CheckoutAttemptCreateRequest{
		PriceID: price.ID.String(), Payment: checkout.CheckoutAttemptPaymentRequest{Rail: "nmi", PaymentMethodID: "pm_" + m.method.String()},
	})
	require.NoError(t, err)
	require.Equal(t, "succeeded", got.Status, "%+v", got)
	w.settle()
	var raw uuid.UUID
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT id FROM billing.subscriptions WHERE merchant_id = $1 AND customer_id = $2`), mid.UUID(), m.c.cid().UUID()).Scan(&raw))
	m.sub = billing.SubscriptionID(raw)
	require.Equal(t, 1, hs.sales(), "the first period is charged through the proxy")
	m.end = *w.subscription(embedded, m.sub).CurrentPeriodEndsAt
	return w, m
}

// A renewal on a HyperSwitch-held card reads the period's NMI order before
// charging, as a vault card's does: a sale another copy of the book already
// made for the period completes the renewal instead of charging it twice.
func TestHyperSwitchRenewalReadsTheOrderBeforeCharging(t *testing.T) {
	t.Parallel()
	w, m := newHyperSwitchMember(t)
	hs, keyed, c, subID, end := m.hs, m.keyed, m.c, m.sub, m.end
	mid := engine.Graph(w.rt).Runtime.ConfiguredMerchant()

	// The period falls due with provider coverage fresh. Then another copy of
	// the book charges it through the same proxy, after the last refresh.
	w.advanceHealthyTo(end.Add(time.Second))
	psp := w.psp["nmi"].UUID()
	order := subscriptions.ObligationOrderReference(subID.UUID(), end)
	operation := subscriptions.SubscriptionCollectionOperationID(mid.UUID(), psp, subscriptions.SubscriptionCollectionPayload{
		Renewal: subscriptions.RenewalTerms{SubscriptionID: subID.UUID()}, PreviousPeriodEnd: end, Attempt: 1})
	other := w.nmi.AddSale(nmimock.Sale{Vault: keyed, OrderID: order, OrderDescription: subscriptions.SubscriptionCollectionDescription(operation),
		Amount: "9.99", Currency: "USD", InitiatedBy: "merchant", Indicator: "used", At: w.clock.Now()})

	w.runRenewals()
	w.settle()
	require.Equal(t, 1, hs.sales(), "the period another copy charged is not charged again")
	require.True(t, w.subscription(embedded, subID).CurrentPeriodEndsAt.After(end), "the renewal is paid by that sale")
	paid := completed(w.payments(embedded, c.id))
	require.Len(t, paid, 2)
	require.Contains(t, []string{paid[0].TransactionID, paid[1].TransactionID}, other.TransactionID, "the renewal is that sale")
}

// HyperSwitch's updater rewrites a card under the same method. A renewal
// declined as an expired card reads it there before the member is asked: the
// same method mirrors the reissued card and the renewal is collected.
func TestHyperSwitchUpdaterIsMirroredOnDecline(t *testing.T) {
	t.Parallel()
	w, m := newHyperSwitchMember(t)
	m.hs.mu.Lock()
	m.hs.decline = "223"
	m.hs.reissue = map[string]string{"last4_digits": "1111", "expiry_month": "03", "expiry_year": "2031", "card_network": "Visa"}
	m.hs.mu.Unlock()
	w.advanceHealthyTo(m.end.Add(time.Second))
	w.runRenewals()
	w.settle()
	method := "pm_" + m.method.String()
	require.Equal(t, []string{"hyperswitch_updater/updated"}, w.methodVersions(method))
	require.Equal(t, []string{"3", "2031"}, []string{w.methodRow(method, "card_exp_month::text"), w.methodRow(method, "card_exp_year::text")})
	require.Zero(t, m.c.notificationCount("payment_method_update_required"), "the member is not asked")
	w.runRenewals()
	w.settle()
	require.Equal(t, 2, m.hs.sales(), "the renewal is collected on the reissued card")
	require.True(t, w.subscription(embedded, m.sub).CurrentPeriodEndsAt.After(m.end))
}
