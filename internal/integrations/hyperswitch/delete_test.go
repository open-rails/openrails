package hyperswitch

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestNativeDeleteRequiresCurrentCapabilityAndExactCompletion(t *testing.T) {
	for _, tc := range []struct {
		name, capability, response string
		status                     int
		readonly, lose             bool
		want                       error
		writes                     int
	}{
		{name: "native completion with money routes disabled", capability: "openrails-native-vault-delete-v1", response: `{"id":"pm_owned"}`, status: 200, writes: 1},
		{name: "old deployment", status: 200, want: ErrUnavailable},
		{name: "unknown capability", capability: "future-version", status: 200, want: ErrUnavailable},
		{name: "read only", capability: "openrails-native-vault-delete-v1", readonly: true, want: ErrReadOnly},
		{name: "logical absence is not erasure", capability: "openrails-native-vault-delete-v1", status: 404, want: ErrUnknown, writes: 1},
		{name: "vendor refusal is not erasure", capability: "openrails-native-vault-delete-v1", status: 400, want: ErrUnknown, writes: 1},
		{name: "vault unavailable", capability: "openrails-native-vault-delete-v1", status: 500, want: ErrUnknown, writes: 1},
		{name: "another method", capability: "openrails-native-vault-delete-v1", response: `{"id":"pm_other"}`, status: 200, want: ErrUnknown, writes: 1},
		{name: "lost success", capability: "openrails-native-vault-delete-v1", lose: true, want: ErrUnknown, writes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "api-key=synthetic" || r.Header.Get("x-profile-id") != "profile_owned" {
					t.Error("request lost its account authority")
				}
				if r.Method == http.MethodGet && r.URL.Path == "/v2/proxy" {
					_ = json.NewEncoder(w).Encode(map[string]any{"contract": "openrails-nmi-form-v2", "native_vault_delete_contract": tc.capability, "strict": false, "routes": []string{}})
					return
				}
				if r.Method != http.MethodDelete || r.URL.Path != "/v2/payment-methods/pm_owned" {
					t.Error("request changed deletion target")
					w.WriteHeader(400)
					return
				}
				writes.Add(1)
				if tc.lose {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.response))
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, MerchantID: "merchant_owned", ProfileID: "profile_owned", APIKey: "synthetic", ReadOnly: tc.readonly})
			if err != nil {
				t.Fatal(err)
			}
			if got := client.DeleteMethod(t.Context(), "pm_owned"); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			if writes.Load() != int32(tc.writes) {
				t.Fatalf("got %d writes, want %d", writes.Load(), tc.writes)
			}
		})
	}
}
