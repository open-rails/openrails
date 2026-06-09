package openrails

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// authorizerWithServer builds an Authorizer whose client points at a freshly
// started httptest server with the given handler and the given fail policy.
func authorizerWithServer(t *testing.T, policy FailPolicy, timeout time.Duration, h http.HandlerFunc, reconcile ReconcileEnqueuer) (*Authorizer, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	client, err := New(Config{BaseURL: srv.URL, TokenProvider: tokFn("cozy_st_k1_secret"), Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewAuthorizer(client, policy, reconcile)
	if err != nil {
		t.Fatal(err)
	}
	return a, srv.Close
}

func allowedHandler(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(AuthorizeResponse{Allowed: true, ReservationID: "res-1"})
}
func deniedHandler(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(AuthorizeResponse{Allowed: false, DenyCode: "insufficient_funds"})
}
func hangHandler(w http.ResponseWriter, _ *http.Request) {
	time.Sleep(200 * time.Millisecond)
	_ = json.NewEncoder(w).Encode(AuthorizeResponse{Allowed: true})
}

func TestAuthorizeHoldAllowed(t *testing.T) {
	a, done := authorizerWithServer(t, FailClosed, time.Second, allowedHandler, nil)
	defer done()
	d, err := a.AuthorizeHold(context.Background(), AuthorizeRequest{RequestID: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allowed || d.ReservationID != "res-1" || d.FailedOpen {
		t.Fatalf("unexpected decision: %+v", d)
	}
}

func TestAuthorizeHoldDeniedHonoredUnderBothPolicies(t *testing.T) {
	for _, policy := range []FailPolicy{FailClosed, FailOpen} {
		a, done := authorizerWithServer(t, policy, time.Second, deniedHandler, nil)
		d, err := a.AuthorizeHold(context.Background(), AuthorizeRequest{RequestID: "r"})
		done()
		if err != nil {
			t.Fatalf("policy=%s: %v", policy, err)
		}
		// A definitive deny must NEVER be flipped open by fail_open.
		if d.Allowed {
			t.Fatalf("policy=%s: definitive deny was admitted: %+v", policy, d)
		}
		if d.DenyCode != "insufficient_funds" {
			t.Fatalf("policy=%s: deny code = %q", policy, d.DenyCode)
		}
	}
}

func TestAuthorizeHoldFailClosedOnTimeout(t *testing.T) {
	a, done := authorizerWithServer(t, FailClosed, 20*time.Millisecond, hangHandler, nil)
	defer done()
	d, err := a.AuthorizeHold(context.Background(), AuthorizeRequest{RequestID: "r"})
	if err != nil {
		t.Fatalf("fail-policy must absorb the unreachable error: %v", err)
	}
	if d.Allowed {
		t.Fatal("fail_closed must reject on timeout")
	}
	if d.DenyCode != "openrails_unreachable" {
		t.Fatalf("deny code = %q", d.DenyCode)
	}
}

type recordingReconcile struct{ calls []AuthorizeRequest }

func (r *recordingReconcile) EnqueueDeferredAuthorize(_ context.Context, req AuthorizeRequest) error {
	r.calls = append(r.calls, req)
	return nil
}

func TestAuthorizeHoldFailOpenOnTimeout(t *testing.T) {
	rec := &recordingReconcile{}
	a, done := authorizerWithServer(t, FailOpen, 20*time.Millisecond, hangHandler, rec)
	defer done()
	d, err := a.AuthorizeHold(context.Background(), AuthorizeRequest{RequestID: "r", PayerTenantID: "org-1"})
	if err != nil {
		t.Fatalf("fail-policy must absorb the unreachable error: %v", err)
	}
	if !d.Allowed || !d.FailedOpen {
		t.Fatalf("fail_open must admit + mark FailedOpen: %+v", d)
	}
	if d.ReservationID != "" {
		t.Fatal("fail-open admission has no hold to settle")
	}
	if len(rec.calls) != 1 || rec.calls[0].RequestID != "r" {
		t.Fatalf("fail_open should enqueue a deferred reconcile, got %+v", rec.calls)
	}
}

func TestAuthorizeHold4xxSurfacesError(t *testing.T) {
	a, done := authorizerWithServer(t, FailOpen, time.Second, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad", http.StatusBadRequest)
	}, nil)
	defer done()
	// A 4xx contract error is NOT fail-policy-able even under fail_open.
	if _, err := a.AuthorizeHold(context.Background(), AuthorizeRequest{RequestID: "r"}); err == nil {
		t.Fatal("expected a surfaced error on 4xx, even under fail_open")
	}
}

// --- AdmitHold (#403/#404 unified verdict) ---

func TestAdmitHoldAllowed(t *testing.T) {
	a, done := authorizerWithServer(t, FailClosed, time.Second, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(AdmitResponse{Allowed: true, ReservationID: "res-admit"})
	}, nil)
	defer done()
	d, err := a.AdmitHold(context.Background(), AdmitRequest{RequestID: "r", EstimateCents: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allowed || d.ReservationID != "res-admit" || d.BlockedBy != "" {
		t.Fatalf("unexpected decision: %+v", d)
	}
}

func TestAdmitHoldThroughputDeny(t *testing.T) {
	a, done := authorizerWithServer(t, FailClosed, time.Second, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(AdmitResponse{Allowed: false, BlockedBy: "throughput", RetryAfterSeconds: 7})
	}, nil)
	defer done()
	d, err := a.AdmitHold(context.Background(), AdmitRequest{RequestID: "r", EstimateCents: 10})
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed {
		t.Fatalf("throughput deny must not be admitted: %+v", d)
	}
	// BlockedBy axis with no money DenyCode maps to the stable rate-limit code.
	if d.BlockedBy != "throughput" || d.DenyCode != "rate_limit_exceeded" || d.RetryAfterSeconds != 7 {
		t.Fatalf("unexpected throughput decision: %+v", d)
	}
}

func TestAdmitHoldMoneyDenyHonored(t *testing.T) {
	a, done := authorizerWithServer(t, FailOpen, time.Second, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(AdmitResponse{Allowed: false, BlockedBy: "money", DenyCode: "insufficient_credits"})
	}, nil)
	defer done()
	d, err := a.AdmitHold(context.Background(), AdmitRequest{RequestID: "r", EstimateCents: 10})
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed { // a definitive deny is never flipped open
		t.Fatalf("money deny admitted: %+v", d)
	}
	if d.DenyCode != "insufficient_credits" {
		t.Fatalf("deny code = %q", d.DenyCode)
	}
}

func TestAdmitHoldFailClosedOnTimeout(t *testing.T) {
	a, done := authorizerWithServer(t, FailClosed, 20*time.Millisecond, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(AdmitResponse{Allowed: true})
	}, nil)
	defer done()
	d, err := a.AdmitHold(context.Background(), AdmitRequest{RequestID: "r", EstimateCents: 10})
	if err != nil {
		t.Fatalf("fail-policy must absorb the unreachable error: %v", err)
	}
	if d.Allowed || d.DenyCode != "openrails_unreachable" {
		t.Fatalf("fail_closed must reject on timeout: %+v", d)
	}
}

func TestParseFailPolicy(t *testing.T) {
	cases := map[string]FailPolicy{
		"fail_closed": FailClosed,
		"fail_open":   FailOpen,
		"closed":      FailClosed,
		"open":        FailOpen,
	}
	for in, want := range cases {
		got, err := ParseFailPolicy(in)
		if err != nil || got != want {
			t.Fatalf("ParseFailPolicy(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseFailPolicy(""); err == nil {
		t.Fatal("empty policy must be an error (no silent default)")
	}
	if _, err := ParseFailPolicy("yolo"); err == nil {
		t.Fatal("unknown policy must be an error")
	}
}

func TestNewAuthorizerRequiresExplicitPolicy(t *testing.T) {
	client, _ := New(Config{BaseURL: "http://x", TokenProvider: tokFn("y")})
	if _, err := NewAuthorizer(client, FailPolicy(0), nil); err == nil {
		t.Fatal("zero/unset policy must be rejected")
	}
	if _, err := NewAuthorizer(nil, FailClosed, nil); err == nil {
		t.Fatal("nil client must be rejected")
	}
}
