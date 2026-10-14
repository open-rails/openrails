package basistheory

import (
	"io"
	"net/http"
	"testing"
)

func TestTenantID(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"private application", http.StatusOK, `{"tenant_id":"tenant-a","type":"private"}`, "tenant-a"},
		{"missing identity", http.StatusOK, `{}`, ""},
		{"empty response", http.StatusNoContent, "", ""},
		{"invalid response", http.StatusOK, `{"tenant_id":`, ""},
		{"forbidden", http.StatusForbidden, `{"title":"forbidden"}`, ""},
		{"not found", http.StatusNotFound, `{"title":"not found"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := newTestClient(t, Config{ReadOnly: true}, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/applications/key" || r.Header.Get(apiKeyHeader) != "key_test" {
					t.Errorf("unexpected identity request: %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			got, err := client.TenantID(t.Context())
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("TenantID() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
