package openrails

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRemoteTrustTierWireNames(t *testing.T) {
	var payerLimitBody map[string]any
	var admitBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/merchant/payer-spend-limits":
			if err := json.NewDecoder(r.Body).Decode(&payerLimitBody); err != nil {
				t.Fatalf("decode payer-spend-limits body: %v", err)
			}
			_, _ = w.Write([]byte(`{}`))
		case "/v1/merchant/admit":
			if err := json.NewDecoder(r.Body).Decode(&admitBody); err != nil {
				t.Fatalf("decode admit body: %v", err)
			}
			_, _ = w.Write([]byte(`{"allowed":true}`))
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

	if err := client.SetPayerSpendLimits(context.Background(), "cust_1", PayerSpendLimitInput{Tier: "legacy"}); err != nil {
		t.Fatalf("SetPayerSpendLimits: %v", err)
	}
	if payerLimitBody["trust_tier"] != "legacy" {
		t.Fatalf("expected trust_tier from legacy Tier alias, got %#v", payerLimitBody)
	}
	if _, ok := payerLimitBody["tier"]; ok {
		t.Fatalf("did not expect deprecated tier on wire: %#v", payerLimitBody)
	}

	if _, err := client.Admit(context.Background(), AdmitRequest{
		CustomerID: "cust_1", Tier: "legacy", EstimatedAmount: 1, RequestID: "req_1",
	}); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if admitBody["trust_tier"] != "legacy" {
		t.Fatalf("expected trust_tier from legacy Tier alias, got %#v", admitBody)
	}
	if _, ok := admitBody["tier"]; ok {
		t.Fatalf("did not expect deprecated tier on wire: %#v", admitBody)
	}

	tier, err := client.GetTier(context.Background(), "cust_1", "USD")
	if err != nil {
		t.Fatalf("GetTier: %v", err)
	}
	if tier != "gold" {
		t.Fatalf("expected trust_tier response to decode, got %q", tier)
	}
}
