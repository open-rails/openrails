package nmi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	c.LoopbackFixture = true
	return c
}

func TestTransportAmbiguity_StoredCredentialSale(t *testing.T) {
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
			name: "HTTP 4xx after direct-post send is ambiguous",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, "response=3&responsetext=bad+params&response_code=300")
			},
			wantAmbiguous: true,
		},
		{
			name: "parsed decline is clean",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, "response=2&response_code=200&responsetext=DECLINED")
			},
			wantAmbiguous: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			client := testClient(t, srv.URL)
			_, err := client.RunSale(context.Background(), SaleParams{CustomerVaultID: "v1", Amount: 100, Currency: "USD", OrderID: "o1", StoredCredential: testInitialOneTimeCredential()})
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

	_, err := client.RunSale(context.Background(), SaleParams{CustomerVaultID: "v1", Amount: 100, Currency: "USD", OrderID: "o1", StoredCredential: testInitialOneTimeCredential()})
	if !IsTransportAmbiguous(err) {
		t.Fatalf("connection failure should be transport-ambiguous, got %v", err)
	}

	// Direct-post mutations too.
	_, err = client.AddRecurringSubscription(context.Background(), RecurringPaymentData{PlanID: "p", CustomerVaultID: "v1", Currency: "USD", StoredCredential: testInitialRecurringCredential()})
	if !IsTransportAmbiguous(err) {
		t.Fatalf("direct-post connection failure should be transport-ambiguous, got %v", err)
	}
}

func TestCreateCustomerVaultTimeoutIsBoundedAndAmbiguous(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	client := testClient(t, srv.URL)
	client.httpClient.Timeout = 20 * time.Millisecond
	started := time.Now()
	_, err := client.CreateCustomerVault(context.Background(), CreateCustomerVaultData{PaymentToken: "tok_test"})

	if !IsTransportAmbiguous(err) {
		t.Fatalf("timed-out vault create should be transport-ambiguous, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("vault create timeout took %s", elapsed)
	}
}

func TestTransportAmbiguity_ReadsAndGuardsStayClean(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	client := testClient(t, srv.URL)

	// GET read: never ambiguous even on 5xx.
	if _, _, err := client.GetPayment(context.Background(), "txn-1"); err == nil || IsTransportAmbiguous(err) {
		t.Fatalf("read errors must stay clean, got %v", err)
	}

	// Read-only guard: clean (nothing was sent).
	client.ReadOnly = true
	_, err := client.RunSale(context.Background(), SaleParams{CustomerVaultID: "v1", Amount: 100, Currency: "USD", OrderID: "o1", StoredCredential: testInitialOneTimeCredential()})
	if !errors.Is(err, ErrProviderReadOnly) || IsTransportAmbiguous(err) {
		t.Fatalf("read-only block must be a clean non-ambiguous error, got %v", err)
	}
}

// All classic financial callers share the same response discriminator. A
// contradictory or incomplete reply is uncertainty after one POST, never
// authority to record a decline or send the operation again.
func TestDirectFinancialResponseFactsAndDiagnostics(t *testing.T) {
	const sentinel = "RAW_PROVIDER_SENTINEL"
	cases := []struct {
		name, body string
		declined   bool
	}{
		{"qualified refusal", "response=2&response_code=202&responsetext=" + sentinel, true},
		{"communication error", "response=3&response_code=420&responsetext=" + sentinel, false},
		{"unqualified error", "response=3&response_code=400&response_message=" + sentinel, false},
		{"contradictory discriminators", "response=2&response=1&response_code=202&response_code=100", false},
		{"repeated equal discriminator", "response=2&response=2&response_code=202", false},
		{"repeated equal code", "response=2&response_code=202&response_code=202", false},
		{"contradictory approval", "response=1&response_code=202&transactionid=one", false},
		{"contradictory refusal", "response=2&response_code=100", false},
		{"missing refusal code", "response=2&responsetext=" + sentinel, false},
		{"invalid refusal code", "response=2&response_code=invalid&responsetext=" + sentinel, false},
		{"invalid encoding", "response=2&response_code=202&responsetext=%Q" + sentinel, false},
		{"duplicate payment identity", "response=1&response_code=100&transactionid=one&transactionid=two", false},
		{"duplicate schedule identity", "response=1&response_code=100&subscription_id=one&subscription_id=two", false},
		{"duplicate amount", "response=1&response_code=100&amount=1.00&amount=2.00", false},
		{"duplicate currency", "response=1&response_code=100&currency=USD&currency=JPY", false},
	}
	for _, caller := range []string{"sale", "initial", "rebill"} {
		for _, tc := range cases {
			t.Run(caller+"/"+tc.name, func(t *testing.T) {
				posts := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					posts++
					fmt.Fprint(w, tc.body)
				}))
				defer server.Close()
				client := testClient(t, server.URL)
				var err error
				var diagnostic, raw string
				declined := false
				switch caller {
				case "sale":
					_, err = client.RunSale(t.Context(), SaleParams{CustomerVaultID: "vault", BillingID: "card", Amount: 999, Currency: "USD", OrderID: "accepted-operation", StoredCredential: testInitialOneTimeCredential()})
				case "initial":
					_, err = client.AddRecurringSubscription(t.Context(), RecurringPaymentData{CustomerVaultID: "vault", BillingID: "card", PlanID: "plan", Amount: 999, Currency: "USD", OrderID: "accepted-operation", StoredCredential: testInitialRecurringCredential()})
				case "rebill":
					var result *ManualRebillResponse
					result, err = client.AttemptManualRebill(t.Context(), ManualRebillParams{VaultID: "vault", BillingID: "card", SubscriptionID: "subscription", OrderID: "accepted-operation", StoredCredential: &StoredCredential{Recurring: true, InitiatedBy: InitiatedByMerchant, Indicator: IndicatorUsed, InitialTransactionID: "original-charge"}})
					if result != nil {
						declined, diagnostic = result.Declined, result.ErrorMessage
					}
				}
				if err != nil {
					diagnostic += err.Error()
					var refusal *CustomerVaultError
					if errors.As(err, &refusal) {
						if !tc.declined {
							t.Error("unknown response exposed typed decline proof")
						}
						declined = !RequiresVerification(err)
						raw = refusal.RawResponse
					}
				}
				if declined != tc.declined {
					t.Errorf("declined=%v want %v", declined, tc.declined)
				}
				if !tc.declined && !RequiresVerification(err) {
					t.Errorf("unqualified response must require verification, got %v", err)
				}
				if strings.Contains(diagnostic, sentinel) {
					t.Errorf("opaque provider text escaped through diagnostic: %s", diagnostic)
				}
				if tc.declined && caller != "rebill" && raw != tc.body {
					t.Error("qualified refusal must retain its raw proof internally")
				}
				if posts != 1 {
					t.Errorf("financial POST count=%d want1", posts)
				}
			})
		}
	}
}

func TestV5UnknownDiagnosticCannotBecomeDeclineProof(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"refund","response":"3","response_code":"420","response_text":"RAW_PROVIDER_SENTINEL"}`)
	}))
	defer server.Close()
	_, err := testClient(t, server.URL).Refund(t.Context(), RefundParams{TransactionID: "original", Amount: 999, Currency: "USD"})
	var response *CustomerVaultError
	if errors.As(err, &response) || !RequiresVerification(err) {
		t.Fatalf("uncertainty must not carry typed decline proof: %v", err)
	}
	if strings.Contains(err.Error(), "RAW_PROVIDER_SENTINEL") {
		t.Fatal("opaque provider text escaped")
	}
}

func TestV5HTTPDiagnosticsOmitProviderEnvelope(t *testing.T) {
	for _, status := range []int{400, 404, 502} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts++
				w.WriteHeader(status)
				fmt.Fprint(w, `{"type":"RAW_PROVIDER_SENTINEL","error_code":"RAW_PROVIDER_SENTINEL","message":"RAW_PROVIDER_SENTINEL"}`)
			}))
			defer server.Close()
			_, err := testClient(t, server.URL).Refund(t.Context(), RefundParams{TransactionID: "original", Amount: 999, Currency: "USD"})
			if err == nil || strings.Contains(err.Error(), "RAW_PROVIDER_SENTINEL") {
				t.Fatalf("unsafe diagnostic: %v", err)
			}
			if status >= 500 && !RequiresVerification(err) {
				t.Fatalf("lost refund must require verification: %v", err)
			}
			if posts != 1 {
				t.Fatalf("posts=%d want 1", posts)
			}
		})
	}
}
