// Package vaulttest gives an end-to-end test its own KV v2 mount on the
// suite's dev Vault, as every test gets its own PostgreSQL schema. The Vault
// is named by OPENRAILS_E2E_VAULT_ADDR and OPENRAILS_E2E_VAULT_TOKEN (a root
// token); a test that needs it is skipped when the address is unset.
package vaulttest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/open-rails/openrails/internal/config"
)

const (
	AddrEnv  = "OPENRAILS_E2E_VAULT_ADDR"
	TokenEnv = "OPENRAILS_E2E_VAULT_TOKEN"
)

// ErrCASMismatch is PutCAS's refusal: version is not the document's current one.
var ErrCASMismatch = errors.New("vaulttest: check-and-set version mismatch")

// Vault is one test's KV v2 mount.
type Vault struct {
	Addr  string
	Token string
	Mount string

	client *vaultapi.Client
}

// New mounts a fresh KV v2 engine for t and removes it, with every document
// in it, when t ends.
func New(t testing.TB) *Vault {
	t.Helper()
	addr := strings.TrimSpace(os.Getenv(AddrEnv))
	if addr == "" {
		t.Skip(AddrEnv + " is unset: this test needs a dev Vault (docs/dev/testing.md)")
	}
	token := strings.TrimSpace(os.Getenv(TokenEnv))
	if token == "" {
		t.Fatal(TokenEnv + " must hold the root token of the Vault at " + AddrEnv)
	}
	cfg := vaultapi.DefaultConfig()
	cfg.Address = addr
	client, err := vaultapi.NewClient(cfg)
	if err != nil {
		t.Fatalf("vaulttest: client: %v", err)
	}
	client.SetToken(token)

	suffix := make([]byte, 8)
	_, _ = rand.Read(suffix)
	v := &Vault{Addr: addr, Token: token, Mount: "e2e-" + hex.EncodeToString(suffix), client: client}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := client.Sys().MountWithContext(ctx, v.Mount, &vaultapi.MountInput{Type: "kv", Options: map[string]string{"version": "2"}}); err != nil {
		t.Fatalf("vaulttest: mount %s at %s: %v", v.Mount, addr, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := client.Sys().UnmountWithContext(ctx, v.Mount); err != nil {
			t.Errorf("vaulttest: unmount %s: %v", v.Mount, err)
		}
	})
	// A new KV v2 mount refuses requests until its version metadata is set up.
	for {
		_, err := client.Logical().ReadWithDataWithContext(ctx, v.Mount+"/data/ready", nil)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("vaulttest: mount %s never became ready: %v", v.Mount, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return v
}

// Config connects OpenRails to this Vault (Config.Vault), the test's mount
// its KV mount.
func (v *Vault) Config() *config.VaultConfig {
	return &config.VaultConfig{Address: v.Addr, Token: v.Token, KVMount: v.Mount}
}

// Client is a root client of this Vault (Deps.Vault, or a test's own calls).
func (v *Vault) Client() *vaultapi.Client { return v.client }

// Put writes doc, which must encode as a JSON object, at path under the
// mount and returns its new version.
func (v *Vault) Put(t testing.TB, path string, doc any) int {
	t.Helper()
	version, err := v.write(t, path, doc, nil)
	if err != nil {
		t.Fatalf("vaulttest: put %s: %v", path, err)
	}
	return version
}

// PutCAS writes doc only when version is path's current version (0: path
// holds nothing yet) and returns the new version. A mismatch is
// ErrCASMismatch; any other failure fails t.
func (v *Vault) PutCAS(t testing.TB, path string, doc any, version int) (int, error) {
	t.Helper()
	next, err := v.write(t, path, doc, &version)
	var response *vaultapi.ResponseError
	if errors.As(err, &response) && response.StatusCode == http.StatusBadRequest && strings.Contains(strings.Join(response.Errors, " "), "check-and-set") {
		return 0, ErrCASMismatch
	}
	if err != nil {
		t.Fatalf("vaulttest: put %s at version %d: %v", path, version, err)
	}
	return next, nil
}

// Get decodes path's current version into out and returns that version; 0,
// leaving out untouched, when path holds nothing.
func (v *Vault) Get(t testing.TB, path string, out any) int {
	t.Helper()
	sec, err := v.client.Logical().ReadWithDataWithContext(t.Context(), v.dataPath(path), nil)
	if err != nil {
		t.Fatalf("vaulttest: get %s: %v", path, err)
	}
	if sec == nil || sec.Data["data"] == nil {
		return 0
	}
	raw, err := json.Marshal(sec.Data["data"])
	if err == nil {
		err = json.Unmarshal(raw, out)
	}
	if err != nil {
		t.Fatalf("vaulttest: decode %s: %v", path, err)
	}
	return version(t, sec.Data["metadata"])
}

func (v *Vault) write(t testing.TB, path string, doc any, cas *int) (int, error) {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil || !strings.HasPrefix(string(raw), "{") {
		t.Fatalf("vaulttest: %s: a document is a JSON object, got %T (%v)", path, doc, err)
	}
	body := map[string]any{"data": json.RawMessage(raw)}
	if cas != nil {
		body["options"] = map[string]any{"cas": *cas}
	}
	sec, err := v.client.Logical().WriteWithContext(t.Context(), v.dataPath(path), body)
	if err != nil {
		return 0, err
	}
	if sec == nil {
		t.Fatalf("vaulttest: put %s: no version in the response", path)
	}
	return version(t, sec.Data), nil
}

func (v *Vault) dataPath(path string) string {
	return v.Mount + "/data/" + strings.Trim(path, "/")
}

func version(t testing.TB, meta any) int {
	t.Helper()
	m, _ := meta.(map[string]any)
	n, ok := m["version"].(json.Number)
	if !ok {
		t.Fatalf("vaulttest: no version in %v", meta)
	}
	i, err := n.Int64()
	if err != nil {
		t.Fatalf("vaulttest: version %q: %v", n, err)
	}
	return int(i)
}
