package vault

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	vaultapi "github.com/hashicorp/vault/api"
)

// Pure-logic error paths only (no HTTP is attempted before these fail). The live
// token/approle login behavior is asserted against a REAL Vault container in
// auth_integration_test.go (#724) — never a httptest mock.

func TestLogin_UnsupportedMethod(t *testing.T) {
	if _, _, err := Login(context.Background(), Config{Address: "http://127.0.0.1:1", AuthMethod: "bogus"}); err == nil {
		t.Fatal("expected error for unsupported auth method")
	}
}

// Token auth with no token fails loudly (#712: no ambient VAULT_TOKEN fallback).
func TestLogin_TokenMethodRequiresToken(t *testing.T) {
	if _, _, err := Login(context.Background(), Config{Address: "http://127.0.0.1:1", AuthMethod: "token"}); err == nil {
		t.Fatal("expected error for token auth without a token")
	}
}

// --- resolveApproleSecretID (#751 task 2): the "re-read if file-delivered,
// else static" accessor, pure logic (no Vault needed). ---

func TestResolveApproleSecretID_StaticFallbackWhenNothingMounted(t *testing.T) {
	t.Setenv("VAULT_SECRETS_PATH", t.TempDir()) // empty dir: no VAULT_SECRET_ID file
	got, err := resolveApproleSecretID("static-value")
	if err != nil {
		t.Fatalf("resolveApproleSecretID: %v", err)
	}
	if got != "static-value" {
		t.Fatalf("got %q, want the static fallback", got)
	}
}

func TestResolveApproleSecretID_FileWinsOverStaticWhenNoEnv(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "VAULT_SECRET_ID"), []byte("from-file\n"), 0o600); err != nil {
		t.Fatalf("seed secret file: %v", err)
	}
	t.Setenv("VAULT_SECRETS_PATH", dir)
	got, err := resolveApproleSecretID("static-value")
	if err != nil {
		t.Fatalf("resolveApproleSecretID: %v", err)
	}
	if got != "from-file" {
		t.Fatalf("got %q, want the mounted file's (trimmed) content", got)
	}
}

// Re-reading picks up rotation between calls — the whole point of #751 task 2.
func TestResolveApproleSecretID_FileReReadPicksUpRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "VAULT_SECRET_ID")
	if err := os.WriteFile(path, []byte("v1"), 0o600); err != nil {
		t.Fatalf("seed secret file: %v", err)
	}
	t.Setenv("VAULT_SECRETS_PATH", dir)
	got, err := resolveApproleSecretID("static-value")
	if err != nil || got != "v1" {
		t.Fatalf("first read = (%q, %v), want v1", got, err)
	}
	if err := os.WriteFile(path, []byte("v2"), 0o600); err != nil {
		t.Fatalf("rotate secret file: %v", err)
	}
	got, err = resolveApproleSecretID("static-value")
	if err != nil || got != "v2" {
		t.Fatalf("second read = (%q, %v), want v2 (rotation not picked up)", got, err)
	}
}

// #712 boundary: resolveApproleSecretID must NOT read VAULT_SECRET_ID itself
// (only config.SecretFiles, inside the config package, may read env) — so a
// mounted file wins even when an env var of the same name is ALSO set. This
// is the documented corner case on resolveApproleSecretID's doc comment: the
// file and env conventions are alternative delivery mechanisms for the same
// slot, not meant to be combined.
func TestResolveApproleSecretID_FileWinsEvenWhenEnvVarAlsoSet(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "VAULT_SECRET_ID"), []byte("from-file"), 0o600); err != nil {
		t.Fatalf("seed secret file: %v", err)
	}
	t.Setenv("VAULT_SECRETS_PATH", dir)
	t.Setenv("VAULT_SECRET_ID", "from-env")
	got, err := resolveApproleSecretID("static-value")
	if err != nil {
		t.Fatalf("resolveApproleSecretID: %v", err)
	}
	if got != "from-file" {
		t.Fatalf("got %q, want the mounted file's content (this package never reads env directly, #712)", got)
	}
}

// --- IsPermissionDenied (#751 task 5 classification), pure logic. ---

func TestIsPermissionDenied(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"403 ResponseError", &vaultapi.ResponseError{StatusCode: 403, Errors: []string{"permission denied"}}, true},
		{"404 ResponseError", &vaultapi.ResponseError{StatusCode: 404}, false},
		{"wrapped 403", errWrap{&vaultapi.ResponseError{StatusCode: 403}}, true},
		{"plain text fallback", plainErr("1 error occurred:\n\t* permission denied\n"), true},
		{"unrelated error", plainErr("connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsPermissionDenied(tc.err); got != tc.want {
				t.Errorf("IsPermissionDenied(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

type plainErr string

func (e plainErr) Error() string { return string(e) }

// errWrap mimics fmt.Errorf("...: %w", err)'s Unwrap shape without pulling in fmt.
type errWrap struct{ err error }

func (e errWrap) Error() string { return "wrapped: " + e.err.Error() }
func (e errWrap) Unwrap() error { return e.err }
