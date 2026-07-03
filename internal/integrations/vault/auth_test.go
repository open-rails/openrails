package vault

import (
	"context"
	"testing"
)

// Pure-logic error paths only (no HTTP is attempted before these fail). The live
// token/approle login behavior is asserted against a REAL Vault container in
// auth_integration_test.go (#724) — never a httptest mock.

func TestLogin_UnsupportedMethod(t *testing.T) {
	if _, err := Login(context.Background(), Config{Address: "http://127.0.0.1:1", AuthMethod: "bogus"}); err == nil {
		t.Fatal("expected error for unsupported auth method")
	}
}

// Token auth with no token fails loudly (#712: no ambient VAULT_TOKEN fallback).
func TestLogin_TokenMethodRequiresToken(t *testing.T) {
	if _, err := Login(context.Background(), Config{Address: "http://127.0.0.1:1", AuthMethod: "token"}); err == nil {
		t.Fatal("expected error for token auth without a token")
	}
}
