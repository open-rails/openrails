package openrails

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, srvURL string) *Client {
	t.Helper()
	c, err := New(Config{BaseURL: srvURL, TokenProvider: tokFn("cozy_st_k1_secret"), Timeout: time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestAuthorizeAllowed(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody AuthorizeRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(AuthorizeResponse{
			Allowed: true, AvailableCents: 5000, ReservationID: "res-1",
		})
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL).Authorize(context.Background(), AuthorizeRequest{
		PayerOrgID: "org-1", InvokerID: "oat:k1", EstimateCents: 100, RequestID: "req-1",
	})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !resp.Allowed || resp.ReservationID != "res-1" {
		t.Fatalf("unexpected resp: %+v", resp)
	}
	if gotAuth != "Bearer cozy_st_k1_secret" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if gotPath != "/v1/service/credits/authorize" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody.PayerOrgID != "org-1" || gotBody.InvokerID != "oat:k1" || gotBody.RequestID != "req-1" || gotBody.EstimateCents != 100 {
		t.Fatalf("body threaded wrong: %+v", gotBody)
	}
}

func TestAuthorizeDeniedIsNotError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(AuthorizeResponse{Allowed: false, DenyCode: "insufficient_funds"})
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL).Authorize(context.Background(), AuthorizeRequest{RequestID: "r"})
	if err != nil {
		t.Fatalf("a clean deny must not be an error: %v", err)
	}
	if resp.Allowed || resp.DenyCode != "insufficient_funds" {
		t.Fatalf("unexpected deny resp: %+v", resp)
	}
}

func TestAuthorizeTimeoutIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(AuthorizeResponse{Allowed: true})
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, TokenProvider: tokFn("cozy_st_k1_secret"), Timeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Authorize(context.Background(), AuthorizeRequest{RequestID: "r"})
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("timeout must map to ErrUnreachable, got %v", err)
	}
}

func TestAuthorize5xxIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).Authorize(context.Background(), AuthorizeRequest{RequestID: "r"})
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("5xx must map to ErrUnreachable, got %v", err)
	}
}

func TestAuthorize4xxIsNotUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad invoker", http.StatusBadRequest)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).Authorize(context.Background(), AuthorizeRequest{RequestID: "r"})
	if err == nil {
		t.Fatal("expected an error on 4xx")
	}
	if errors.Is(err, ErrUnreachable) {
		t.Fatal("4xx is a contract error, must NOT be ErrUnreachable")
	}
}

func TestCaptureAndRelease(t *testing.T) {
	var capturePath, releasePath string
	// Decode into a RAW map (not CaptureRequest) so the test guards the literal
	// WIRE field name: OpenRails binds `amount` as required, and a struct-to-struct
	// decode would silently pass even if the json tag drifted.
	var captureRaw map[string]int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/capture"):
			capturePath = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&captureRaw)
		case strings.HasSuffix(r.URL.Path, "/release"):
			releasePath = r.URL.Path
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if err := c.Capture(context.Background(), "res-9", 42, nil); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if capturePath != "/v1/service/credits/holds/res-9/capture" {
		t.Fatalf("capture path = %s", capturePath)
	}
	if _, ok := captureRaw["amount"]; !ok || captureRaw["amount"] != 42 {
		t.Fatalf("capture body must carry OpenRails wire field amount=42, got %+v", captureRaw)
	}
	if err := c.Release(context.Background(), "res-9"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if releasePath != "/v1/service/credits/holds/res-9/release" {
		t.Fatalf("release path = %s", releasePath)
	}
}

func TestBalance(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(BalanceResponse{AvailableCents: 1234})
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL).Balance(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if resp.AvailableCents != 1234 {
		t.Fatalf("available = %d", resp.AvailableCents)
	}
	if !strings.Contains(gotQuery, "tenant_subject_id=org-1") || strings.Contains(gotQuery, "invoker") {
		t.Fatalf("query = %q", gotQuery)
	}
}

func TestNewRejectsMissingConfig(t *testing.T) {
	if _, err := New(Config{TokenProvider: tokFn("x")}); err == nil {
		t.Fatal("expected error for missing base url")
	}
	if _, err := New(Config{BaseURL: "http://x"}); err == nil {
		t.Fatal("expected error for missing token provider")
	}
}

// #411 [8]: the minted service JWT is the SOLE credential — it is sent on every
// call and there is no OAT fallback.
func TestServiceJWTIsSoleCredential(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(AuthorizeResponse{Allowed: true})
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, Timeout: time.Second, TokenProvider: tokFn("good.jwt")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Authorize(context.Background(), AuthorizeRequest{RequestID: "r"}); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if len(seen) != 1 || seen[0] != "Bearer good.jwt" {
		t.Fatalf("expected a single JWT call, got %v", seen)
	}
}

// #411 [8]: a rejected JWT is NOT retried with anything — the call errors (no
// fallback masks the auth problem).
func TestServiceJWTRejectionErrorsNoFallback(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "service_jwt_invalid"}})
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, Timeout: time.Second, TokenProvider: tokFn("rejected.jwt")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Authorize(context.Background(), AuthorizeRequest{RequestID: "r"}); err == nil {
		t.Fatal("expected an error on a rejected JWT (no fallback)")
	}
	if calls != 1 {
		t.Fatalf("expected exactly one call (no retry), got %d", calls)
	}
}

// #411 [8]: a token-provider failure errors the call (never silently degrades).
func TestServiceJWTMintFailureErrors(t *testing.T) {
	c, err := New(Config{BaseURL: "http://x", Timeout: time.Second, TokenProvider: func(context.Context) (string, error) {
		return "", errInjectedMint
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Authorize(context.Background(), AuthorizeRequest{RequestID: "r"}); err == nil {
		t.Fatal("expected an error when the token provider fails")
	}
}

var errInjectedMint = errors.New("injected mint failure")

func tokFn(s string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return s, nil }
}
