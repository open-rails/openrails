package basistheory

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUpdateCardWire(t *testing.T) {
	const reply = `{
  "new_token": {
    "id": "c06d0789-0a38-40be-b7cc-c28a718f76f1",
    "tenant_id": "77cb0024-123e-41a8-8ff8-a3d5a0fa8a08",
    "type": "card",
    "containers": ["/merchants/m1/"],
    "fingerprint": "fp-new",
    "data": {"number": "XXXXXXXXXXXX4242", "expiration_month": 12, "expiration_year": 2031},
    "card": {"bin": "42424242", "last4": "4242", "expiration_month": 12, "expiration_year": 2031, "brand": "visa", "funding": "credit"}
  },
  "result_code": "UPD_PAN"
}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/account-updater/real-time" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("BT-API-KEY") != "key_private" || r.Header.Get("BT-IDEMPOTENCY-KEY") != "attempt-1" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("headers %v", r.Header)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"token_id":"b4b89606-794a-4652-b375-f4e1d717a5db"}` {
			t.Errorf("body %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	defer server.Close()
	client, err := New(Config{APIKey: "key_private", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.UpdateCard(t.Context(), "b4b89606-794a-4652-b375-f4e1d717a5db", "attempt-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ResultCode != AUUpdatedPAN || got.NewToken == nil || got.NewToken.ID != "c06d0789-0a38-40be-b7cc-c28a718f76f1" ||
		got.NewToken.TenantID != "77cb0024-123e-41a8-8ff8-a3d5a0fa8a08" || got.NewToken.Type != "card" || got.NewToken.Fingerprint != "fp-new" ||
		len(got.NewToken.Containers) != 1 || got.NewToken.Containers[0] != "/merchants/m1/" {
		t.Fatalf("decoded %+v", got)
	}
	if c := got.NewToken.Card; c == nil || c.Last4 != "4242" || c.Brand != "visa" || c.ExpirationMonth != 12 || c.ExpirationYear != 2031 {
		t.Fatalf("card %+v", got.NewToken.Card)
	}
	// The token's number is never decoded into anything OpenRails keeps.
	if raw, _ := json.Marshal(got); strings.Contains(string(raw), "XXXXXXXXXXXX4242") {
		t.Fatalf("token data survived decoding: %s", raw)
	}
}

func TestUpdateCardNoUpdateAndRefusals(t *testing.T) {
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = io.WriteString(w, `{"result_code":"NO_UPDATE"}`)
			return
		}
		_, _ = io.WriteString(w, `{"title":"Not Found","detail":"token not found"}`)
	}))
	defer server.Close()
	client, err := New(Config{APIKey: "key_private", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.UpdateCard(t.Context(), "tok", "k")
	if err != nil || got.NewToken != nil || ClassifyAccountUpdaterResult(got.ResultCode) != AUOutcomeNoChange {
		t.Fatalf("no update: %+v %v", got, err)
	}
	status = http.StatusNotFound
	_, err = client.UpdateCard(t.Context(), "tok", "k")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("refusal: %v", err)
	}
	readOnly, err := New(Config{APIKey: "key_private", BaseURL: server.URL, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readOnly.UpdateCard(t.Context(), "tok", "k"); !errors.Is(err, ErrProviderReadOnly) {
		t.Fatalf("read-only: %v", err)
	}
}

func TestClassifyAccountUpdaterResult(t *testing.T) {
	for code, want := range map[string]AUOutcome{
		"UPD_PAN": AUOutcomeUpdated, "UPD_EXP_DATE": AUOutcomeUpdated, "UPD_BRAND_CONV": AUOutcomeUpdated, "UPD_CORRECTED": AUOutcomeUpdated,
		"NO_UPDATE": AUOutcomeNoChange, "WRN_ISSUER_NOT_ENROLLED": AUOutcomeNoChange, "WRN_ISSUER_NO_DATA": AUOutcomeNoChange,
		"WRN_OPT_OUT": AUOutcomeNoChange, "WRN_UNSUPPORTED_NETWORK": AUOutcomeNoChange,
		"WRN_CLOSED_ACCOUNT": AUOutcomeClosed, "WRN_CONTACT_CARDHOLDER": AUOutcomeContactCardholder,
		"ERR_INVALID_TOKEN": AUOutcomeUnrecognized, "SOMETHING_NEW": AUOutcomeUnrecognized,
	} {
		if got := ClassifyAccountUpdaterResult(code); got != want {
			t.Errorf("%s = %s, want %s", code, got, want)
		}
	}
}
