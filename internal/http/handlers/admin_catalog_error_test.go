package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// TestWriteCatalogError pins the #783 error-hygiene contract: domain errors map
// to stable 4xx codes and raw sql/pgx internals never reach the client.
func TestWriteCatalogError(t *testing.T) {
	cases := []struct {
		name           string
		err            error
		wantStatus     int
		mustContain    string
		mustNotContain string
	}{
		{
			name:        "not found → 404",
			err:         errors.New("price not found"),
			wantStatus:  http.StatusNotFound,
			mustContain: "not found",
		},
		{
			name:        "negative amount → 400 (was 500)",
			err:         errors.New("unit_amount must be non-negative"),
			wantStatus:  http.StatusBadRequest,
			mustContain: "non-negative",
		},
		{
			name:           "unique violation → 409, no raw SQLSTATE leak",
			err:            errors.New(`ERROR: duplicate key value violates unique constraint "unique_prices_product_amount_window" (SQLSTATE 23505)`),
			wantStatus:     http.StatusConflict,
			mustContain:    "already exists",
			mustNotContain: "SQLSTATE",
		},
		{
			name:           "unexpected error → 500 generic, no raw text leak",
			err:            errors.New("boom: internal detail that must not leak"),
			wantStatus:     http.StatusInternalServerError,
			mustContain:    "internal error",
			mustNotContain: "boom",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httprequest.NewHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil), nil)
			writeCatalogError(r, tc.err)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status: got %d want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			body := rec.Body.String()
			if tc.mustContain != "" && !strings.Contains(body, tc.mustContain) {
				t.Fatalf("body %q must contain %q", body, tc.mustContain)
			}
			if tc.mustNotContain != "" && strings.Contains(body, tc.mustNotContain) {
				t.Fatalf("body %q must NOT leak %q", body, tc.mustNotContain)
			}
		})
	}
}
