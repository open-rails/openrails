package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/openrails/internal/http/request"
)

func TestMuxRouterHandleAndParam(t *testing.T) {
	mux := http.NewServeMux()
	r := NewMux(mux, "/billing/v1", nil)
	r.Handle(http.MethodGet, "/me/subscriptions/:id", func(req *request.Request) {
		req.SuccessJSONMessage(req.Param("id"))
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/billing/v1/me/subscriptions/sub_123", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got == "" || !contains(got, "sub_123") {
		t.Fatalf("body %q does not contain path param sub_123", got)
	}
}

func TestMuxRouterMiddlewareOrderAndShortCircuit(t *testing.T) {
	mux := http.NewServeMux()
	var order []string
	mwA := func(next Handler) Handler {
		return func(r *request.Request) { order = append(order, "A"); next(r) }
	}
	mwB := func(next Handler) Handler {
		return func(r *request.Request) { order = append(order, "B"); next(r) }
	}
	r := NewMux(mux, "", nil).Group("/g", mwA)
	r.Handle(http.MethodGet, "/x", func(req *request.Request) {
		order = append(order, "H")
		req.SuccessJSONMessage("ok")
	}, mwB)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/g/x", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(order) != 3 || order[0] != "A" || order[1] != "B" || order[2] != "H" {
		t.Fatalf("order = %v, want [A B H]", order)
	}

	// A middleware that aborts must stop the chain (handler not reached).
	mux2 := http.NewServeMux()
	reached := false
	abort := func(next Handler) Handler {
		return func(r *request.Request) { r.AbortJSON(http.StatusForbidden, "no") }
	}
	r2 := NewMux(mux2, "", nil)
	r2.Handle(http.MethodGet, "/y", func(req *request.Request) {
		reached = true
		req.SuccessJSONMessage("ok")
	}, abort)
	rec2 := httptest.NewRecorder()
	mux2.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/y", nil))
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec2.Code)
	}
	if reached {
		t.Fatal("handler reached despite aborting middleware")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
