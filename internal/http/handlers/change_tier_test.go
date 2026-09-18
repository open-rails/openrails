package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/app"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/checkout"
)

func decodeErrorEnvelope(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return body.Error
}

// A request under another key while a tier change is unresolved is a coded
// conflict naming the operation the caller must replay.
func TestTierChangeInFlightConflictNamesOperation(t *testing.T) {
	rec := httptest.NewRecorder()
	id := uuid.New()
	writeChangeTierError(httprequest.NewHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil), &app.Runtime{}), &checkout.TierChangeInFlightError{OperationID: id})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	env := decodeErrorEnvelope(t, rec)
	if env["code"] != openrails.CodeTierChangeInFlight {
		t.Fatalf("code = %v", env["code"])
	}
	if meta, _ := env["metadata"].(map[string]any); meta["operation_id"] != id.String() {
		t.Fatalf("metadata = %v", env["metadata"])
	}
}

// A coded refusal keeps the provider's status and decline code.
func TestTierChangeRefusalKeepsProviderCode(t *testing.T) {
	rec := httptest.NewRecorder()
	writeChangeTierError(httprequest.NewHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil), &app.Runtime{}), &checkout.TierChangeError{HTTPStatus: http.StatusPaymentRequired, Code: "insufficient_funds", Message: "Your card has insufficient funds."})
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", rec.Code)
	}
	env := decodeErrorEnvelope(t, rec)
	if env["code"] != "insufficient_funds" || env["type"] != "card_error" {
		t.Fatalf("envelope = %v", env)
	}
}

// An unresolved provider outcome is accepted, not completed: 202 with the
// operation id, so a lost first response can still be read back.
func TestTierChangeProcessingIsAccepted(t *testing.T) {
	rec := httptest.NewRecorder()
	writeTierChangeResponse(httprequest.NewHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil), &app.Runtime{}), &checkout.TierChangeResponse{Object: "tier_change", Status: "processing", OperationID: "op-1"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	var body checkout.TierChangeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.OperationID != "op-1" || body.Status != "processing" {
		t.Fatalf("body = %s (%v)", rec.Body.String(), err)
	}
	rec = httptest.NewRecorder()
	writeTierChangeResponse(httprequest.NewHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil), &app.Runtime{}), &checkout.TierChangeResponse{Object: "tier_change", Status: "succeeded"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
