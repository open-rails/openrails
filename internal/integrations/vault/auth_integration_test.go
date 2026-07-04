//go:build integration

// Real-Vault auth + re-auth-lifecycle integration tests (#724, #751). These
// replace the former httptest Vault mock in auth_test.go: static-token and
// AppRole login, the LifetimeWatcher renewal path, and the #751 Supervisor's
// automatic re-authentication (past max TTL, and on a revoked/dead token) are
// all proven against the vaulttest container — never a mock.
package vault

import (
	"context"
	"strings"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"

	"github.com/open-rails/openrails/internal/integrations/vault/vaulttest"
)

// TestLogin_TokenAgainstRealVault proves the token auth path end to end: Login
// sets the supplied token on the client and real KV operations are authorized by
// it — no AppRole/Kubernetes config needed for a dev/e2e Vault.
func TestLogin_TokenAgainstRealVault(t *testing.T) {
	ctx := context.Background()
	addr, rootToken := vaulttest.Addr(t)

	client, sup, err := Login(ctx, Config{Address: addr, AuthMethod: "token", Token: rootToken})
	if err != nil {
		t.Fatalf("Login(token): %v", err)
	}
	if client.Token() != rootToken {
		t.Fatalf("client token = %q, want the supplied token", client.Token())
	}
	if err := sup.AuthState(); err != nil {
		t.Fatalf("AuthState() = %v, want nil for a healthy root token", err)
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
	client, _, err := Login(ctx, Config{Address: addr, Token: rootToken})
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

	client, _, err := Login(ctx, Config{Address: addr, AuthMethod: "approle", RoleID: roleID, SecretID: secretID})
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
	if _, _, err := Login(context.Background(), Config{Address: addr, AuthMethod: "approle", RoleID: roleID, SecretID: "wrong"}); err == nil {
		t.Fatal("approle login with a bad secret_id must fail loudly")
	}
}

// TestApproleSupervisor_ReauthsPastMaxTTL is the #751 task-(a) test: a role
// bound to a SHORT max TTL (so the lease provably cannot be renewed past it)
// keeps serving KV reads/writes well beyond that MAX TTL, because the
// Supervisor re-authenticates automatically the moment the LifetimeWatcher
// ends — the "supervisor calling Login again" that a stale comment on the old
// renew() promised but never built.
func TestApproleSupervisor_ReauthsPastMaxTTL(t *testing.T) {
	if testing.Short() {
		t.Skip("re-auth test needs several seconds of wall clock")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // stops the Supervisor's goroutine
	addr, _ := vaulttest.Addr(t)

	roleID, secretID := vaulttest.EnsureAppRole(t, "openrails-it-maxttl", "openrails-approle-maxttl", vaulttest.PolicyKVReadWrite,
		map[string]any{"token_ttl": "2s", "token_max_ttl": "6s"})
	client, sup, err := Login(ctx, Config{Address: addr, AuthMethod: "approle", RoleID: roleID, SecretID: secretID})
	if err != nil {
		t.Fatalf("Login(approle, 2s/6s ttl): %v", err)
	}
	firstToken := client.Token()

	// Deliberately NOT wired with WithReauthTrigger: recovery here must come
	// from the LifetimeWatcher's own DoneCh firing at/near max TTL, proving
	// the NATURAL (non-403-triggered) re-auth path, independent of #751 task
	// 5's immediate-kick path (covered by TestApproleSupervisor_ReauthsOnRevocation).
	kv := NewKVv2Adapter(client, "secret")
	path := "secret/openrails/vaulttest/reauth-maxttl/probe"
	t.Cleanup(func() { _ = kv.DeleteSecret(context.Background(), path) })

	// Poll past the 6s MAX TTL until the token rotates and writes succeed
	// again. A momentary gap around the exact expiry instant (the watcher's
	// grace/backoff math is wall-clock-approximate, not millisecond-exact
	// against Vault's own revocation) is expected and tolerated — only
	// failing to ever recover within the generous deadline is a bug.
	deadline := time.Now().Add(20 * time.Second)
	var rotated bool
	for time.Now().Before(deadline) {
		if _, err := kv.WriteSecret(ctx, path, map[string]string{"value": "alive"}); err == nil && client.Token() != firstToken {
			rotated = true
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if !rotated {
		t.Fatal("token never rotated / writes never recovered past max TTL — natural (DoneCh-triggered) re-auth did not happen")
	}
	if _, _, err := kv.ReadSecret(ctx, path); err != nil {
		t.Fatalf("read after re-auth: %v", err)
	}
	if authErr := sup.AuthState(); authErr != nil {
		t.Fatalf("AuthState() = %v, want nil after a successful re-auth", authErr)
	}
}

// TestApproleSupervisor_ReauthsOnRevocation is the #751 task-(b) approle case:
// revoking the CURRENT token out from under a live client makes the very next
// KV read observe Vault's permission-denied response; NotifyPermissionDenied
// (wired via WithReauthTrigger) confirms via self-lookup that the token is
// dead and kicks the Supervisor immediately — no waiting out the ambient
// lease/renewal window. A subsequent read succeeds without any restart.
func TestApproleSupervisor_ReauthsOnRevocation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, _ := vaulttest.Addr(t)

	roleID, secretID := vaulttest.EnsureAppRole(t, "openrails-it-revoke", "openrails-approle-revoke", vaulttest.PolicyKVReadWrite, nil)
	client, sup, err := Login(ctx, Config{Address: addr, AuthMethod: "approle", RoleID: roleID, SecretID: secretID})
	if err != nil {
		t.Fatalf("Login(approle): %v", err)
	}
	revokedToken := client.Token()

	kv := NewKVv2Adapter(client, "secret").WithReauthTrigger(sup)
	path := "secret/openrails/vaulttest/reauth-revoke/probe"
	t.Cleanup(func() { _ = kv.DeleteSecret(context.Background(), path) })
	if _, err := kv.WriteSecret(ctx, path, map[string]string{"value": "pre-revoke"}); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	vaulttest.RevokeToken(t, revokedToken)

	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, _, err := kv.ReadSecret(ctx, path); err == nil {
			lastErr = nil
			break
		} else {
			lastErr = err
		}
		time.Sleep(200 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("read never recovered after revocation + 403-triggered re-auth: %v", lastErr)
	}
	if client.Token() == revokedToken {
		t.Fatal("token never rotated after revocation — re-auth was not triggered")
	}
	if authErr := sup.AuthState(); authErr != nil {
		t.Fatalf("AuthState() = %v, want nil after recovering from revocation", authErr)
	}
}

// TestStaticTokenSupervisor_RenewsPeriodicToken proves the #751 task-3
// renewable branch: a periodic (renewable) static token is kept alive by the
// SAME watcher machinery as approle/kubernetes — this is renewal, not
// re-authentication, so the token identity never changes.
func TestStaticTokenSupervisor_RenewsPeriodicToken(t *testing.T) {
	if testing.Short() {
		t.Skip("renewal test needs several seconds of wall clock")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, _ := vaulttest.Addr(t)
	root := vaulttest.RootClient(t)

	created, err := root.Auth().Token().Create(&vaultapi.TokenCreateRequest{
		Policies: []string{"default"},
		Period:   "3s",
		NoParent: true,
	})
	if err != nil {
		t.Fatalf("create periodic token: %v", err)
	}
	token := created.Auth.ClientToken

	client, sup, err := Login(ctx, Config{Address: addr, AuthMethod: "token", Token: token})
	if err != nil {
		t.Fatalf("Login(token, periodic 3s): %v", err)
	}

	deadline := time.Now().Add(7 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := client.Auth().Token().LookupSelfWithContext(ctx); err != nil {
			t.Fatalf("periodic token died — renewal did not keep it alive: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if client.Token() != token {
		t.Fatal("token identity changed — renewal must reuse the SAME token, not re-authenticate")
	}
	if authErr := sup.AuthState(); authErr != nil {
		t.Fatalf("AuthState() = %v, want nil while renewal keeps succeeding", authErr)
	}
}

// TestStaticTokenSupervisor_NonRenewableExpiryIsLoud is the #751 task-3
// non-renewable branch and task-(b)'s static-token case: a non-renewable
// token with a short TTL gets a prominent boot warning naming the deadline,
// and AuthState() flips to a loud, PERMANENT error once the lease ends — no
// silent decay, and (pinned) no self-healing, since a bare token has no
// credential material to redo a login with.
func TestStaticTokenSupervisor_NonRenewableExpiryIsLoud(t *testing.T) {
	if testing.Short() {
		t.Skip("expiry test needs several seconds of wall clock")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, _ := vaulttest.Addr(t)
	root := vaulttest.RootClient(t)

	notRenewable := false
	created, err := root.Auth().Token().Create(&vaultapi.TokenCreateRequest{
		Policies:  []string{"default"},
		TTL:       "2s",
		Renewable: &notRenewable,
		NoParent:  true,
	})
	if err != nil {
		t.Fatalf("create non-renewable token: %v", err)
	}
	token := created.Auth.ClientToken

	hook := logtest.NewGlobal()
	defer hook.Reset()

	_, sup, err := Login(ctx, Config{Address: addr, AuthMethod: "token", Token: token})
	if err != nil {
		t.Fatalf("Login(token, non-renewable, 2s ttl): %v", err)
	}

	var warned bool
	for _, e := range hook.AllEntries() {
		if e.Level == log.WarnLevel && strings.Contains(e.Message, "NON-RENEWABLE") {
			warned = true
		}
	}
	if !warned {
		t.Fatal("expected a boot-time NON-RENEWABLE warning naming the expiry deadline")
	}
	if authErr := sup.AuthState(); authErr != nil {
		t.Fatalf("AuthState() = %v, want nil immediately after boot", authErr)
	}

	deadline := time.Now().Add(6 * time.Second)
	var sawErr bool
	for time.Now().Before(deadline) {
		if sup.AuthState() != nil {
			sawErr = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !sawErr {
		t.Fatal("AuthState() never flipped to an error after the non-renewable token's TTL elapsed")
	}

	// Pinned: no self-healing. A bare static token has no login material.
	time.Sleep(1 * time.Second)
	if sup.AuthState() == nil {
		t.Fatal("static non-renewable token self-healed — impossible, and must never happen")
	}
}
