package middleware

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-rails/openrails/internal/archivewire"
)

type unreadArchive struct{ t *testing.T }

func (r unreadArchive) Read([]byte) (int, error) {
	r.t.Error("unauthenticated archive body was read")
	return 0, io.EOF
}

func TestArchiveBodyLimitAuthenticatesBeforeReading(t *testing.T) {
	h := BodyLimitHTTP(DefaultMaxBodyBytes)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	r := httptest.NewRequest(http.MethodPost, "/host/billing/v1/merchant/billing-archive", unreadArchive{t})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestArchiveBodyLimitKeepsOrdinaryRoutesBounded(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 2<<20)
	h := BodyLimitHTTP(DefaultMaxBodyBytes)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil || n != int64(len(body)) {
			t.Errorf("archive body truncated: bytes=%d err=%v", n, err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, test := range []struct {
		method, path string
		status       int
	}{
		{http.MethodPost, "/v1/merchant/billing-archive", http.StatusNoContent},
		{http.MethodPost, "/host/v1/merchant/billing-archive", http.StatusNoContent},
		{http.MethodPost, "/v1/import/billing", http.StatusRequestEntityTooLarge},
		{http.MethodPost, "/v1/merchant/billing-archive/other", http.StatusRequestEntityTooLarge},
		{http.MethodPost, "/unrelated/../v1/merchant/billing-archive", http.StatusRequestEntityTooLarge},
		{http.MethodPost, "//v1/merchant/billing-archive", http.StatusRequestEntityTooLarge},
		{http.MethodPost, "/v1/merchant/%62illing-archive", http.StatusRequestEntityTooLarge},
		{http.MethodGet, "/v1/merchant/billing-archive", http.StatusRequestEntityTooLarge},
	} {
		t.Run(test.method+test.path, func(t *testing.T) {
			r := httptest.NewRequest(test.method, test.path, bytes.NewReader(body))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}

	called := false
	h = BodyLimitHTTP(DefaultMaxBodyBytes)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	r := httptest.NewRequest(http.MethodPost, "/v1/merchant/billing-archive", strings.NewReader("unused"))
	r.ContentLength = archivewire.MaxBytes + 1
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if called || w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), `"code":"request_body_too_large"`) {
		t.Fatalf("oversized archive reached handler or lost envelope: called=%t status=%d body=%s", called, w.Code, w.Body.String())
	}
}
