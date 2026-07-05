package openrails

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRemoteTrustLevelWireNames(t *testing.T) {
	var settingsBody map[string]any
	var admissionsBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/merchant/settings":
			if err := json.NewDecoder(r.Body).Decode(&settingsBody); err != nil {
				t.Fatalf("decode settings body: %v", err)
			}
			_, _ = w.Write([]byte(`{}`))
		case "/v1/merchant/admissions":
			if err := json.NewDecoder(r.Body).Decode(&admissionsBody); err != nil {
				t.Fatalf("decode admissions body: %v", err)
			}
			_, _ = w.Write([]byte(`{"items":[{"status":200,"result":{"allowed":true}}]}`))
		case "/v1/merchant/trust-level":
			_, _ = w.Write([]byte(`{"currency":"USD","trust_level":"gold"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := NewRemote(srv.URL, WithTokenProvider(func(context.Context) (string, error) {
		return "test-token", nil
	}))

	if err := client.SetMerchantSettings(context.Background(), MerchantSettings{
		TrustLevelSpendLimits: []PayerSpendLimitInput{{TrustLevel: "gold"}},
	}); err != nil {
		t.Fatalf("SetMerchantSettings: %v", err)
	}
	policies, ok := settingsBody["trust_level_spend_limits"].([]any)
	if !ok || len(policies) != 1 {
		t.Fatalf("expected one trust_level_spend_limits item, got %#v", settingsBody)
	}
	policy, _ := policies[0].(map[string]any)
	if policy["trust_level"] != "gold" {
		t.Fatalf("expected trust_level inside settings document, got %#v", settingsBody)
	}

	if _, err := client.AdmitBatch(context.Background(), []AdmitRequest{{
		CustomerID: "cust_1", TrustLevel: "gold", EstimatedAmount: 1, RequestID: "req_1",
	}}); err != nil {
		t.Fatalf("AdmitBatch: %v", err)
	}
	items, ok := admissionsBody["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("expected one admissions item, got %#v", admissionsBody)
	}
	admitBody, _ := items[0].(map[string]any)
	if admitBody["trust_level"] != "gold" {
		t.Fatalf("expected trust_level on admission item, got %#v", admissionsBody)
	}

	trustLevel, err := client.GetTrustLevel(context.Background(), "cust_1", "USD")
	if err != nil {
		t.Fatalf("GetTrustLevel: %v", err)
	}
	if trustLevel != "gold" {
		t.Fatalf("expected trust_level response to decode, got %q", trustLevel)
	}
}

// TestRemoteValidationErrorParity asserts that client-side pre-flight argument
// checks return typed *StatusError values whose errors.Is chain includes
// ErrInvalid — matching the embedded transport's behavior (#338).
// No live server is required; validation fires before any HTTP call.
func TestRemoteValidationErrorParity(t *testing.T) {
	// Use an unreachable URL — none of the tested calls should reach the network.
	client := NewRemote("http://127.0.0.1:0", WithTokenProvider(func(context.Context) (string, error) {
		return "test-token", nil
	}))

	ctx := context.Background()

	tests := []struct {
		name string
		fn   func() error
	}{
		{
			name: "Capture empty request_id",
			fn: func() error {
				return client.Capture(ctx, "", 100, nil)
			},
		},
		{
			name: "Release empty request_id",
			fn: func() error {
				return client.Release(ctx, "")
			},
		},
		{
			name: "SetCustomerSpendDelegations empty customer_id",
			fn: func() error {
				return client.SetCustomerSpendDelegations(ctx, "", nil)
			},
		},
		{
			name: "ListEntitlements empty subject",
			fn: func() error {
				_, err := client.ListEntitlements(ctx, "", time.Time{})
				return err
			},
		},
		{
			name: "HasEntitlement empty entitlement",
			fn: func() error {
				_, err := client.HasEntitlement(ctx, "user@example.com", "", time.Time{})
				return err
			},
		},
		{
			name: "ListCustomersWithEntitlement empty entitlement",
			fn: func() error {
				_, err := client.ListCustomersWithEntitlement(ctx, "", time.Time{})
				return err
			},
		},
		{
			name: "ListProductAccess empty subject",
			fn: func() error {
				_, err := client.ListProductAccess(ctx, "")
				return err
			},
		},
		{
			name: "HasProductAccess empty subject",
			fn: func() error {
				_, err := client.HasProductAccess(ctx, "", "pro")
				return err
			},
		},
		{
			name: "HasProductAccess empty product_id",
			fn: func() error {
				_, err := client.HasProductAccess(ctx, "user@example.com", "")
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.fn()
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("errors.Is(err, ErrInvalid) = false; got %T: %v", err, err)
			}
		})
	}
}

// TestWithAPIKeyAndVerify checks the #685 ergonomics wiring: WithAPIKey sets the
// static bearer, and Verify hits the cheap authenticated settings read.
func TestWithAPIKeyAndVerify(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	client := NewRemote(srv.URL, WithAPIKey(" sk-test "))
	if err := Verify(context.Background(), client); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("WithAPIKey bearer = %q", gotAuth)
	}
	if gotPath != "/v1/merchant/settings" {
		t.Fatalf("Verify path = %q", gotPath)
	}
}

// TestWithAPIKeyEmptyFailsPerCall: an empty key errors each call descriptively
// (constructor stays no-error, the mintless tokenFn pattern).
func TestWithAPIKeyEmptyFailsPerCall(t *testing.T) {
	client := NewRemote("http://127.0.0.1:0", WithAPIKey(""))
	err := Verify(context.Background(), client)
	if err == nil || !strings.Contains(err.Error(), "WithAPIKey") {
		t.Fatalf("expected descriptive empty-key error, got %v", err)
	}
}

// TestNewRemoteInvalidBaseURL: static URL validation is I/O-free and surfaces
// as a descriptive per-call error, never a constructor error.
func TestNewRemoteInvalidBaseURL(t *testing.T) {
	for _, base := range []string{"", "not a url", "ftp://example.com", "http://"} {
		client := NewRemote(base, WithAPIKey("k"))
		_, err := client.Balance(context.Background(), "11111111-1111-1111-1111-111111111111")
		if err == nil || !strings.Contains(err.Error(), "base URL") {
			t.Fatalf("base %q: expected descriptive base URL error, got %v", base, err)
		}
	}
}
