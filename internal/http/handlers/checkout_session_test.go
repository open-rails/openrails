package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-rails/openrails/internal/app"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
)

func TestWriteCheckoutSessionErrorRendersPaymentRefusals(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantType    string
		wantCode    string
		wantMessage string
		wantReason  any
		wantFailure any
	}{
		{
			name:        "generic decline",
			err:         &paymentmethods.PaymentMethodError{LocalizationID: "do_not_honor", Message: "raw processor detail that must stay server-side"},
			wantStatus:  http.StatusPaymentRequired,
			wantType:    "card_error",
			wantCode:    "card_declined",
			wantMessage: "Your card was declined. Contact your bank or try a different card.",
			wantReason:  "card_declined",
			wantFailure: "do_not_honor",
		},
		{
			name:        "security code mismatch by numeric response code",
			err:         &paymentmethods.PaymentMethodError{LocalizationID: "225", Message: "raw processor detail that must stay server-side"},
			wantStatus:  http.StatusPaymentRequired,
			wantType:    "card_error",
			wantCode:    "card_declined",
			wantMessage: "The card's security code is incorrect.",
			wantReason:  "cvv_avs",
			wantFailure: "225",
		},
		{
			name:        "suspected fraud",
			err:         &paymentmethods.PaymentMethodError{LocalizationID: "fraudulent_card", Message: "raw processor detail that must stay server-side"},
			wantStatus:  http.StatusPaymentRequired,
			wantType:    "card_error",
			wantCode:    "card_declined",
			wantMessage: "Your card was declined. Contact your bank or try a different card.",
			wantReason:  "fraud_suspected",
			wantFailure: "fraudulent_card",
		},
		{
			name:        "unknown response",
			err:         &paymentmethods.PaymentMethodError{LocalizationID: "unmapped_gateway_response", Message: "raw processor detail that must stay server-side"},
			wantStatus:  http.StatusPaymentRequired,
			wantType:    "card_error",
			wantCode:    "card_declined",
			wantMessage: "Your card was declined. Contact your bank or try a different card.",
			wantReason:  "unknown",
			wantFailure: "unmapped_gateway_response",
		},
		{
			name:        "gateway rejection is not a card decline",
			err:         &paymentmethods.PaymentMethodError{LocalizationID: "300", Message: "raw processor detail that must stay server-side"},
			wantStatus:  http.StatusBadGateway,
			wantType:    "api_error",
			wantCode:    "payment_provider_rejected",
			wantMessage: "The payment processor could not complete this payment. Please try again later.",
			wantReason:  "processor_error",
			wantFailure: "300",
		},
		{
			name:        "stale saved payment method",
			err:         fmt.Errorf("%w: raw processor detail that must stay server-side", checkout.ErrPaymentMethodStale),
			wantStatus:  http.StatusPaymentRequired,
			wantType:    "card_error",
			wantCode:    "payment_method_stale",
			wantMessage: "This saved payment method can no longer be used. Add the card again.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			httpReq := httptest.NewRequest(http.MethodPost, "/v1/me/checkout", nil)
			httpReq.Header.Set("X-Request-ID", "req-checkout-149")
			req := httprequest.NewHTTP(rec, httpReq, &app.Runtime{})

			writeCheckoutSessionError(req, tt.err, checkoutSessionErrorContext{})

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			var body struct {
				Error struct {
					Type      string         `json:"type"`
					Code      string         `json:"code"`
					Message   string         `json:"message"`
					RequestID string         `json:"request_id"`
					Metadata  map[string]any `json:"metadata"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Type != tt.wantType || body.Error.Code != tt.wantCode || body.Error.Message != tt.wantMessage {
				t.Fatalf("error = %#v, want type %q code %q message %q", body.Error, tt.wantType, tt.wantCode, tt.wantMessage)
			}
			if body.Error.RequestID != "req-checkout-149" {
				t.Fatalf("request_id = %q", body.Error.RequestID)
			}
			if body.Error.Metadata["decline_reason"] != tt.wantReason || body.Error.Metadata["failure_code"] != tt.wantFailure {
				t.Fatalf("metadata = %#v, want decline_reason %v failure_code %v", body.Error.Metadata, tt.wantReason, tt.wantFailure)
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

// An unresolved provider outcome is a typed retry-with-the-same-key conflict,
// never an internal error.
func TestProcessingProviderOutcomeIsConflict(t *testing.T) {
	session := func(r *httprequest.Request, err error) {
		writeCheckoutSessionError(r, err, checkoutSessionErrorContext{})
	}
	for _, tc := range []struct {
		write func(*httprequest.Request, error)
		err   error
	}{
		{session, fmt.Errorf("sale: %w", checkout.ErrCheckoutProcessing)},
		{writeChangeTierError, fmt.Errorf("upgrade: %w", checkout.ErrCheckoutProcessing)},
		{writeChangeTierError, checkout.ErrTierChangePending},
	} {
		rec := httptest.NewRecorder()
		tc.write(httprequest.NewHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil), &app.Runtime{}), tc.err)
		if rec.Code != http.StatusConflict {
			t.Fatalf("%v: status = %d, want %d", tc.err, rec.Code, http.StatusConflict)
		}
	}
}
