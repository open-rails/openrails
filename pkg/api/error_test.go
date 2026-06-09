package api

import (
	"net/http"
	"testing"
)

func TestAPIErrorToResponseIncludesMetadata(t *testing.T) {
	err := NewAPIError(http.StatusPaymentRequired, ErrorTypeCard, CodeInsufficientFunds, "short").
		WithParam("usdc_balance").
		WithMetadata(map[string]any{
			"usdc_funding": map[string]any{
				"asset":   "USDC",
				"network": "solana",
				"amount":  "12.5",
			},
		})

	resp := err.ToResponse()
	if resp.Error.Param == nil || *resp.Error.Param != "usdc_balance" {
		t.Fatalf("param = %#v, want usdc_balance", resp.Error.Param)
	}
	funding, ok := resp.Error.Metadata["usdc_funding"].(map[string]any)
	if !ok {
		t.Fatalf("usdc_funding metadata missing: %#v", resp.Error.Metadata)
	}
	if funding["network"] != "solana" || funding["amount"] != "12.5" {
		t.Fatalf("funding metadata = %#v", funding)
	}
}
