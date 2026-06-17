package openrails

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

// TestPathIDValidationFailsFast ensures the remote client rejects empty hold
// request_ids and nil window ids CLIENT-SIDE (as ErrInvalid) instead of
// constructing a malformed URL (e.g. /holds//capture) or a zero-UUID path that
// would hit an unintended route on the server.
func TestPathIDValidationFailsFast(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewRemote(srv.URL,
		WithTokenProvider(func(context.Context) (string, error) { return "tok", nil }),
	)
	ctx := context.Background()

	checks := []struct {
		name string
		call func() error
	}{
		{"CaptureHold", func() error { _, e := c.CaptureHold(ctx, CaptureHoldRequest{RequestID: "  "}); return e }},
		{"ReleaseHold", func() error { return c.ReleaseHold(ctx, "") }},
		{"Capture", func() error { return c.Capture(ctx, "", 1, nil) }},
		{"Release", func() error { return c.Release(ctx, "") }},
		{"RefillWindow", func() error { _, e := c.RefillWindow(ctx, uuid.Nil, 1, 1); return e }},
		{"CloseWindow", func() error { _, e := c.CloseWindow(ctx, uuid.Nil); return e }},
	}

	for _, tc := range checks {
		err := tc.call()
		if err == nil {
			t.Fatalf("%s: expected client-side validation error, got nil", tc.name)
		}
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: expected ErrInvalid, got %v", tc.name, err)
		}
	}

	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("expected no requests to reach the server, got %d", got)
	}
}
