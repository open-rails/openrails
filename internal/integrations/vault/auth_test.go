package vault

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveApproleSecretID(t *testing.T) {
	dir := t.TempDir()
	got, err := resolveApproleSecretID(dir, "boot")
	if err != nil || got != "boot" {
		t.Fatalf("empty directory: got %q, %v; want the configured secret", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "VAULT_SECRET_ID"), []byte(" rotated \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err = resolveApproleSecretID(dir, "boot"); err != nil || got != "rotated" {
		t.Fatalf("mounted file: got %q, %v; want the file's value", got, err)
	}
	if got, err = resolveApproleSecretID("", "boot"); err != nil || got != "boot" {
		t.Fatalf("no secrets path: got %q, %v; want the configured secret", got, err)
	}
	if got, err = resolveApproleSecretID(filepath.Join(dir, "absent"), "boot"); err != nil || got != "boot" {
		t.Fatalf("absent directory: got %q, %v; want the configured secret", got, err)
	}
}
