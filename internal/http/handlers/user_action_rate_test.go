package handlers

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/openrails/internal/destructive"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/stretchr/testify/require"
)

func TestUserActionLimitRespondsWithRetryAfter(t *testing.T) {
	w := httptest.NewRecorder()
	r := httprequest.NewHTTP(w, httptest.NewRequest("DELETE", "/me/payment-methods/example", nil), nil)
	writeRefusal(r, fmt.Errorf("remove card: %w", &destructive.UserActionRateError{RetryAfterSeconds: 3120}), "not reached")
	require.Equal(t, 429, w.Code)
	require.Equal(t, "3120", w.Header().Get("Retry-After"))
	require.Contains(t, w.Body.String(), `"code":"rate_limit_exceeded"`)
	require.Contains(t, w.Body.String(), `"retry_after_seconds":3120`)
}
