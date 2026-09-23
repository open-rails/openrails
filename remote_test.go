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

	"github.com/google/uuid"
)

var testCustomer = CustomerID(uuid.MustParse("7d5b4a0e-8c3f-4c1e-9b2a-1f0e2d3c4b5a"))

func TestRemoteTrustLevelWireNames(t *testing.T) {
	var settingsBody map[string]any
	var admissionsBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/merchant/settings":
			if err := json.NewDecoder(r.Body).Decode(&settingsBody); err != nil {
				t.Fatalf("decode settings body: %v", err)
			}
			_, _ = w.Write([]byte(`{}`))
		case "/v2/merchant/admissions":
			if err := json.NewDecoder(r.Body).Decode(&admissionsBody); err != nil {
				t.Fatalf("decode admissions body: %v", err)
			}
			_, _ = w.Write([]byte(`{"items":[{"status":200,"result":{"allowed":true}}]}`))
		case "/v2/merchant/trust-level":
			_, _ = w.Write([]byte(`{"currency":"USD","trust_level":"gold"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client, clientErr := NewRemote(srv.URL, WithDefaultMerchant("fixture"), WithTokenProvider(func(context.Context) (string, error) {
		return "test-token", nil
	}))
	if clientErr != nil {
		t.Fatal(clientErr)
	}

	if err := client.SetMerchantSettings(context.Background(), MerchantSettings{
		BillingPolicies: []BillingPolicyInput{{
			Name: "api_line", Kind: "outstanding_cap", OutstandingCapAmount: 200_000_000,
		}},
		BillingPolicyBindings: []BillingPolicyBindingInput{{PolicyName: "api_line", Tier: "gold"}},
	}); err != nil {
		t.Fatalf("SetMerchantSettings: %v", err)
	}
	policies, ok := settingsBody["billing_policies"].([]any)
	if !ok || len(policies) != 1 {
		t.Fatalf("expected one billing_policies item, got %#v", settingsBody)
	}
	policy, _ := policies[0].(map[string]any)
	if policy["name"] != "api_line" || policy["kind"] != "outstanding_cap" {
		t.Fatalf("expected the named policy inside settings document, got %#v", settingsBody)
	}
	bindings, ok := settingsBody["billing_policy_bindings"].([]any)
	if !ok || len(bindings) != 1 {
		t.Fatalf("expected one billing_policy_bindings item, got %#v", settingsBody)
	}
	binding, _ := bindings[0].(map[string]any)
	if binding["policy"] != "api_line" || binding["tier"] != "gold" {
		t.Fatalf("expected the tier binding inside settings document, got %#v", settingsBody)
	}

	if _, err := client.AdmitBatch(context.Background(), []AdmitRequest{{
		CustomerID: testCustomer.String(), TrustLevel: "gold", EstimatedAmount: 1, ExpiresAt: holdDeadline(), RequestID: "req_1", AccrualRateDeltaPerHour: 42,
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
	if admitBody["accrual_rate_delta_per_hour"] != "42" {
		t.Fatalf("prospective rate missing from admission: %#v", admitBody)
	}

	trustLevel, err := client.GetTrustLevel(context.Background(), (testCustomer).String(), "USD")
	if err != nil {
		t.Fatalf("GetTrustLevel: %v", err)
	}
	if trustLevel != "gold" {
		t.Fatalf("expected trust_level response to decode, got %q", trustLevel)
	}
}

func TestRemoteSetCustomerSpendDelegation(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		if r.URL.Path != "/v2/merchant/customers/"+testCustomer.String()+"/spend-delegations:upsert" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	client, clientErr := NewRemote(srv.URL, WithDefaultMerchant("fixture"), WithTokenProvider(func(context.Context) (string, error) {
		return "test-token", nil
	}))
	if clientErr != nil {
		t.Fatal(clientErr)
	}
	err := client.SetCustomerSpendDelegation(context.Background(), (testCustomer).String(), SpendDelegationInput{
		Scope: "invoker", ScopeKey: "issuer:subject:digest:entitlement",
		Windows: []SpendLimitWindow{{Key: "month", WindowSeconds: 2592000, Limit: 42, Currency: "USD"}},
	})
	if err != nil {
		t.Fatalf("SetCustomerSpendDelegation: %v", err)
	}
	if got["scope"] != "invoker" || got["scope_key"] != "issuer:subject:digest:entitlement" {
		t.Fatalf("unexpected body: %#v", got)
	}
}

func TestRemoteSetCustomerSpendDelegationsUsesMerchantMachineRoute(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		if r.URL.Path != "/v2/merchant/customers/"+testCustomer.String()+"/spend-delegations" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	client, clientErr := NewRemote(srv.URL, WithDefaultMerchant("fixture"), WithTokenProvider(func(context.Context) (string, error) {
		return "test-token", nil
	}))
	if clientErr != nil {
		t.Fatal(clientErr)
	}
	err := client.SetCustomerSpendDelegations(context.Background(), (testCustomer).String(), []SpendDelegationInput{{
		Scope: "invoker", ScopeKey: "invoker-1",
		Windows: []SpendLimitWindow{{Key: "month", WindowSeconds: 2592000, Limit: 42, Currency: "USD"}},
	}})
	if err != nil {
		t.Fatalf("SetCustomerSpendDelegations: %v", err)
	}
	if rows, ok := got["delegations"].([]any); !ok || len(rows) != 1 {
		t.Fatalf("unexpected body: %#v", got)
	}
}

func TestRemoteDeleteCustomerSpendDelegation(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"deleted":true}`))
	}))
	defer srv.Close()

	client, clientErr := NewRemote(srv.URL, WithDefaultMerchant("fixture"), WithTokenProvider(func(context.Context) (string, error) {
		return "test-token", nil
	}))
	if clientErr != nil {
		t.Fatal(clientErr)
	}
	err := client.DeleteCustomerSpendDelegation(context.Background(), (testCustomer).String(), "invoker", "user:11111111-1111-1111-1111-111111111111")
	if err != nil {
		t.Fatalf("DeleteCustomerSpendDelegation: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Fatalf("method = %s, want DELETE", gotMethod)
	}
	if gotPath != "/v2/merchant/customers/"+testCustomer.String()+"/spend-delegations/invoker/user:11111111-1111-1111-1111-111111111111" {
		t.Fatalf("path = %s", gotPath)
	}
}

// TestRemoteValidationErrorParity asserts that client-side pre-flight argument
// checks return typed *StatusError values whose errors.Is chain includes
// ErrInvalid — matching the embedded transport's behavior (#338).
// No live server is required; validation fires before any HTTP call.
func TestRemoteValidationErrorParity(t *testing.T) {
	// Use an unreachable URL — none of the tested calls should reach the network.
	client, clientErr := NewRemote("http://127.0.0.1:0", WithDefaultMerchant("fixture"), WithTokenProvider(func(context.Context) (string, error) {
		return "test-token", nil
	}))
	if clientErr != nil {
		t.Fatal(clientErr)
	}

	ctx := context.Background()

	tests := []struct {
		name string
		fn   func() error
	}{
		{
			name: "Capture empty request_id",
			fn: func() error {
				_, err := client.Capture(ctx, "", 100, nil)
				return err
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
				return client.SetCustomerSpendDelegations(ctx, (CustomerID{}).String(), nil)
			},
		},
		{
			name: "SetCustomerSpendDelegation empty customer_id",
			fn: func() error {
				return client.SetCustomerSpendDelegation(ctx, (CustomerID{}).String(), SpendDelegationInput{})
			},
		},
		{
			name: "ListEntitlements empty subject",
			fn: func() error {
				_, err := client.ListEntitlements(ctx, (CustomerID{}).String(), time.Time{})
				return err
			},
		},
		{
			name: "HasEntitlement empty entitlement",
			fn: func() error {
				_, err := client.HasEntitlement(ctx, (testCustomer).String(), "", time.Time{})
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
				_, err := client.ProductAccess.List(ctx, &ProductAccessListParams{})
				return err
			},
		},
		{
			name: "HasProductAccess empty subject",
			fn: func() error {
				_, err := client.ProductAccess.Check(ctx, &ProductAccessCheckParams{ProductID: ProductID(uuid.New()).String()})
				return err
			},
		},
		{
			name: "HasProductAccess empty product_id",
			fn: func() error {
				_, err := client.ProductAccess.Check(ctx, &ProductAccessCheckParams{CustomerID: testCustomer.String()})
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

	client, clientErr := NewRemote(srv.URL, WithDefaultMerchant("fixture"), WithAPIKey(" sk-test "))
	if clientErr != nil {
		t.Fatal(clientErr)
	}
	if err := client.Verify(context.Background()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("WithAPIKey bearer = %q", gotAuth)
	}
	if gotPath != "/v2/merchant/settings" {
		t.Fatalf("Verify path = %q", gotPath)
	}
}

func TestNewRemoteRejectsInvalidConfiguration(t *testing.T) {
	for _, base := range []string{"", "not a url", "ftp://example.com", "http://", "https://user:pass@example.com", "https://example.com?x=y", "https://example.com#fragment"} {
		client, err := NewRemote(base, WithAPIKey("key"))
		if err == nil || client != nil || !strings.Contains(err.Error(), "base URL") {
			t.Fatalf("base %q: client=%v error=%v", base, client, err)
		}
	}
	if client, err := NewRemote("https://example.com", WithAPIKey("")); err == nil || client != nil {
		t.Fatal("empty API key accepted")
	}
	if client, err := NewRemote("https://example.com"); err == nil || client != nil {
		t.Fatal("absent credential provider accepted")
	}
}

// holdDeadline is the declared deadline every hold-placing admit must carry
// (xs-007 row 33): an hour from now, as a job would declare.
func holdDeadline() *time.Time {
	v := time.Now().Add(time.Hour)
	return &v
}
