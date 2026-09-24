package vault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"

	vaultapi "github.com/hashicorp/vault/api"
)

func TestLoginRefusesBeforeNetwork(t *testing.T) {
	for _, cfg := range []Config{
		{Address: "http://127.0.0.1:1", AuthMethod: "bogus"},
		{Address: "http://127.0.0.1:1", AuthMethod: "token"}, // #712: no ambient VAULT_TOKEN fallback
	} {
		if _, _, err := Login(context.Background(), cfg); err == nil {
			t.Fatalf("Login(%+v) must fail", cfg)
		}
	}
}

// #751: the AppRole secret id is re-read from the mounted file on every attempt
// (rotation), and this package never reads VAULT_SECRET_ID from env (#712).
func TestResolveApproleSecretID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VAULT_SECRETS_PATH", dir)
	t.Setenv("VAULT_SECRET_ID", "from-env")
	if got, err := resolveApproleSecretID("static"); err != nil || got != "static" {
		t.Fatalf("nothing mounted = (%q, %v), want static fallback", got, err)
	}
	path := filepath.Join(dir, "VAULT_SECRET_ID")
	for content, want := range map[string]string{"v1\n": "v1", "v2": "v2"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := resolveApproleSecretID("static"); err != nil || got != want {
			t.Fatalf("mounted %q = (%q, %v), want %q", content, got, err, want)
		}
	}
}

func TestIsPermissionDenied(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{&vaultapi.ResponseError{StatusCode: 403}, true},
		{fmt.Errorf("wrapped: %w", &vaultapi.ResponseError{StatusCode: 403}), true},
		{&vaultapi.ResponseError{StatusCode: 404, Errors: []string{"permission denied"}}, false},
		{errors.New("1 error occurred:\n\t* Permission Denied\n"), true},
		{errors.New("connection refused"), false},
	} {
		if got := IsPermissionDenied(tc.err); got != tc.want {
			t.Errorf("IsPermissionDenied(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func newKV(t *testing.T, h http.HandlerFunc) *KVv2Adapter {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg := vaultapi.DefaultConfig()
	cfg.Address, cfg.MaxRetries = srv.URL, 0
	client, err := vaultapi.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	client.SetToken("test-token")
	client.SetNamespace("")
	return NewKVv2Adapter(client, " /secret/ ")
}

func TestKVv2PathMapping(t *testing.T) {
	a := NewKVv2Adapter(nil, "secret")
	const rest = "openrails/merchants/m1/psps/solana/live/AKnL4NNf/private_key"
	if a.rest("secret/"+rest) != rest || a.dataPath("secret/"+rest) != "secret/data/"+rest || a.metadataPath("secret/"+rest) != "secret/metadata/"+rest {
		t.Fatalf("mapping: %q %q %q", a.rest("secret/"+rest), a.dataPath("secret/"+rest), a.metadataPath("secret/"+rest))
	}
	if a.BackendIdentity() != "" {
		t.Fatal("nil client has no identity")
	}
}

func TestKVv2ReadWriteDelete(t *testing.T) {
	var got []string
	var body map[string]any
	a := newKV(t, func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		if r.Header.Get("X-Vault-Token") != "test-token" {
			t.Errorf("missing token header")
		}
		switch {
		case r.URL.Path == "/v1/secret/data/m1/missing":
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"errors":[]}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":{"data":{"value":"s3cret","n":7},"metadata":{"version":3}}}`))
		case r.Method == http.MethodPut:
			body = nil
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, _ = w.Write([]byte(`{"data":{"version":4}}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(204)
		}
	})
	ctx := context.Background()

	data, version, err := a.ReadSecretVersion(ctx, "secret/m1/key", 3)
	if err != nil || version != 3 || len(data) != 1 || data["value"] != "s3cret" {
		t.Fatalf("read = %v %d %v (non-string values must be dropped)", data, version, err)
	}
	if data, version, err := a.ReadSecret(ctx, "secret/m1/missing"); data != nil || version != 0 || err != nil {
		t.Fatalf("absent must be (nil,0,nil), got %v %d %v", data, version, err)
	}
	if _, _, err := a.ReadSecretVersion(ctx, "secret/m1/key", -1); err == nil {
		t.Fatal("negative version must be refused")
	}

	if v, err := a.WriteSecretCAS(ctx, "secret/m1/key", map[string]string{"value": "x"}, 3); err != nil || v != 4 {
		t.Fatalf("cas write = %d %v", v, err)
	}
	if opts, _ := body["options"].(map[string]any); opts["cas"] != float64(3) || body["data"].(map[string]any)["value"] != "x" {
		t.Fatalf("cas body = %v", body)
	}
	if _, err := a.WriteSecret(ctx, "secret/m1/key", map[string]string{"value": "y"}); err != nil || body["options"] != nil {
		t.Fatalf("plain write must not send options: %v %v", body, err)
	}
	if _, err := a.WriteSecretCAS(ctx, "secret/m1/key", nil, -1); err == nil {
		t.Fatal("negative CAS version must be refused")
	}
	if err := a.DeleteSecret(ctx, "secret/m1/key"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET /v1/secret/data/m1/key?version=3",
		"GET /v1/secret/data/m1/missing?",
		"PUT /v1/secret/data/m1/key?",
		"PUT /v1/secret/data/m1/key?",
		"DELETE /v1/secret/metadata/m1/key?", // purges all versions
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("requests:\n got %v\nwant %v", got, want)
	}
}

// KV-v2 LIST is one level; the store contract is full relative leaf names, and a
// path may be both a leaf and a directory.
func TestKVv2ListRecursesToLeaves(t *testing.T) {
	tree := map[string]string{
		"/v1/secret/metadata/m1":               `["psps/","webhook_secret"]`,
		"/v1/secret/metadata/m1/psps":          `["nmi","nmi/"]`,
		"/v1/secret/metadata/m1/psps/nmi":      `["live/"]`,
		"/v1/secret/metadata/m1/psps/nmi/live": `["api_key"]`,
	}
	a := newKV(t, func(w http.ResponseWriter, r *http.Request) {
		keys, ok := tree[r.URL.Path]
		if r.URL.Query().Get("list") != "true" || !ok {
			w.WriteHeader(404)
			return
		}
		_, _ = fmt.Fprintf(w, `{"data":{"keys":%s}}`, keys)
	})
	names, err := a.ListSecrets(context.Background(), "secret/m1/")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	if fmt.Sprint(names) != "[psps/nmi psps/nmi/live/api_key webhook_secret]" {
		t.Fatalf("names = %v", names)
	}
}

// Credential errors are typed so callers fail closed and a wired supervisor
// hears every failure (#751 task 5).
func TestKVv2ErrorsAreTypedAndReported(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   error
	}{
		{403, `{"errors":["permission denied"]}`, ErrPermissionDenied},
		{503, `{"errors":["Vault is sealed"]}`, ErrSealed},
		{503, `{"errors":["standby"]}`, ErrUnavailable},
		{500, `{"errors":["boom"]}`, ErrUnavailable},
	} {
		a := newKV(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		})
		var reported []error
		a.onPermissionDenied = func(err error) { reported = append(reported, err) }
		ctx := context.Background()
		if _, _, err := a.ReadSecret(ctx, "secret/k"); !errors.Is(err, tc.want) {
			t.Errorf("%d read: got %v want %v", tc.status, err, tc.want)
		}
		if _, err := a.WriteSecret(ctx, "secret/k", map[string]string{"v": "1"}); !errors.Is(err, tc.want) {
			t.Errorf("%d write: got %v want %v", tc.status, err, tc.want)
		}
		_ = a.DeleteSecret(ctx, "secret/k")
		_, _ = a.ListSecrets(ctx, "secret")
		if len(reported) != 4 || IsPermissionDenied(reported[0]) != (tc.status == 403) {
			t.Errorf("%d: reported %v", tc.status, reported)
		}
	}
}
