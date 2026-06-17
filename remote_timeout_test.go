package openrails

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestWithTimeoutEnforcedWithCustomClient guards the per-call deadline: even
// when the host injects its own *http.Client that has NO Timeout set,
// WithTimeout must still bound the call (it is applied as a per-request context
// deadline in doRaw). A slow upstream therefore fails fast with ErrUnreachable
// instead of stalling the hot path.
func TestWithTimeoutEnforcedWithCustomClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewRemote(srv.URL,
		// Custom client with NO Timeout — the pre-fix code would block until the
		// server responded (500ms) instead of honoring WithTimeout.
		WithHTTPClient(&http.Client{}),
		WithTimeout(50*time.Millisecond),
		WithTokenProvider(func(context.Context) (string, error) { return "tok", nil }),
	)

	start := time.Now()
	_, err := c.Balance(context.Background(), "11111111-1111-1111-1111-111111111111")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("expected ErrUnreachable, got %v", err)
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("call was not bounded by WithTimeout: took %s", elapsed)
	}
}
