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

func TestKVv2Documents(t *testing.T) {
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
		case r.URL.Path == "/v1/secret/data/m1/stale":
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"errors":["check-and-set parameter did not match the current version"]}`))
		case r.Method == http.MethodGet && r.URL.Query().Get("list") == "true":
			_, _ = w.Write([]byte(`{"data":{"keys":["merchant","psps/"]}}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":{"data":{"rail":"nmi"},"metadata":{"version":3,"created_time":"2026-10-09T10:00:00Z"}}}`))
		case r.Method == http.MethodPut || r.Method == http.MethodPost:
			body = nil
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, _ = w.Write([]byte(`{"data":{"version":4,"created_time":"2026-10-09T10:01:00Z"}}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(204)
		}
	})
	ctx := context.Background()

	doc, err := a.ReadDocument(ctx, "m1/key")
	if err != nil || !doc.Found || doc.Version != 3 || string(doc.Data) != `{"rail":"nmi"}` || doc.Updated.IsZero() {
		t.Fatalf("read = %+v %v", doc, err)
	}
	if doc, err := a.ReadDocument(ctx, "m1/missing"); doc.Found || doc.Version != 0 || err != nil {
		t.Fatalf("absent must be not found, got %+v %v", doc, err)
	}
	written, err := a.WriteDocument(ctx, "m1/key", json.RawMessage(`{"rail":"nmi"}`), 3)
	if err != nil || written.Version != 4 {
		t.Fatalf("cas write = %+v %v", written, err)
	}
	if opts, _ := body["options"].(map[string]any); opts["cas"] != float64(3) || body["data"].(map[string]any)["rail"] != "nmi" {
		t.Fatalf("cas body = %v", body)
	}
	if _, err := a.WriteDocument(ctx, "m1/stale", json.RawMessage(`{}`), 1); !errors.Is(err, ErrCASMismatch) {
		t.Fatalf("a stale check-and-set must be ErrCASMismatch, got %v", err)
	}
	if _, err := a.WriteDocument(ctx, "m1/key", json.RawMessage(`[]`), 0); err == nil {
		t.Fatal("a document must be an object")
	}
	if _, err := a.WriteDocument(ctx, "m1/key", json.RawMessage(`{}`), -1); err == nil {
		t.Fatal("negative CAS version must be refused")
	}
	if names, err := a.ListDocuments(ctx, "m1"); err != nil || fmt.Sprint(names) != "[merchant psps/]" {
		t.Fatalf("list = %v %v", names, err)
	}
	if err := a.DeleteDocument(ctx, "m1/key"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET /v1/secret/data/m1/key?",
		"GET /v1/secret/data/m1/missing?",
		"PUT /v1/secret/data/m1/key?",
		"PUT /v1/secret/data/m1/stale?",
		"GET /v1/secret/metadata/m1?list=true",
		"DELETE /v1/secret/metadata/m1/key?", // purges all versions
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("requests:\n got %v\nwant %v", got, want)
	}
}

// Backend errors are typed so callers fail closed and a wired supervisor
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
		if _, err := a.ReadDocument(ctx, "k"); !errors.Is(err, tc.want) {
			t.Errorf("%d read: got %v want %v", tc.status, err, tc.want)
		}
		if _, err := a.WriteDocument(ctx, "k", json.RawMessage(`{"v":"1"}`), 0); !errors.Is(err, tc.want) {
			t.Errorf("%d write: got %v want %v", tc.status, err, tc.want)
		}
		_ = a.DeleteDocument(ctx, "k")
		_, _ = a.ListDocuments(ctx, "")
		if len(reported) != 4 || IsPermissionDenied(reported[0]) != (tc.status == 403) {
			t.Errorf("%d: reported %v", tc.status, reported)
		}
	}
}
