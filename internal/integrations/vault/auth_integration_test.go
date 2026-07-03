//go:build integration

// Real-Vault auth + token-lifecycle integration tests (#724). These replace the
// former httptest Vault mock in auth_test.go: static-token and AppRole login are
// asserted against the vaulttest container, and the LifetimeWatcher renewal path
// (auth.go renew) is proven with a real short-period token.
package vault

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/integrations/vault/vaulttest"
)

// TestLogin_TokenAgainstRealVault proves the token auth path end to end: Login
// sets the supplied token on the client and real KV operations are authorized by
// it — no AppRole/Kubernetes config needed for a dev/e2e Vault.
func TestLogin_TokenAgainstRealVault(t *testing.T) {
	ctx := context.Background()
	addr, rootToken := vaulttest.Addr(t)

	client, err := Login(ctx, Config{Address: addr, AuthMethod: "token", Token: rootToken})
	if err != nil {
		t.Fatalf("Login(token): %v", err)
	}
	if client.Token() != rootToken {
		t.Fatalf("client token = %q, want the supplied token", client.Token())
	}

	kv := NewKVv2Adapter(client, "secret")
	path := "secret/openrails/vaulttest/auth-token/probe"
	if _, err := kv.WriteSecret(ctx, path, map[string]string{"value": "token-auth"}); err != nil {
		t.Fatalf("write through token-auth client: %v", err)
	}
	t.Cleanup(func() { _ = kv.DeleteSecret(context.Background(), path) })
	got, _, err := kv.ReadSecret(ctx, path)
	if err != nil {
		t.Fatalf("read through token-auth client: %v", err)
	}
	if got["value"] != "token-auth" {
		t.Fatalf("round trip = %q, want token-auth", got["value"])
	}
}

// An empty AuthMethod defaults to token auth when a Token is present (the
// VAULT_TOKEN-only convenience) — against real Vault.
func TestLogin_TokenInferredFromTokenOnly(t *testing.T) {
	ctx := context.Background()
	addr, rootToken := vaulttest.Addr(t)
	client, err := Login(ctx, Config{Address: addr, Token: rootToken})
	if err != nil {
		t.Fatalf("Login(inferred token): %v", err)
	}
	if _, err := client.Sys().Health(); err != nil {
		t.Fatalf("health through inferred-token client: %v", err)
	}
}

// TestLogin_AppRole logs in via a real AppRole bound to the KV read-write policy
// and proves the minted token is policy-scoped: KV under openrails/* works,
// anything outside 403s (Vault's runtime boundary, not our probe).
func TestLogin_AppRole(t *testing.T) {
	ctx := context.Background()
	addr, _ := vaulttest.Addr(t)
	roleID, secretID := vaulttest.EnsureAppRole(t, "openrails-it-kv", "openrails-approle-kv", vaulttest.PolicyKVReadWrite, nil)

	client, err := Login(ctx, Config{Address: addr, AuthMethod: "approle", RoleID: roleID, SecretID: secretID})
	if err != nil {
		t.Fatalf("Login(approle): %v", err)
	}
	if client.Token() == "" {
		t.Fatal("approle login produced no client token")
	}

	kv := NewKVv2Adapter(client, "secret")
	path := "secret/openrails/vaulttest/auth-approle/probe"
	if _, err := kv.WriteSecret(ctx, path, map[string]string{"value": "approle"}); err != nil {
		t.Fatalf("in-policy KV write: %v", err)
	}
	t.Cleanup(func() { _ = kv.DeleteSecret(context.Background(), path) })
	if _, err := kv.WriteSecret(ctx, "secret/not-openrails/escape", map[string]string{"value": "x"}); err == nil {
		t.Fatal("out-of-policy KV write succeeded; want Vault 403")
	}
}

func TestLogin_AppRoleBadCredentialsFailsLoudly(t *testing.T) {
	addr, _ := vaulttest.Addr(t)
	roleID, _ := vaulttest.EnsureAppRole(t, "openrails-it-kv", "openrails-approle-kv", vaulttest.PolicyKVReadWrite, nil)
	if _, err := Login(context.Background(), Config{Address: addr, AuthMethod: "approle", RoleID: roleID, SecretID: "wrong"}); err == nil {
		t.Fatal("approle login with a bad secret_id must fail loudly")
	}
}

// TestTokenLifecycle_RenewalAndRevocation pins the #724 token-lifecycle
// semantics with a REAL short-period token:
//
//  1. Login(approle) on a role with token_period=5s starts the LifetimeWatcher
//     (auth.go renew); operations keep working well past the original TTL.
//  2. Root revokes the token: operations fail LOUDLY (Vault 403), never silently.
//  3. There is NO automatic re-login (pinned): the watcher logs and exits, and a
//     supervisor calling Login again (or a process restart) is the recovery path.
func TestTokenLifecycle_RenewalAndRevocation(t *testing.T) {
	if testing.Short() {
		t.Skip("token-lifecycle test needs ~12s of wall clock")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // stops the renewer goroutine
	addr, _ := vaulttest.Addr(t)

	roleID, secretID := vaulttest.EnsureAppRole(t, "openrails-it-short", "openrails-approle-short", vaulttest.PolicyKVReadWrite,
		map[string]any{"token_period": "5s"})
	client, err := Login(ctx, Config{Address: addr, AuthMethod: "approle", RoleID: roleID, SecretID: secretID})
	if err != nil {
		t.Fatalf("Login(approle, 5s period): %v", err)
	}

	kv := NewKVv2Adapter(client, "secret")
	path := "secret/openrails/vaulttest/lifecycle/probe"
	t.Cleanup(func() { _ = kv.DeleteSecret(context.Background(), path) })

	// Operate for ~11s (>2x the 5s period). Without renewal the token dies at 5s;
	// every op succeeding proves the LifetimeWatcher kept it alive.
	deadline := time.Now().Add(11 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		if _, err := kv.WriteSecret(ctx, path, map[string]string{"value": "alive"}); err != nil {
			t.Fatalf("op at +%ds failed — LifetimeWatcher did not keep the token alive: %v", i, err)
		}
		time.Sleep(1 * time.Second)
	}

	// Revocation ⇒ loud failure (never a silent empty read).
	vaulttest.RevokeToken(t, client.Token())
	_, _, err = kv.ReadSecret(ctx, path)
	if err == nil {
		t.Fatal("read with a revoked token succeeded; want a loud Vault error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "permission denied") && !errors.Is(err, context.DeadlineExceeded) {
		t.Logf("revoked-token error (pinned as loud, exact text may vary): %v", err)
	}

	// Pinned: no self-healing re-login. The same client stays broken until a
	// supervisor calls Login again / the process restarts.
	time.Sleep(2 * time.Second)
	if _, _, err := kv.ReadSecret(ctx, path); err == nil {
		t.Fatal("client self-healed after revocation; re-login policy is supervisor/restart-owned and must not be emergent")
	}
}
