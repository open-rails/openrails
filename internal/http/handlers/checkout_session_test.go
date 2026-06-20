package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/openrails/internal/app"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
)

func TestWriteCheckoutSessionErrorIncludesUSDCFundingMetadata(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httprequest.NewHTTP(
		rec,
		httptest.NewRequest(http.MethodPost, "/v1/me/checkout/chk_123/confirm", nil),
		&app.Runtime{},
	)

	writeCheckoutSessionError(req, &recurring.InsufficientUSDCError{
		HaveBaseUnits: 250_000,
		NeedBaseUnits: 1_500_000,
	}, checkoutSessionErrorContext{
		Processor:         "solana",
		Wallet:            "11111111111111111111111111111111",
		CheckoutSessionID: "chk_123",
	})

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusPaymentRequired)
	}
	var body struct {
		Error struct {
			Code     string         `json:"code"`
			Param    string         `json:"param"`
			Metadata map[string]any `json:"metadata"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "insufficient_funds" || body.Error.Param != "usdc_balance" {
		t.Fatalf("error = %#v", body.Error)
	}
	funding, ok := body.Error.Metadata["usdc_funding"].(map[string]any)
	if !ok {
		t.Fatalf("usdc_funding metadata missing: %#v", body.Error.Metadata)
	}
	if funding["asset"] != "USDC" || funding["network"] != "solana" {
		t.Fatalf("unexpected asset/network: %#v", funding)
	}
	if funding["wallet"] != "11111111111111111111111111111111" {
		t.Fatalf("wallet = %#v", funding["wallet"])
	}
	if funding["amount"] != "1.5" || funding["balance"] != "0.25" || funding["shortfall"] != "1.25" {
		t.Fatalf("amount metadata = %#v", funding)
	}
	if funding["amount_base_units"] != "1500000" || funding["balance_base_units"] != "250000" || funding["shortfall_base_units"] != "1250000" {
		t.Fatalf("base-unit metadata = %#v", funding)
	}
}
