package basistheory

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/open-rails/openrails/internal/shared/httpx"
)

func newTestClient(t *testing.T, cfg Config, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg.APIKey, cfg.BaseURL = "key_test", srv.URL
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

// or#795: unknown codes are UNRECOGNIZED (never guessed); UPD_ matches by prefix so a
// future variant is still applied.
func TestClassifyAccountUpdaterResult(t *testing.T) {
	for code, want := range map[string]AUOutcome{
		AUUpdatedPAN: AUOutcomeUpdated, AUUpdatedExpDate: AUOutcomeUpdated,
		"upd_pan": AUOutcomeUpdated, " UPD_PAN_EXP_DATE ": AUOutcomeUpdated,
		AUNoUpdate: AUOutcomeNoChange, AUNoMatch: AUOutcomeNoChange, "": AUOutcomeNoChange,
		AUClosedAccount: AUOutcomeClosed, AUContactCardholder: AUOutcomeContactCardholder,
		"WRN_SOMETHING_NEW": AUOutcomeUnrecognized, "nonsense": AUOutcomeUnrecognized,
	} {
		if got := ClassifyAccountUpdaterResult(code); got != want {
			t.Errorf("Classify(%q) = %q, want %q", code, got, want)
		}
	}
}

func TestParseAccountUpdaterResultsByHeaderName(t *testing.T) {
	rows, err := ParseAccountUpdaterResults(strings.NewReader(
		"result_code,new_token,Token,new_last4\nUPD_PAN, tok_new ,tok_old,4242\nNO_MATCH,,tok_2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[1].Token != "tok_2" || rows[0] != (AccountUpdaterResultRow{Token: "tok_old", NewToken: "tok_new", ResultCode: "UPD_PAN", NewLast4: "4242"}) {
		t.Fatalf("rows = %+v", rows)
	}
	if _, err := ParseAccountUpdaterResults(strings.NewReader("result_code\nUPD_PAN\n")); err == nil {
		t.Fatal("csv without a token column must be refused")
	}
}

// SEC-24: provider-supplied upload/download URLs must not reach internal hosts.
func TestAccountUpdaterURLsAreOutboundPolicyChecked(t *testing.T) {
	var hits atomic.Int32
	var gotMethod, gotType, gotBody string
	h := func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		gotMethod, gotType, gotBody = r.Method, r.Header.Get("Content-Type"), string(b)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("token,result_code\ntok_1,UPD_EXP_DATE\n"))
		}
	}
	rows := []AccountUpdaterRequestRow{{Token: "tok_1", ExpirationYear: "2030", ExpirationMonth: "01"}}

	strict, srv := newTestClient(t, Config{}, h)
	if err := strict.UploadAccountUpdaterCSV(context.Background(), srv.URL+"/up", rows); !errors.Is(err, httpx.ErrBlockedAddress) {
		t.Fatalf("strict upload err = %v", err)
	}
	if _, err := strict.DownloadAccountUpdaterResults(context.Background(), srv.URL+"/down"); !errors.Is(err, httpx.ErrBlockedAddress) {
		t.Fatalf("strict download err = %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("refused URLs must not be dialed")
	}

	c, srv := newTestClient(t, Config{Outbound: httpx.Policy{Allow: httpx.AllowLoopback}}, h)
	if err := c.UploadAccountUpdaterCSV(context.Background(), srv.URL+"/up", rows); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPut || gotType != "text/csv" ||
		gotBody != "token,expiration_year,expiration_month,merchant_id\ntok_1,2030,01,\n" {
		t.Fatalf("upload wire %s %s %q", gotMethod, gotType, gotBody)
	}
	res, err := c.DownloadAccountUpdaterResults(context.Background(), srv.URL+"/down")
	if err != nil || len(res) != 1 || ClassifyAccountUpdaterResult(res[0].ResultCode) != AUOutcomeUpdated {
		t.Fatalf("download = %+v, %v", res, err)
	}
}

// The proxy outcome is three-way: destination answered (result, even a decline),
// clean pre-forward BT error, or ambiguous (may have forwarded; never retry blind).
func TestClassifyProxyResponse(t *testing.T) {
	const approved = "response=1&responsetext=SUCCESS&transactionid=1234567"
	const btAuth = `{"proxy_error":{"errors":{"error":["The BT-API-KEY header is required"]},"title":"One or more validation errors occurred.","status":401,"detail":"Unauthorized"}}`
	const (
		result = iota
		clean
		ambig
	)
	for _, tc := range []struct {
		name       string
		status     int
		dest, body string
		want       int
		wantDest   int
	}{
		{"approval passes through", 200, "200", approved, result, 200},
		{"NMI decline is a result", 200, "200", "response=2&responsetext=DECLINE", result, 200},
		{"destination 500 is still a result", 502, " 500 ", "oops", result, 500},
		{"BT proxy_error envelope", 401, "", btAuth, clean, 0},
		{"bare 4xx", 400, "", "bad request", clean, 0},
		{"408 no header", 408, "", "", ambig, 0},
		{"5xx no header", 502, "", "bad gateway", ambig, 0},
		{"unparseable header", 200, "abc", "", ambig, 0},
		{"zero header", 200, "0", "", ambig, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := classifyProxyResponse(tc.status, tc.dest, []byte(tc.body))
			switch tc.want {
			case result:
				if err != nil || res.DestinationStatus != tc.wantDest || string(res.Body) != tc.body {
					t.Fatalf("res=%+v err=%v", res, err)
				}
			case clean:
				pe, ok := IsBTProxyError(err)
				if !ok || IsTransportAmbiguous(err) || pe.Status != tc.status {
					t.Fatalf("want clean ProxyError(%d), got %v", tc.status, err)
				}
			case ambig:
				if _, ok := IsBTProxyError(err); ok || !IsTransportAmbiguous(err) {
					t.Fatalf("want ambiguous, got %v", err)
				}
			}
		})
	}
	_, err := classifyProxyResponse(401, "", []byte(btAuth))
	if pe, _ := IsBTProxyError(err); pe.Title == "" || pe.Detail != "Unauthorized" || len(pe.Errors["error"]) != 1 {
		t.Fatalf("envelope not parsed: %+v", pe)
	}
}

func TestProxyFormWire(t *testing.T) {
	const dest = "https://secure.networkmerchants.com/api/transact.php"
	const expr = `{{ token: 3fa85f64-5717-4562-b3fc-2c963f66afa6 | json: "$.data.number" }}`
	var r0 *http.Request
	var body string
	c, _ := newTestClient(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		r0, body = r, string(b)
		w.Header().Set(ProxyDestinationStatusHeader, "200")
		_, _ = w.Write([]byte("response=1&transactionid=42"))
	})
	res, err := c.ProxyForm(context.Background(), dest, url.Values{"type": {"sale"}, "ccnumber": {expr}})
	if err != nil || res.DestinationStatus != 200 || string(res.Body) != "response=1&transactionid=42" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if r0.Method != http.MethodPost || r0.URL.Path != "/proxy" || r0.Header.Get("BT-PROXY-URL") != dest ||
		r0.Header.Get("BT-API-KEY") != "key_test" || r0.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		t.Fatalf("wire: %s %s %v", r0.Method, r0.URL.Path, r0.Header)
	}
	// The proxy does not support idempotency; a key would suggest retry safety it lacks.
	if k := r0.Header.Get("BT-IDEMPOTENCY-KEY"); k != "" {
		t.Fatalf("proxy sent idempotency key %q", k)
	}
	if want := "ccnumber=" + url.QueryEscape(expr) + "&type=sale"; body != want {
		t.Fatalf("body %q want %q", body, want)
	}
	if _, err := c.ProxyForm(context.Background(), " ", url.Values{}); err == nil || IsTransportAmbiguous(err) {
		t.Fatalf("blank destination must fail cleanly, got %v", err)
	}
}

func TestProxyTransportFailureIsAmbiguous(t *testing.T) {
	c, _ := newTestClient(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	})
	_, err := c.ProxyForm(context.Background(), "https://example.test/transact", url.Values{"type": {"sale"}})
	if !IsTransportAmbiguous(err) {
		t.Fatalf("want ambiguous, got %v", err)
	}
}

func TestReadOnlyBlocksWritesBeforeNetwork(t *testing.T) {
	var writes atomic.Int32
	c, _ := newTestClient(t, Config{ReadOnly: true}, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
		_, _ = w.Write([]byte(`{"id":"ti_1","type":"card"}`))
	})
	ctx := context.Background()
	_, err := c.ProxyForm(ctx, "https://example.test/transact", url.Values{})
	if !errors.Is(err, ErrProviderReadOnly) || IsTransportAmbiguous(err) {
		t.Fatalf("proxy under readonly: %v", err)
	}
	for name, fn := range map[string]func() error{
		"convert": func() error { _, err := c.ConvertTokenIntent(ctx, "ti_1", ConvertOpts{}); return err },
		"delete":  func() error { return c.DeleteToken(ctx, "tok_1") },
		"nt":      func() error { _, err := c.CreateNetworkToken(ctx, NetworkTokenRequest{TokenID: "tok_1"}); return err },
		"au job":  func() error { _, err := c.CreateAccountUpdaterJob(ctx, "k"); return err },
	} {
		if err := fn(); !errors.Is(err, ErrProviderReadOnly) {
			t.Errorf("%s: want ErrProviderReadOnly, got %v", name, err)
		}
	}
	if writes.Load() != 0 {
		t.Fatal("readonly writes reached the network")
	}
	if ti, err := c.GetTokenIntent(ctx, "ti_1"); err != nil || ti.ID != "ti_1" {
		t.Fatalf("reads must pass under readonly: %+v %v", ti, err)
	}
}

func TestTokenWritesCarryIdempotencyKey(t *testing.T) {
	var gotKey, gotPath, gotBody string
	c, _ := newTestClient(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotKey, gotPath, gotBody = r.Header.Get("BT-IDEMPOTENCY-KEY"), r.URL.Path, string(b)
		_, _ = w.Write([]byte(`{"id":"tok_1","type":"card","cryptogram":"c"}`))
	})
	ctx := context.Background()
	if _, err := c.ConvertTokenIntent(ctx, "ti_1", ConvertOpts{IdempotencyKey: "intent-row-123", Deduplicate: true}); err != nil {
		t.Fatal(err)
	}
	if gotKey != "intent-row-123" || gotPath != "/tokens" || gotBody != `{"deduplicate_token":true,"token_intent_id":"ti_1"}` {
		t.Fatalf("convert: key %q path %q body %s", gotKey, gotPath, gotBody)
	}
	if _, err := c.CreateNetworkToken(ctx, NetworkTokenRequest{TokenID: "tok_1", IdempotencyKey: "nt-key-1"}); err != nil {
		t.Fatal(err)
	}
	if gotKey != "nt-key-1" || gotPath != "/network-tokens" || gotBody != `{"token_id":"tok_1"}` {
		t.Fatalf("network token: key %q path %q body %s", gotKey, gotPath, gotBody)
	}
	if _, err := c.CreateCryptogram(ctx, "nt_1"); err != nil || gotKey != "" || gotPath != "/network-tokens/nt_1/cryptogram" {
		t.Fatalf("cryptogram is single-use and never keyed: key %q path %q err %v", gotKey, gotPath, err)
	}
}

func TestExpiredIntentIsLoudNotFound(t *testing.T) {
	c, _ := newTestClient(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"title":"Not Found","status":404}`))
	})
	_, err := c.GetTokenIntent(context.Background(), "expired-intent")
	if !IsNotFound(err) || !strings.Contains(err.Error(), "404") {
		t.Fatalf("want loud 404, got %v", err)
	}
}

func signPSS(t *testing.T, key *rsa.PrivateKey, body []byte) string {
	t.Helper()
	digest := sha256.Sum256(body)
	sig, err := rsa.SignPSS(rand.Reader, key, crypto.SHA256, digest[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: crypto.SHA256})
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(sig)
}

func TestWebhookVerifier(t *testing.T) {
	oldKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	newKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	var current atomic.Pointer[rsa.PublicKey]
	current.Store(&oldKey.PublicKey)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		der, _ := x509.MarshalPKIXPublicKey(current.Load())
		_, _ = w.Write(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	}))
	defer srv.Close()
	v := NewWebhookVerifier(srv.URL)
	ctx := context.Background()
	body := []byte(`{"id":"evt_1","type":"token.deleted","data":{"token":{"id":"tok_1"}}}`)
	tampered := []byte(strings.Replace(string(body), "tok_1", "tok_X", 1))

	for _, tc := range []struct {
		name         string
		body         []byte
		sig, version string
		want         error
	}{
		{"valid", body, signPSS(t, oldKey, body), "v1", nil},
		{"version header optional", body, signPSS(t, oldKey, body), "", nil},
		{"tampered body", tampered, signPSS(t, oldKey, body), "v1", ErrWebhookSignatureInvalid},
		{"unknown version", body, signPSS(t, oldKey, body), "v9", ErrWebhookVersionUnknown},
		{"missing signature", body, " ", "v1", ErrWebhookSignatureMissing},
		{"not base64", body, "!!!", "v1", ErrWebhookSignatureInvalid},
		{"wrong key", body, signPSS(t, newKey, body), "v1", ErrWebhookSignatureInvalid},
	} {
		if err := v.Verify(ctx, tc.body, tc.sig, tc.version); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v want %v", tc.name, err, tc.want)
		}
	}

	current.Store(&newKey.PublicKey) // CDN rotates: next mismatch refetches once and self-heals
	if err := v.Verify(ctx, body, signPSS(t, newKey, body), "v1"); err != nil {
		t.Fatalf("post-rotation verify: %v", err)
	}
	if err := v.Verify(ctx, body, signPSS(t, oldKey, body), "v1"); !errors.Is(err, ErrWebhookSignatureInvalid) {
		t.Fatalf("retired key must fail: %v", err)
	}
}
