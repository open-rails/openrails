package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-rails/openrails/internal/app"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
)

func TestWriteCheckoutSessionErrorSanitizesVaultFailures(t *testing.T) {
	tests := []struct {
		name           string
		localizationID string
		wantCode       string
		wantMessage    string
		wantReason     string
	}{
		{
			name:           "generic decline",
			localizationID: "do_not_honor",
			wantCode:       "card_declined",
			wantMessage:    "Your card was declined. Contact your bank or try a different card.",
			wantReason:     "card_declined",
		},
		{
			name:           "security code mismatch",
			localizationID: "invalid_card_security_code",
			wantCode:       "cvv_avs",
			wantMessage:    "Check your card security code and billing details, then try again.",
			wantReason:     "cvv_avs",
		},
		{
			name:           "suspected fraud",
			localizationID: "fraudulent_card",
			wantCode:       "fraud_suspected",
			wantMessage:    "Your bank declined this payment. Contact your bank or try a different card.",
			wantReason:     "fraud_suspected",
		},
		{
			name:           "unknown response",
			localizationID: "unmapped_gateway_response",
			wantCode:       "payment_failed",
			wantMessage:    "We could not complete this payment. Please try again or use a different card.",
			wantReason:     "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			httpReq := httptest.NewRequest(http.MethodPost, "/v1/me/checkout", nil)
			httpReq.Header.Set("X-Request-ID", "req-checkout-149")
			req := httprequest.NewHTTP(rec, httpReq, &app.Runtime{})

			writeCheckoutSessionError(req, &paymentmethods.PaymentMethodError{
				LocalizationID: tt.localizationID,
				Message:        "raw processor detail that must stay server-side",
			}, checkoutSessionErrorContext{})

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			var body struct {
				Error struct {
					Code      string         `json:"code"`
					Message   string         `json:"message"`
					RequestID string         `json:"request_id"`
					Metadata  map[string]any `json:"metadata"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != tt.wantCode || body.Error.Message != tt.wantMessage {
				t.Fatalf("error = %#v, want code %q message %q", body.Error, tt.wantCode, tt.wantMessage)
			}
			if body.Error.RequestID != "req-checkout-149" {
				t.Fatalf("request_id = %q", body.Error.RequestID)
			}
			if body.Error.Metadata["decline_reason"] != tt.wantReason {
				t.Fatalf("decline_reason = %#v, want %q", body.Error.Metadata["decline_reason"], tt.wantReason)
			}
			if strings.Contains(rec.Body.String(), "raw processor detail") {
				t.Fatalf("response leaked processor text: %s", rec.Body.String())
			}
		})
	}
}

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
		Rail:              "solana",
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
