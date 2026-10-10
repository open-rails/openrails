package billingauth

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/api"
)

// WriteJSONError writes the OpenRails error envelope from a plain
// http.ResponseWriter, for middleware that answers before a handler exists,
// with the request id the request-log middleware set on the response.
func WriteJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := api.NewAPIError(status, api.TypeForCode(status, code), code, message)
	if id := strings.TrimSpace(w.Header().Get("X-Request-ID")); id != "" {
		body.WithRequestID(id)
	}
	_ = json.NewEncoder(w).Encode(body.ToResponse())
}
