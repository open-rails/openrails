package billingauth_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/openrails/pkg/billingauth"
)

// captureUC is a terminal handler that records the UserContext it sees (if any).
func captureUC(seen *billingauth.UserContext, found *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uc, ok := billingauth.FromContext(r.Context())
		*seen, *found = uc, ok
		w.WriteHeader(http.StatusOK)
	})
}

func TestRequired(t *testing.T) {
	const uid = "user-123"

	t.Run("success attaches UserContext and calls next", func(t *testing.T) {
		var seen billingauth.UserContext
		var found bool
		auth := billingauth.AuthenticatorFunc(func(_ context.Context, _ *http.Request) (billingauth.UserContext, error) {
			return billingauth.UserContext{UserID: uid, Org: "acme"}, nil
		})
		h := billingauth.Required(auth)(captureUC(&seen, &found))

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if !found || seen.UserID != uid || seen.Org != "acme" {
			t.Fatalf("next saw uc=%+v found=%v, want UserID=%q Org=acme", seen, found, uid)
		}
	})

	t.Run("error yields 401 and does not call next", func(t *testing.T) {
		called := false
		auth := billingauth.AuthenticatorFunc(func(_ context.Context, _ *http.Request) (billingauth.UserContext, error) {
			return billingauth.UserContext{}, billingauth.ErrUnauthenticated
		})
		h := billingauth.Required(auth)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if called {
			t.Fatal("next was called despite auth failure")
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("response not JSON: %v", err)
		}
		if body["object"] != "error" {
			t.Fatalf("error envelope = %v, want object=error", body)
		}
	})

	t.Run("custom error message is surfaced", func(t *testing.T) {
		auth := billingauth.AuthenticatorFunc(func(_ context.Context, _ *http.Request) (billingauth.UserContext, error) {
			return billingauth.UserContext{}, errors.New("token expired")
		})
		h := billingauth.Required(auth)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		var body struct {
			Error struct{ Message string } `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.Error.Message != "token expired" {
			t.Fatalf("message = %q, want 'token expired'", body.Error.Message)
		}
	})

	t.Run("nil authenticator is a 500, not a panic", func(t *testing.T) {
		h := billingauth.Required(nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
}

func TestOptional(t *testing.T) {
	t.Run("auth failure proceeds anonymously", func(t *testing.T) {
		var found bool
		var seen billingauth.UserContext
		auth := billingauth.AuthenticatorFunc(func(_ context.Context, _ *http.Request) (billingauth.UserContext, error) {
			return billingauth.UserContext{}, billingauth.ErrUnauthenticated
		})
		h := billingauth.Optional(auth)(captureUC(&seen, &found))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (optional)", rec.Code)
		}
		if found {
			t.Fatal("UserContext present despite auth failure on Optional")
		}
	})

	t.Run("auth success attaches UserContext", func(t *testing.T) {
		var found bool
		var seen billingauth.UserContext
		auth := billingauth.AuthenticatorFunc(func(_ context.Context, _ *http.Request) (billingauth.UserContext, error) {
			return billingauth.UserContext{UserID: "u1"}, nil
		})
		h := billingauth.Optional(auth)(captureUC(&seen, &found))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		if !found || seen.UserID != "u1" {
			t.Fatalf("uc=%+v found=%v, want UserID=u1", seen, found)
		}
	})
}
