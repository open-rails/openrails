package vault

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	vaultapi "github.com/hashicorp/vault/api"
)

// fakeKVv2 is an in-memory mock of the Vault KV-v2 HTTP API surface the adapter
// uses: PUT/GET on /v1/<mount>/data/<rest> and DELETE/LIST on
// /v1/<mount>/metadata/<rest>. It lets us prove the live KVv2Adapter's Get/Set/
// Delete/List round-trip AND the exact path layout WITHOUT a real Vault.
type fakeKVv2 struct {
	mu    sync.Mutex
	store map[string]map[string]string // data-path -> inner key/value map
	t     *testing.T
	mount string
	seen  map[string]string // method -> last full request path (for path-layout assertions)
}

func newFakeKVv2(t *testing.T, mount string) *httptest.Server {
	f := &fakeKVv2{store: map[string]map[string]string{}, t: t, mount: mount, seen: map[string]string{}}
	return httptest.NewServer(f)
}

func (f *fakeKVv2) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen[r.Method+" "+r.URL.Path] = r.URL.Path

	// The Vault API client issues LIST as GET with ?list=true.
	isList := r.URL.Query().Get("list") == "true"

	switch {
	case isList && hasSeg(r.URL.Path, f.mount, "metadata"):
		f.seen["LIST "+r.URL.Path] = r.URL.Path
		writeJSON(w, map[string]any{"data": map[string]any{"keys": []string{"alpha", "beta"}}})
	case r.Method == http.MethodPut && hasSeg(r.URL.Path, f.mount, "data"):
		var body struct {
			Data map[string]string `json:"data"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		f.store[r.URL.Path] = body.Data
		writeJSON(w, map[string]any{"data": map[string]any{"version": 1}})
	case r.Method == http.MethodGet && hasSeg(r.URL.Path, f.mount, "data"):
		inner, ok := f.store[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			writeJSON(w, map[string]any{"errors": []string{}})
			return
		}
		writeJSON(w, map[string]any{"data": map[string]any{"data": inner}})
	case r.Method == http.MethodDelete && hasSeg(r.URL.Path, f.mount, "metadata"):
		// metadata delete purges all versions; map the metadata path to its data path.
		dataPath := replaceSeg(r.URL.Path, "metadata", "data")
		delete(f.store, dataPath)
		w.WriteHeader(http.StatusNoContent)
	default:
		f.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
	}
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func hasSeg(path, mount, seg string) bool {
	return contains(path, "/v1/"+mount+"/"+seg+"/")
}

func replaceSeg(path, from, to string) string {
	return replaceOnce(path, "/"+from+"/", "/"+to+"/")
}

// adapterAgainst builds a real KVv2Adapter pointed at the httptest server.
func adapterAgainst(t *testing.T, srvURL, mount string) *KVv2Adapter {
	t.Helper()
	cfg := vaultapi.DefaultConfig()
	cfg.Address = srvURL
	cl, err := vaultapi.NewClient(cfg)
	if err != nil {
		t.Fatalf("vault client: %v", err)
	}
	cl.SetToken("test-token")
	return NewKVv2Adapter(cl, mount)
}

func TestKVv2Adapter_RoundTrip_HTTPMock(t *testing.T) {
	ctx := context.Background()
	const mount = "secret"
	srv := newFakeKVv2(t, mount)
	defer srv.Close()
	a := adapterAgainst(t, srv.URL, mount)

	const full = "secret/openrails/merchants/00000000-0000-0000-0000-000000000001/stripe/secret_key"

	// Set
	if err := a.WriteSecret(ctx, full, map[string]string{"value": "sk_live_x"}); err != nil {
		t.Fatalf("WriteSecret: %v", err)
	}
	// Get
	got, err := a.ReadSecret(ctx, full)
	if err != nil {
		t.Fatalf("ReadSecret: %v", err)
	}
	if got["value"] != "sk_live_x" {
		t.Fatalf("round-trip value = %q, want sk_live_x", got["value"])
	}

	// Path layout: the data write/read must hit /v1/secret/data/openrails/merchants/<id>/stripe/secret_key
	wantData := "/v1/secret/data/openrails/merchants/00000000-0000-0000-0000-000000000001/stripe/secret_key"
	f := srv.Config.Handler.(*fakeKVv2)
	f.mu.Lock()
	_, putSeen := f.seen["PUT "+wantData]
	_, getSeen := f.seen["GET "+wantData]
	f.mu.Unlock()
	if !putSeen {
		t.Errorf("PUT did not hit expected KV-v2 data path %q (path layout broken)", wantData)
	}
	if !getSeen {
		t.Errorf("GET did not hit expected KV-v2 data path %q (path layout broken)", wantData)
	}

	// Delete purges via the metadata endpoint, then Get returns empty (not found).
	if err := a.DeleteSecret(ctx, full); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
	wantMeta := "/v1/secret/metadata/openrails/merchants/00000000-0000-0000-0000-000000000001/stripe/secret_key"
	f.mu.Lock()
	_, delSeen := f.seen["DELETE "+wantMeta]
	f.mu.Unlock()
	if !delSeen {
		t.Errorf("DELETE did not hit expected KV-v2 metadata path %q", wantMeta)
	}
	after, err := a.ReadSecret(ctx, full)
	if err != nil {
		t.Fatalf("ReadSecret after delete: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("after delete got %v, want empty (maps to ErrSecretNotFound)", after)
	}
}

func TestKVv2Adapter_List_HTTPMock(t *testing.T) {
	ctx := context.Background()
	const mount = "secret"
	srv := newFakeKVv2(t, mount)
	defer srv.Close()
	a := adapterAgainst(t, srv.URL, mount)

	names, err := a.ListSecrets(ctx, "secret/openrails/merchants/tenant-x")
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if len(names) != 2 || names[0] != "alpha" || names[1] != "beta" {
		t.Fatalf("ListSecrets = %v, want [alpha beta]", names)
	}
}

// --- tiny string helpers (avoid importing strings just for two calls) ---

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func replaceOnce(s, from, to string) string {
	for i := 0; i+len(from) <= len(s); i++ {
		if s[i:i+len(from)] == from {
			return s[:i] + to + s[i+len(from):]
		}
	}
	return s
}
