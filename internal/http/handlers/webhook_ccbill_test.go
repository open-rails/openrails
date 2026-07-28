package handlers

import (
	"context"
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
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/shared/iputil"
	"github.com/open-rails/openrails/internal/shared/webhookutil"
	"github.com/open-rails/openrails/internal/webhookauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// credentialPostureFromBool maps the test helpers' legacy bool parameter onto
// the explicit posture enum (#745).
func credentialPostureFromBool(sandbox bool) config.CredentialPosture {
	if sandbox {
		return config.CredentialPostureSandbox
	}
	return config.CredentialPostureLive
}

// stubCCBillLiveProbe swaps the catalog probe for the duration of a test.
func stubCCBillLiveProbe(t *testing.T, presence merchants.LiveRailPresence, err error) {
	t.Helper()
	orig := ccbillLivePSPProbe
	ccbillLivePSPProbe = func(*httprequest.Request) webhookauth.LiveRailProbe {
		return func(context.Context) (merchants.LiveRailPresence, error) { return presence, err }
	}
	t.Cleanup(func() { ccbillLivePSPProbe = orig })
}

// stubCCBillNoProbe simulates a runtime with no merchants service wired.
func stubCCBillNoProbe(t *testing.T) {
	t.Helper()
	orig := ccbillLivePSPProbe
	ccbillLivePSPProbe = func(*httprequest.Request) webhookauth.LiveRailProbe { return nil }
	t.Cleanup(func() { ccbillLivePSPProbe = orig })
}

// devAllowlist is the explicit operator-declared source allowlist the SEC-19
// gate requires before any non-CCBill address can be considered at all.
var devAllowlist = []string{"203.0.113.0/24"}

func newCCBillWebhookRequest(t *testing.T, testMode bool, remoteAddr string) (*httprequest.Request, *httptest.ResponseRecorder) {
	t.Helper()
	w := httptest.NewRecorder()
	body := `{"eventType":"NewSaleSuccess","clientAccnum":"900000","clientSubacc":"0000","transactionId":"1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/ccbill?eventType=NewSaleSuccess", strings.NewReader(body))
	req.RemoteAddr = remoteAddr
	req.SetPathValue("provider", "ccbill")
	req = req.WithContext(merchant.WithID(req.Context(), merchant.ID(uuid.New())))
	rt := &app.Runtime{Config: &config.Config{
		TestMode:                 credentialPostureFromBool(testMode),
		CCBillWebhookIPAllowlist: devAllowlist,
	}}
	return httprequest.NewHTTP(w, req, rt), w
}

// #668: CCBill has no HMAC — the source-IP allowlist is its only
// authentication. The declared dev allowlist must be refused whenever a live
// CCBill PSP exists: live posture + test_mode=true must still 403 a webhook
// from a non-CCBill IP.
func TestCCBillWebhookLivePostureRejectsForgedIPDespiteTestMode(t *testing.T) {
	stubCCBillLiveProbe(t, merchants.LiveRailPresent, nil)
	r, w := newCCBillWebhookRequest(t, true, "203.0.113.5:12345")

	Webhook(r)

	require.Equal(t, http.StatusForbidden, w.Code)
}

// Sandbox posture + an explicitly declared source + a probe that PROVES no live
// CCBill PSP exists: the request proceeds to enqueue (which 500s here only
// because no River producer is wired — the point is it is NOT a 403).
func TestCCBillWebhookSandboxPostureHonorsDeclaredAllowlist(t *testing.T) {
	stubCCBillLiveProbe(t, merchants.LiveRailAbsent, nil)
	r, w := newCCBillWebhookRequest(t, true, "203.0.113.5:12345")

	Webhook(r)

	require.NotEqual(t, http.StatusForbidden, w.Code)
	require.Equal(t, http.StatusInternalServerError, w.Code) // enqueue fails: no River producer in test runtime
}

// SEC-19: an UNPROVEN probe (the shape the pre-fix code produced structurally —
// a silent empty RLS read — plus probe errors and a missing merchants service)
// must fail CLOSED, even with a declared allowlist entry and sandbox posture.
func TestCCBillWebhookUnprovenLiveProbeFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*testing.T)
	}{
		{"probe proves nothing (RLS-silent empty read)", func(t *testing.T) { stubCCBillLiveProbe(t, merchants.LiveRailUnknown, nil) }},
		{"probe errors", func(t *testing.T) { stubCCBillLiveProbe(t, merchants.LiveRailUnknown, errors.New("catalog unavailable")) }},
		{"probe errors but still claims absent", func(t *testing.T) {
			stubCCBillLiveProbe(t, merchants.LiveRailAbsent, errors.New("catalog unavailable"))
		}},
		{"no merchants service wired", func(t *testing.T) { stubCCBillNoProbe(t) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.apply(t)
			r, w := newCCBillWebhookRequest(t, true, "203.0.113.5:12345")

			Webhook(r)

			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
		})
	}
}

func TestCCBillWebhookIPAllowedMatrix(t *testing.T) {
	newReq := func(testMode bool, allowlist []string) *httprequest.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/ccbill", nil)
		return httprequest.NewHTTP(httptest.NewRecorder(), req, &app.Runtime{Config: &config.Config{
			TestMode:                 credentialPostureFromBool(testMode),
			CCBillWebhookIPAllowlist: allowlist,
		}})
	}

	// Allowlisted CCBill source IP always passes, regardless of posture.
	stubCCBillLiveProbe(t, merchants.LiveRailPresent, nil)
	require.True(t, ccbillWebhookIPAllowed(newReq(false, nil), "64.38.212.5"))
	require.True(t, ccbillWebhookIPAllowed(newReq(true, nil), "64.38.212.5"))

	// test_mode off: a non-CCBill IP is rejected even when declared.
	require.False(t, ccbillWebhookIPAllowed(newReq(false, devAllowlist), "203.0.113.5"))

	// test_mode on + live CCBill PSP declared: the declared source is refused.
	require.False(t, ccbillWebhookIPAllowed(newReq(true, devAllowlist), "203.0.113.5"))

	stubCCBillLiveProbe(t, merchants.LiveRailAbsent, nil)
	// SEC-19: sandbox + provably no live PSP is NOT enough on its own — the
	// source must be explicitly declared. An empty allowlist accepts nothing.
	require.False(t, ccbillWebhookIPAllowed(newReq(true, nil), "203.0.113.5"))
	// ...and an address outside the declared range is still refused.
	require.False(t, ccbillWebhookIPAllowed(newReq(true, devAllowlist), "198.51.100.7"))
	// All three conditions met.
	require.True(t, ccbillWebhookIPAllowed(newReq(true, devAllowlist), "203.0.113.5"))

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
// SignatureValid.
func TestCCBillWebhookMessageNeverClaimsSignatureValid(t *testing.T) {
	prepared, err := webhookutil.PrepareCCBill([]byte(`{"eventType":"NewSaleSuccess","clientAccnum":"900000","transactionId":"1"}`), "NewSaleSuccess")
	require.NoError(t, err)

	msg := ccbillWebhookMessage("64.38.212.5", prepared, "900000-0000")
	require.Nil(t, msg.SignatureValid)
	require.Equal(t, "900000-0000", msg.PspID)
}

// newCCBillWebhookRequestBehindProxy builds a webhook request that physically
// arrived through remoteAddr (the direct socket peer — e.g. a load balancer),
// carrying forwardedFor as the X-Forwarded-For header. trustedProxies configures
// the #746 resolver Runtime.TrustedProxies uses to decide whether to honor it.
func newCCBillWebhookRequestBehindProxy(t *testing.T, remoteAddr, forwardedFor string, trustedProxies []string) (*httprequest.Request, *httptest.ResponseRecorder) {
	t.Helper()
	w := httptest.NewRecorder()
	body := `{"eventType":"NewSaleSuccess","clientAccnum":"900000","clientSubacc":"0000","transactionId":"1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/ccbill?eventType=NewSaleSuccess", strings.NewReader(body))
	req.RemoteAddr = remoteAddr
	if forwardedFor != "" {
		req.Header.Set("X-Forwarded-For", forwardedFor)
	}
	req.SetPathValue("provider", "ccbill")
	req = req.WithContext(merchant.WithID(req.Context(), merchant.ID(uuid.New())))
	rt := &app.Runtime{
		Config:         &config.Config{TestMode: config.CredentialPostureLive, TrustedProxies: trustedProxies},
		TrustedProxies: iputil.ParseTrustedProxies(trustedProxies),
	}
	return httprequest.NewHTTP(w, req, rt), w
}

// #746: a CCBill webhook that physically arrives through a configured trusted
// reverse proxy (RemoteAddr = the proxy/load balancer, X-Forwarded-For =
// CCBill's real source IP) passes the IP allowlist — the resolved client IP,
// not the proxy's own socket address, is what's evaluated. Exercises the full
// Webhook() dispatch path, not just the allowlist helper in isolation.
func TestCCBillWebhookAllowsRequestBehindTrustedProxy(t *testing.T) {
	stubCCBillLiveProbe(t, merchants.LiveRailAbsent, nil)

	r, w := newCCBillWebhookRequestBehindProxy(t, "10.0.0.5:443", "64.38.212.5", []string{"10.0.0.0/8"})

	Webhook(r)

	// Reaches enqueue (which 500s here only because no River producer is
	// wired in this test runtime) instead of 403ing at the IP allowlist —
	// proof the resolved client IP (from XFF, since the proxy is trusted) was
	// evaluated, not the load balancer's own address.
	require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
}

// #746: without trusted_proxies configured, a spoofed X-Forwarded-For
// claiming to be a CCBill IP has ZERO effect — the socket peer (the actual
// sender) is what's evaluated, and an off-allowlist sender correctly 403s.
func TestCCBillWebhookRejectsSpoofedForwardedForWithoutTrustedProxies(t *testing.T) {
	stubCCBillLiveProbe(t, merchants.LiveRailAbsent, nil)

	r, w := newCCBillWebhookRequestBehindProxy(t, "203.0.113.9:443", "64.38.212.5", nil)

	Webhook(r)

	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}

// #746: the same spoof attempt against a proxy that IS in trusted_proxies but
// where the attacker IS the direct peer (no proxy involved at all) still
// 403s — trusting a CIDR range never means trusting an arbitrary claimed
// X-Forwarded-For from an untrusted direct connection outside that range.
func TestCCBillWebhookIPAllowedIgnoresForwardedForFromUntrustedPeer(t *testing.T) {
	stubCCBillLiveProbe(t, merchants.LiveRailAbsent, nil)

	r, w := newCCBillWebhookRequestBehindProxy(t, "203.0.113.9:443", "64.38.212.5", []string{"10.0.0.0/8"})

	Webhook(r)

	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}
