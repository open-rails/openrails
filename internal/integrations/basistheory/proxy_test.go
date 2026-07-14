package basistheory

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestClassifyProxyResponse pins the three-way outcome discrimination matrix.
func TestClassifyProxyResponse(t *testing.T) {
	nmiApproved := `response=1&responsetext=SUCCESS&transactionid=1234567&response_code=100`
	nmiDeclined := `response=2&responsetext=DECLINE&transactionid=7654321&response_code=200`
	btAuthError := `{"proxy_error":{"errors":{"error":["The BT-API-KEY header is required"]},"title":"One or more validation errors occurred.","status":401,"detail":"Unauthorized"}}`

	t.Run("destination answered: approval body passes through verbatim", func(t *testing.T) {
		res, err := classifyProxyResponse(200, "200", []byte(nmiApproved))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.DestinationStatus != 200 || string(res.Body) != nmiApproved {
			t.Fatalf("unexpected result: %+v", res)
		}
	})

	t.Run("destination answered: NMI decline is a result, not an error", func(t *testing.T) {
		res, err := classifyProxyResponse(200, "200", []byte(nmiDeclined))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.DestinationStatus != 200 {
			t.Fatalf("unexpected result: %+v", res)
		}
	})

	t.Run("destination answered with non-200 destination status still a result", func(t *testing.T) {
		res, err := classifyProxyResponse(502, "500", []byte("oops"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.DestinationStatus != 500 {
			t.Fatalf("unexpected result: %+v", res)
		}
	})

	t.Run("BT 401 proxy_error, no destination header: clean pre-forward error, not a decline, not ambiguous", func(t *testing.T) {
		_, err := classifyProxyResponse(401, "", []byte(btAuthError))
		pe, ok := IsBTProxyError(err)
		if !ok {
			t.Fatalf("want ProxyError, got %v", err)
		}
		if pe.Status != 401 || pe.Title == "" {
			t.Fatalf("unexpected proxy error: %+v", pe)
		}
		if IsTransportAmbiguous(err) {
			t.Fatal("pre-forward BT failure must not be ambiguous")
		}
	})

	t.Run("4xx without proxy_error body: still a clean BT error", func(t *testing.T) {
		_, err := classifyProxyResponse(400, "", []byte("bad request"))
		if _, ok := IsBTProxyError(err); !ok {
			t.Fatalf("want ProxyError, got %v", err)
		}
		if IsTransportAmbiguous(err) {
			t.Fatal("clean 4xx must not be ambiguous")
		}
	})

	t.Run("408 without destination header is ambiguous", func(t *testing.T) {
		_, err := classifyProxyResponse(408, "", nil)
		if !IsTransportAmbiguous(err) {
			t.Fatalf("want ambiguous, got %v", err)
		}
	})

	t.Run("5xx without destination header is ambiguous", func(t *testing.T) {
		_, err := classifyProxyResponse(502, "", []byte("bad gateway"))
		if !IsTransportAmbiguous(err) {
			t.Fatalf("want ambiguous, got %v", err)
		}
	})

	t.Run("unparseable destination status header is ambiguous", func(t *testing.T) {
		_, err := classifyProxyResponse(200, "abc", nil)
		if !IsTransportAmbiguous(err) {
			t.Fatalf("want ambiguous, got %v", err)
		}
	})
}

// TestProxyFormWire pins the proxy request wire shape: headers + urlencoded
// body with detokenization expressions passed through byte-exact.
func TestProxyFormWire(t *testing.T) {
	var got struct {
		method, proxyURL, apiKey, contentType, body, idemKey string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		got.method = r.Method
		got.proxyURL = r.Header.Get("BT-PROXY-URL")
		got.apiKey = r.Header.Get("BT-API-KEY")
		got.contentType = r.Header.Get("Content-Type")
		got.idemKey = r.Header.Get("BT-IDEMPOTENCY-KEY")
		got.body = string(b)
		w.Header().Set(ProxyDestinationStatusHeader, "200")
		_, _ = w.Write([]byte("response=1&transactionid=42"))
	}))
	defer srv.Close()

	c, err := New(Config{APIKey: "key_test", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{}
	form.Set("type", "sale")
	form.Set("ccnumber", `{{ token: 3fa85f64-5717-4562-b3fc-2c963f66afa6 | json: "$.data.number" }}`)
	res, err := c.ProxyForm(context.Background(), "https://secure.networkmerchants.com/api/transact.php", form)
	if err != nil {
		t.Fatalf("proxy: %v", err)
	}
	if res.DestinationStatus != 200 {
		t.Fatalf("destination status: %d", res.DestinationStatus)
	}
	if got.method != http.MethodPost {
		t.Fatalf("method: %s", got.method)
	}
	if got.proxyURL != "https://secure.networkmerchants.com/api/transact.php" {
		t.Fatalf("BT-PROXY-URL: %q", got.proxyURL)
	}
	if got.apiKey != "key_test" {
		t.Fatalf("BT-API-KEY: %q", got.apiKey)
	}
	if got.contentType != "application/x-www-form-urlencoded" {
		t.Fatalf("content type: %q", got.contentType)
	}
	// Proxy explicitly does NOT support idempotency — assert nothing leaks.
	if got.idemKey != "" {
		t.Fatalf("proxy must not send BT-IDEMPOTENCY-KEY, got %q", got.idemKey)
	}
	wantBody := "ccnumber=" + url.QueryEscape(`{{ token: 3fa85f64-5717-4562-b3fc-2c963f66afa6 | json: "$.data.number" }}`) + "&type=sale"
	if got.body != wantBody {
		t.Fatalf("body:\n got %q\nwant %q", got.body, wantBody)
	}
}

func TestProxyTransportFailureIsAmbiguous(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		_ = conn.Close() // connection reset mid-request
	}))
	defer srv.Close()
	c, err := New(Config{APIKey: "key_test", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ProxyForm(context.Background(), "https://example.test/transact", url.Values{"type": {"sale"}})
	if !IsTransportAmbiguous(err) {
		t.Fatalf("want ambiguous transport error, got %v", err)
	}
}

func TestReadOnlyBlocksWritesLocally(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("readonly client must not reach the network on writes")
	}))
	defer srv.Close()
	c, err := New(Config{APIKey: "key_test", BaseURL: srv.URL, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ProxyForm(context.Background(), "https://example.test/transact", url.Values{})
	if !errors.Is(err, ErrProviderReadOnly) {
		t.Fatalf("want ErrProviderReadOnly, got %v", err)
	}
	if IsTransportAmbiguous(err) {
		t.Fatal("readonly rejection is clean, never ambiguous")
	}
	_, err = c.ConvertTokenIntent(context.Background(), "ti_1", ConvertOpts{})
	if !errors.Is(err, ErrProviderReadOnly) {
		t.Fatalf("want ErrProviderReadOnly, got %v", err)
	}
}

func TestIdempotencyKeyOnTokenWrites(t *testing.T) {
	var gotKey, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("BT-IDEMPOTENCY-KEY")
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"id":"tok_1","type":"card"}`))
	}))
	defer srv.Close()
	c, err := New(Config{APIKey: "key_test", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ConvertTokenIntent(context.Background(), "ti_1", ConvertOpts{IdempotencyKey: "intent-row-123", Deduplicate: true}); err != nil {
		t.Fatal(err)
	}
	if gotKey != "intent-row-123" || gotPath != "/tokens" {
		t.Fatalf("idempotency key %q path %q", gotKey, gotPath)
	}
	if _, err := c.CreateNetworkToken(context.Background(), NetworkTokenRequest{TokenID: "tok_1", IdempotencyKey: "nt-key-1"}); err != nil {
		t.Fatal(err)
	}
	if gotKey != "nt-key-1" || gotPath != "/network-tokens" {
		t.Fatalf("idempotency key %q path %q", gotKey, gotPath)
	}
}

func TestExpiredIntentIsLoudNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"title":"Not Found","status":404}`))
	}))
	defer srv.Close()
	c, err := New(Config{APIKey: "key_test", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.GetTokenIntent(context.Background(), "expired-intent")
	if !IsNotFound(err) {
		t.Fatalf("want loud not-found, got %v", err)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("error should carry status: %v", err)
	}
}
