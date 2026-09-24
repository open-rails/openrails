package webhookutil

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

func signed(scheme, secret string, at time.Time, body []byte) string {
	ts := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(body)))
	return "t=" + ts + "," + scheme + "=" + hex.EncodeToString(mac.Sum(nil))
}

func TestCanonicalRail(t *testing.T) {
	for in, want := range map[string]string{"nmi": "nmi", " NMI ": "nmi", "/stripe/": "stripe", "ccbill": "ccbill", "basistheory": "basis_theory"} {
		got, err := CanonicalRail(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	// or#893: a PSP key is not a rail; the refusal names the replacement URL.
	for in, canonical := range map[string]string{"mobius": "nmi", " MOBIUS/": "nmi", "basis_theory": "basistheory"} {
		got, err := CanonicalRail(in)
		require.Empty(t, got)
		require.ErrorIs(t, err, ErrWebhookRailRetired)
		require.ErrorContains(t, err, "/webhooks/"+canonical+"/{account_id}")
	}
}

// FC-7: no unsigned path; every refusal is typed so handlers map it to a status.
func TestPrepareSignedRails(t *testing.T) {
	const secret = "whsec_test"
	now := time.Now()
	stripeBody := []byte(`{"id":"evt_1","type":"invoice.paid"}`)
	nmiBody := []byte(`{"event_id":" evt_2 ","event_type":"transaction.sale.success"}`)

	header := signed("v1", secret, now, stripeBody)
	p, err := PrepareStripe(stripeBody, secret, header, time.Minute)
	require.NoError(t, err)
	require.Equal(t, Prepared{Rail: subscriptions.RailStripe, EventID: "evt_1", EventType: "invoice.paid", Body: stripeBody, Signature: header, SignatureVerified: true}, p)

	header = signed("s", secret, now, nmiBody)
	p, err = PrepareNMI(" NMI ", nmiBody, secret, header)
	require.NoError(t, err)
	require.Equal(t, Prepared{Rail: "nmi", EventID: "evt_2", EventType: "transaction.sale.success", Body: nmiBody, Signature: header, SignatureVerified: true}, p)

	stripe := func(body []byte, secret, header string) error {
		_, err := PrepareStripe(body, secret, header, time.Minute)
		return err
	}
	nmi := func(body []byte, secret, header string) error {
		_, err := PrepareNMI("nmi", body, secret, header)
		return err
	}
	for _, tc := range []struct {
		name    string
		prepare func([]byte, string, string) error
		body    []byte
		secret  string
		header  string
		want    error
	}{
		{"stripe no secret", stripe, stripeBody, " ", signed("v1", secret, now, stripeBody), ErrWebhookSignatureRequired},
		{"stripe no header", stripe, stripeBody, secret, " ", ErrWebhookSignatureMissing},
		{"stripe bad signature", stripe, stripeBody, secret, signed("v1", "attacker", now, stripeBody), ErrWebhookSignatureInvalid},
		{"stripe stale", stripe, stripeBody, secret, signed("v1", secret, now.Add(-time.Hour), stripeBody), ErrWebhookSignatureInvalid},
		{"stripe missing id", stripe, []byte(`{"id":"","type":"x"}`), secret, signed("v1", secret, now, []byte(`{"id":"","type":"x"}`)), ErrWebhookPayloadInvalid},
		{"nmi no secret", nmi, nmiBody, "", signed("s", secret, now, nmiBody), ErrNMIWebhookSecretMissing},
		{"nmi no header", nmi, nmiBody, secret, "", ErrNMIWebhookSignatureMissing},
		{"nmi bad signature", nmi, nmiBody, secret, signed("s", "attacker", now, nmiBody), ErrNMIWebhookSignatureInvalid},
		// FC-8: public ingestion enforces the 5 minute window.
		{"nmi outside window", nmi, nmiBody, secret, signed("s", secret, now.Add(-NMISignatureTolerance-time.Minute), nmiBody), ErrNMIWebhookSignatureInvalid},
		{"nmi missing event id", nmi, []byte(`{"event_id":" "}`), secret, signed("s", secret, now, []byte(`{"event_id":" "}`)), ErrWebhookEventIDMissing},
		{"nmi bad json", nmi, []byte(`{"event_id":`), secret, signed("s", secret, now, []byte(`{"event_id":`)), ErrWebhookPayloadInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorIs(t, tc.prepare(tc.body, tc.secret, tc.header), tc.want)
		})
	}
	// The queued re-verify path keeps HMAC integrity but drops the window.
	old := signed("s", secret, now.Add(-24*time.Hour), nmiBody)
	require.NoError(t, VerifyNMISignatureWithTolerance(secret, old, nmiBody, 0))
	require.Error(t, VerifyNMISignatureWithTolerance(secret, old, []byte("{}"), 0))
}

func TestPrepareCCBill(t *testing.T) {
	p, err := PrepareCCBill([]byte(" eventType=NewSaleSuccess&subscriptionId=123&subscriptionId=456 "), " NewSaleSuccess ")
	require.NoError(t, err)
	require.Equal(t, subscriptions.RailCCBill, p.Rail)
	require.Equal(t, "NewSaleSuccess", p.EventType)
	require.False(t, p.SignatureVerified, "CCBill signs nothing; its gate is the source IP")
	require.JSONEq(t, `{"eventType":"NewSaleSuccess","subscriptionId":"123"}`, string(p.Body))

	p, err = PrepareCCBill([]byte(`{"eventType":"renewalsuccess"}`), "RenewalSuccess")
	require.NoError(t, err)
	require.JSONEq(t, `{"eventType":"renewalsuccess"}`, string(p.Body))

	for _, tc := range []struct {
		body, eventType string
		want            error
	}{
		{"bad=%zz", "NewSaleSuccess", ErrWebhookPayloadInvalid},
		{`{"ok":true}`, " ", ErrWebhookEventTypeMissing},
		{`{"eventType":"Cancellation"}`, "RenewalSuccess", ErrWebhookEventTypeMismatch},
		{`[1]`, "RenewalSuccess", ErrWebhookPayloadInvalid},
	} {
		_, err := PrepareCCBill([]byte(tc.body), tc.eventType)
		require.ErrorIs(t, err, tc.want, tc.body)
	}
}

// IDEM-11: the provider event id is the dedup identity; without one, the key is
// a stable hash of rail, type and body.
func TestComputeUniqueKey(t *testing.T) {
	require.Equal(t, "webhook:nmi:evt_1", ComputeUniqueKey(" NMI ", " evt_1 ", "a", []byte(`{}`)))
	require.Equal(t, ComputeUniqueKey("nmi", "evt_1", "a", []byte(`{}`)), ComputeUniqueKey("nmi", "evt_1", "b", []byte(`{"x":1}`)))
	hashed := ComputeUniqueKey("ccbill", "", "NewSaleSuccess", []byte(`{"a":1}`))
	require.Regexp(t, `^webhook:ccbill:[0-9a-f]{16}$`, hashed)
	require.Equal(t, hashed, ComputeUniqueKey("ccbill", "", "NewSaleSuccess", []byte(`{"a":1}`)))
	require.NotEqual(t, hashed, ComputeUniqueKey("ccbill", "", "NewSaleSuccess", []byte(`{"a":2}`)))
	require.NotEqual(t, hashed, ComputeUniqueKey("ccbill", "", "Cancellation", []byte(`{"a":1}`)))
	require.NotEqual(t, hashed, ComputeUniqueKey("nmi", "", "NewSaleSuccess", []byte(`{"a":1}`)))
}
