package middleware

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func TestRequestLogHTTPPropagatesRequestID(t *testing.T) {
	logger := log.StandardLogger()
	previousOutput := logger.Out
	previousFormatter := logger.Formatter
	t.Cleanup(func() {
		logger.SetOutput(previousOutput)
		logger.SetFormatter(previousFormatter)
	})

	var output bytes.Buffer
	logger.SetOutput(&output)
	logger.SetFormatter(&log.JSONFormatter{DisableTimestamp: true})

	tests := []struct {
		name      string
		requestID string
		generated bool
	}{
		{name: "caller id", requestID: " request-checkout-965 "},
		{name: "missing id", generated: true},
		{name: "oversized id", requestID: string(bytes.Repeat([]byte("x"), 129)), generated: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output.Reset()
			req := httptest.NewRequest(http.MethodPost, "/v1/me/checkout", nil)
			if tt.requestID != "" {
				req.Header.Set("X-Request-ID", tt.requestID)
			}
			rec := httptest.NewRecorder()

			RequestLogHTTP()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, r.Header.Get("X-Request-ID"), w.Header().Get("X-Request-ID"))
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rec, req)

			requestID := rec.Header().Get("X-Request-ID")
			if tt.generated {
				require.NoError(t, uuid.Validate(requestID))
			} else {
				require.Equal(t, "request-checkout-965", requestID)
			}

			var entry map[string]any
			require.NoError(t, json.Unmarshal(output.Bytes(), &entry))
			require.Equal(t, requestID, entry["request_id"])
			require.Equal(t, float64(http.StatusNoContent), entry["status"])
		})
	}
}

func TestRequestLogHTTPSkippedPathStillReturnsRequestID(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	RequestLogHTTP("/health/live")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)

	require.NoError(t, uuid.Validate(rec.Header().Get("X-Request-ID")))
}
