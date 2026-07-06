//go:build integration

// Package vaulttest provides a shared HashiCorp Vault dev-mode testcontainer for
// integration tests, mirroring internal/dbtest's Postgres/Redis pattern (#724):
// one container per test-package process (sync.Once), torn down by the package's
// TestMain via RunMain/TerminateShared. If VAULT_ADDR + VAULT_TOKEN are set, that
// external Vault is reused instead (the way OPENRAILS_TEST_DB_URL reuses a
// compose Postgres).
//
// Dev mode gives KV-v2 at secret/ and a root token out of the box. Helpers mint
// policy-scoped child tokens (the canned policies mirror docs/vault.md), mount
// transit, and enable AppRole so auth flows run against REAL Vault — never a
// httptest mock (mocked-Vault tests assert our guesses about Vault, not Vault).
package vaulttest

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	// image is pinned (dbtest discipline: postgres:18-alpine, redis:7-alpine).
	image        = "hashicorp/vault:1.21"
	devRootToken = "openrails-vaulttest-root"
)

// Canned least-privilege policies, mirroring docs/vault.md. Tests mint child
// tokens bound to exactly one of these to prove the #661 capability matrix
// against real Vault ACL evaluation.
const (
	// PolicyKVReadWrite is the full KV secret-store grant (secret_backend=vault).
	PolicyKVReadWrite = `
path "secret/data/openrails/*"     { capabilities = ["create","read","update","delete"] }
path "secret/metadata/openrails/*" { capabilities = ["read","delete","list"] }
`
	// PolicyKVReadOnly can read/list merchant secrets but never write them.
	PolicyKVReadOnly = `
path "secret/data/openrails/*"     { capabilities = ["read"] }
path "secret/metadata/openrails/*" { capabilities = ["read","list"] }
`
	// PolicyTransitOnly is the Solana-signing-only deployment: zero KV access.
	PolicyTransitOnly = `
path "transit/sign/*" { capabilities = ["update"] }
path "transit/keys/*" { capabilities = ["read"] }
`
)

var (
	sharedOnce      sync.Once
	sharedErr       error
	sharedAddr      string
	sharedToken     string
	sharedContainer testcontainers.Container
)

// RunMain is the TestMain entry point for packages whose integration tests use
// ONLY vaulttest. Packages that also use dbtest compose the terminations
// explicitly (dbtest.RunMain and this both os.Exit, so they cannot nest).
func RunMain(m *testing.M) {
	code := m.Run()
	TerminateShared()
	os.Exit(code)
}

// TerminateShared terminates the shared Vault container if this process started
// one. Safe when an external VAULT_ADDR was used; idempotent.
func TerminateShared() {
	if sharedContainer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sharedContainer.Terminate(ctx); err != nil {
		fmt.Printf("vaulttest: failed to terminate shared vault container: %v\n", err)
	}
	sharedContainer = nil
}

// Addr returns the address and root token of the process-shared dev Vault,
// provisioning the container on first use.
func Addr(t testing.TB) (addr, rootToken string) {
	t.Helper()
	sharedOnce.Do(func() {
		sharedAddr, sharedToken, sharedContainer, sharedErr = provision(context.Background())
	})
	require.NoError(t, sharedErr, "provision shared integration vault")
	return sharedAddr, sharedToken
}

func provision(ctx context.Context) (addr, token string, ctr testcontainers.Container, err error) {
	if a, tok := strings.TrimSpace(os.Getenv("VAULT_ADDR")), strings.TrimSpace(os.Getenv("VAULT_TOKEN")); a != "" && tok != "" {
		return a, tok, nil, nil
	}
	ctr, addr, err = startContainer(ctx)
	if err != nil {
		return "", "", nil, err
	}
	return addr, devRootToken, ctr, nil
}

func startContainer(ctx context.Context) (testcontainers.Container, string, error) {
	// Retry startup: sharedOnce makes a single failure poison every Vault test
	// in the process, and loaded CI runners intermittently blow the readiness
	// window (the recurring "wait until ready: context deadline exceeded" flake).
	var ctr testcontainers.Container
	var err error
	for attempt := 1; ; attempt++ {
		ctr, err = testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Image:        image,
				ExposedPorts: []string{"8200/tcp"},
				Env: map[string]string{
					"VAULT_DEV_ROOT_TOKEN_ID":  devRootToken,
					"VAULT_DEV_LISTEN_ADDRESS": "0.0.0.0:8200",
				},
				// Dev mode is initialized+unsealed+active => sys/health returns 200.
				WaitingFor: wait.ForHTTP("/v1/sys/health").WithPort("8200/tcp").
					WithStartupTimeout(120 * time.Second),
				HostConfigModifier: func(hc *container.HostConfig) {
					hc.Resources.Memory = 512 << 20
					hc.Resources.NanoCPUs = 1_000_000_000
				},
			},
			Started: true,
		})
		if err == nil || attempt >= 3 || ctx.Err() != nil {
			break
		}
		if ctr != nil {
			_ = ctr.Terminate(context.WithoutCancel(ctx))
		}
	}
	if err != nil {
		return nil, "", fmt.Errorf("start vault container: %w", err)
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("vault container host: %w", err)
	}
	port, err := ctr.MappedPort(ctx, "8200/tcp")
	if err != nil {
		return nil, "", fmt.Errorf("vault mapped port: %w", err)
	}
	return ctr, fmt.Sprintf("http://%s:%s", host, port.Port()), nil
}

// RootClient returns a client on the shared Vault authenticated with the root token.
func RootClient(t testing.TB) *vaultapi.Client {
	t.Helper()
	addr, token := Addr(t)
	return Client(t, addr, token)
}

// Client builds a vault client for addr authenticated with token.
func Client(t testing.TB, addr, token string) *vaultapi.Client {
	t.Helper()
	cfg := vaultapi.DefaultConfig()
	cfg.Address = addr
	cl, err := vaultapi.NewClient(cfg)
	require.NoError(t, err, "vault client")
	cl.SetToken(token)
	return cl
}

// TokenWithPolicy writes an ACL policy and mints a fresh orphan child token bound
// only to it (plus the built-in default policy, which grants sys/capabilities-self).
func TokenWithPolicy(t testing.TB, name, hcl string) string {
	t.Helper()
	root := RootClient(t)
	require.NoError(t, root.Sys().PutPolicy(name, hcl), "put policy %s", name)
	secret, err := root.Auth().Token().Create(&vaultapi.TokenCreateRequest{
		Policies: []string{name},
		NoParent: true,
	})
	require.NoError(t, err, "create token for policy %s", name)
	return secret.Auth.ClientToken
}

// ClientWithPolicy is TokenWithPolicy + Client on the shared Vault.
func ClientWithPolicy(t testing.TB, name, hcl string) *vaultapi.Client {
	t.Helper()
	addr, _ := Addr(t)
	return Client(t, addr, TokenWithPolicy(t, name, hcl))
}

// EnsureTransit mounts the transit secrets engine on the shared Vault (idempotent).
func EnsureTransit(t testing.TB) {
	t.Helper()
	ensureTransit(t, RootClient(t))
}

func ensureTransit(t testing.TB, root *vaultapi.Client) {
	t.Helper()
	err := root.Sys().Mount("transit", &vaultapi.MountInput{Type: "transit"})
	if err != nil && !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("enable transit: %v", err)
	}
}

// EnsureAppRole enables the approle auth method (idempotent) and provisions a
// role bound to the named policy, returning its login credentials. extra merges
// into the role definition (e.g. token_period for the renewal-lifecycle tests).
func EnsureAppRole(t testing.TB, role, policyName, policyHCL string, extra map[string]any) (roleID, secretID string) {
	t.Helper()
	ctx := context.Background()
	root := RootClient(t)
	err := root.Sys().EnableAuthWithOptions("approle", &vaultapi.EnableAuthOptions{Type: "approle"})
	if err != nil && !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("enable approle auth: %v", err)
	}
	require.NoError(t, root.Sys().PutPolicy(policyName, policyHCL), "put policy %s", policyName)
	def := map[string]any{"token_policies": []string{policyName}}
	for k, v := range extra {
		def[k] = v
	}
	_, err = root.Logical().WriteWithContext(ctx, "auth/approle/role/"+role, def)
	require.NoError(t, err, "write approle role %s", role)

	rid, err := root.Logical().ReadWithContext(ctx, "auth/approle/role/"+role+"/role-id")
	require.NoError(t, err, "read role-id")
	roleID, _ = rid.Data["role_id"].(string)
	sid, err := root.Logical().WriteWithContext(ctx, "auth/approle/role/"+role+"/secret-id", nil)
	require.NoError(t, err, "mint secret-id")
	secretID, _ = sid.Data["secret_id"].(string)
	require.NotEmpty(t, roleID, "role_id")
	require.NotEmpty(t, secretID, "secret_id")
	return roleID, secretID
}

// RevokeToken revokes a child token (and its lease tree) via the root token, so
// tests can prove revocation surfaces as loud failures.
func RevokeToken(t testing.TB, token string) {
	t.Helper()
	require.NoError(t, RootClient(t).Auth().Token().RevokeTree(token), "revoke token")
}

// Dedicated is a per-test Vault container for outage-semantics tests: Pause
// freezes the process (memory intact — data survives Unpause), so tests can pin
// unreachable-at-boot, fail-closed-at-read, and no-restart recovery. The SHARED
// container is never paused (other tests in the package depend on it).
type Dedicated struct {
	Addr      string
	RootToken string
	container testcontainers.Container
	docker    *testcontainers.DockerClient
}

// StartDedicated starts a dedicated dev Vault container bound to t's lifetime.
func StartDedicated(t testing.TB) *Dedicated {
	t.Helper()
	ctx := context.Background()
	ctr, addr, err := startContainer(ctx)
	require.NoError(t, err, "start dedicated vault container")
	t.Cleanup(func() {
		tctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = ctr.Terminate(tctx)
	})
	docker, err := testcontainers.NewDockerClientWithOpts(ctx)
	require.NoError(t, err, "docker client")
	return &Dedicated{Addr: addr, RootToken: devRootToken, container: ctr, docker: docker}
}

// Client returns a client on the dedicated Vault.
func (d *Dedicated) Client(t testing.TB, token string) *vaultapi.Client {
	t.Helper()
	return Client(t, d.Addr, token)
}

// Pause freezes the Vault process (simulated outage; memory preserved).
func (d *Dedicated) Pause(t testing.TB) {
	t.Helper()
	_, err := d.docker.ContainerPause(context.Background(), d.container.GetContainerID(), mobyclient.ContainerPauseOptions{})
	require.NoError(t, err, "pause dedicated vault container")
}

// Unpause resumes the Vault process with all pre-outage state intact.
func (d *Dedicated) Unpause(t testing.TB) {
	t.Helper()
	_, err := d.docker.ContainerUnpause(context.Background(), d.container.GetContainerID(), mobyclient.ContainerUnpauseOptions{})
	require.NoError(t, err, "unpause dedicated vault container")
}
