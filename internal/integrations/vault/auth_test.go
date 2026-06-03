package vault

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestLogin_Token proves the token auth path: Login sets the supplied token on
// the client without calling any auth/login endpoint, so a dev/e2e Vault reached
// with VAULT_ADDR + VAULT_TOKEN works with no AppRole/Kubernetes config.
func TestLogin_Token(t *testing.T) {
	var sawToken string
	var hitAuthLogin bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawToken = r.Header.Get("X-Vault-Token")
		if contains(r.URL.Path, "/auth/") && contains(r.URL.Path, "/login") {
			hitAuthLogin = true
		}
		// Respond to a token-self lookup or any probe with an empty OK.
		writeJSON(w, map[string]any{"data": map[string]any{}})
	}))
	defer srv.Close()

	client, err := Login(context.Background(), Config{
		Address:    srv.URL,
		AuthMethod: "token",
		Token:      "root-dev-token",
	})
	if err != nil {
		t.Fatalf("Login(token): %v", err)
	}
	if client.Token() != "root-dev-token" {
		t.Fatalf("client token = %q, want root-dev-token", client.Token())
	}

	// Make a call so the mock observes the token; prove we never hit auth/login.
	_, _ = client.Logical().ReadWithContext(context.Background(), "secret/data/probe")
	if sawToken != "root-dev-token" {
		t.Fatalf("request carried token %q, want root-dev-token", sawToken)
	}
	if hitAuthLogin {
		t.Fatal("token auth must NOT call an auth/*/login endpoint")
	}
}

// TestLogin_TokenInferredFromTokenOnly proves an empty AuthMethod defaults to
// token auth when a Token is present (the VAULT_TOKEN-only convenience).
func TestLogin_TokenInferredFromTokenOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": map[string]any{}})
	}))
	defer srv.Close()

	client, err := Login(context.Background(), Config{Address: srv.URL, Token: "tok"})
	if err != nil {
		t.Fatalf("Login(inferred token): %v", err)
	}
	if client.Token() != "tok" {
		t.Fatalf("client token = %q, want tok", client.Token())
	}
}

func TestLogin_UnsupportedMethod(t *testing.T) {
	if _, err := Login(context.Background(), Config{Address: "http://127.0.0.1:1", AuthMethod: "bogus"}); err == nil {
		t.Fatal("expected error for unsupported auth method")
	}
}
