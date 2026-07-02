package nmi

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/openrails/config"
)

func testClient(t *testing.T, url string) *NMIClient {
	t.Helper()
	c, err := NewClient("mobius", &config.NMIProviderSettings{
		SecurityKey:   "test_security_key",
		WebhookSecret: "test_secret",
	}, true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	c.DirectPostURL = url
	c.QueryURL = url
	c.V5BaseURL = url
	return c
}

func TestTransportAmbiguity_V5Sale(t *testing.T) {
	tests := []struct {
		name          string
		handler       http.HandlerFunc
		wantAmbiguous bool
	}{
		{
			name: "5xx is ambiguous",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
			},
			wantAmbiguous: true,
		},
		{
			name: "unparseable 2xx body is ambiguous (mutation executed)",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, "not json")
			},
			wantAmbiguous: true,
		},
		{
			name: "parsed 4xx envelope is clean",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"type":"invalid_request","message":"bad params"}`)
			},
			wantAmbiguous: false,
		},
		{
			name: "parsed decline is clean",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, `{"id":"t1","response":"2","response_code":"200","response_text":"DECLINED"}`)
			},
			wantAmbiguous: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			client := testClient(t, srv.URL)
			_, err := client.RunSale(SaleParams{CustomerVaultID: "v1", Amount: 100, Currency: "USD", OrderID: "o1"})
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := IsTransportAmbiguous(err); got != tt.wantAmbiguous {
				t.Fatalf("IsTransportAmbiguous(%v) = %v, want %v", err, got, tt.wantAmbiguous)
			}
		})
	}
}

func TestTransportAmbiguity_ConnectionRefused(t *testing.T) {
	// A dead endpoint: conservative posture is ambiguous (verify resolves it
	// to verified-not-executed and the executor retries).
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	client := testClient(t, url)

	_, err := client.RunSale(SaleParams{CustomerVaultID: "v1", Amount: 100, Currency: "USD", OrderID: "o1"})
	if !IsTransportAmbiguous(err) {
		t.Fatalf("connection failure should be transport-ambiguous, got %v", err)
	}

	// Direct-post mutations too.
	_, err = client.AddRecurringSubscription(RecurringPaymentData{PlanID: "p", CustomerVaultID: "v1", Currency: "USD"})
	if !IsTransportAmbiguous(err) {
		t.Fatalf("direct-post connection failure should be transport-ambiguous, got %v", err)
	}
}

func TestTransportAmbiguity_ReadsAndGuardsStayClean(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	client := testClient(t, srv.URL)

	// GET read: never ambiguous even on 5xx.
	if _, _, err := client.GetPayment("txn-1"); err == nil || IsTransportAmbiguous(err) {
		t.Fatalf("read errors must stay clean, got %v", err)
	}

	// Read-only guard: clean (nothing was sent).
	client.ReadOnly = true
	_, err := client.RunSale(SaleParams{CustomerVaultID: "v1", Amount: 100, Currency: "USD", OrderID: "o1"})
	if !errors.Is(err, ErrProviderReadOnly) || IsTransportAmbiguous(err) {
		t.Fatalf("read-only block must be a clean non-ambiguous error, got %v", err)
	}
}
