package openrails

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRemoteTrustTierWireNames(t *testing.T) {
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
		case "/v1/merchant/trust-tier":
			_, _ = w.Write([]byte(`{"currency":"USD","trust_tier":"gold"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := NewRemote(srv.URL, WithTokenProvider(func(context.Context) (string, error) {
		return "test-token", nil
	}))

	if err := client.SetMerchantSettings(context.Background(), MerchantSettings{
		TierSpendLimits: []PayerSpendLimitInput{{TrustTier: "gold"}},
	}); err != nil {
		t.Fatalf("SetMerchantSettings: %v", err)
	}
	policies, ok := settingsBody["tier_spend_limits"].([]any)
	if !ok || len(policies) != 1 {
		t.Fatalf("expected one tier_spend_limits item, got %#v", settingsBody)
	}
	policy, _ := policies[0].(map[string]any)
	if policy["trust_tier"] != "gold" {
		t.Fatalf("expected trust_tier inside settings document, got %#v", settingsBody)
	}

	if _, err := client.AdmitBatch(context.Background(), []AdmitRequest{{
		CustomerID: "cust_1", TrustTier: "gold", EstimatedAmount: 1, RequestID: "req_1",
	}}); err != nil {
		t.Fatalf("AdmitBatch: %v", err)
	}
	items, ok := admissionsBody["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("expected one admissions item, got %#v", admissionsBody)
	}
	admitBody, _ := items[0].(map[string]any)
	if admitBody["trust_tier"] != "gold" {
		t.Fatalf("expected trust_tier on admission item, got %#v", admissionsBody)
	}
	if _, ok := admitBody["tier"]; ok {
		t.Fatalf("did not expect deprecated tier on wire: %#v", admissionsBody)
	}

	tier, err := client.GetTier(context.Background(), "cust_1", "USD")
	if err != nil {
		t.Fatalf("GetTier: %v", err)
	}
	if tier != "gold" {
		t.Fatalf("expected trust_tier response to decode, got %q", tier)
	}
}
