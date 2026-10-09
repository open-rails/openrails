//go:build e2e && integration

package ci_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/bootstrap/serverboot"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/integrations/vault"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/internal/vaultfake"
	"github.com/open-rails/openrails/server"
)

// The standalone server boots from its merchant manifest without waiting on
// Vault, and a changed Transit key fails closed there too: no new Solana
// identity is provisioned until an operator approves it.
func TestStandaloneBootNeverWaitsOnVaultAndFailsClosedOnKeyChange(t *testing.T) {
	t.Setenv("VAULT_MAX_RETRIES", "0")
	f := newFixture(t)
	fake := vaultfake.New("e2e-root")
	t.Cleanup(fake.Close)
	slug := "standalone-" + uuid.NewString()[:8]
	manifest := filepath.Join(t.TempDir(), "merchants.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(`version: 1
merchants:
  `+slug+`:
    display_name: Standalone
    psps:
      solana:
        rail: solana
        signer: { mode: vault_transit, key: `+transitKey+` }
`), 0o600))

	boot := func() (*server.Server, error) {
		srv := f.newServer(t, func(cfg *server.Config, _ *server.Deps) {
			cfg.Engine.ProviderWriteMode = openrails.ProviderWritesFull
			cfg.Engine.Vault = &openrails.VaultConfig{Enabled: true, Address: fake.URL(), Token: fake.Token}
			cfg.Engine.ProviderSandbox = &openrails.ProviderSandboxConfig{SolanaRPCURL: "http://127.0.0.1:1"}
			cfg.Auth = server.AuthConfig{
				Issuer: "http://127.0.0.1/" + slug, AllowMemory: true, AllowMissingSenders: true, AllowEphemeralSigningKey: true, AllowLoopbackHTTP: true, MintDisabled: true, DirectPeerIP: true,
				Schema: f.authSchema(),
			}
		})
		graph, cp := operator.Of(srv)
		return srv, serverboot.ReconcileBootMerchantManifest(t.Context(), graph.Config, graph, cp, manifest, nil, "")
	}
	activeFor := func(account string) int {
		var n int
		require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{f.schema, "psps"}.Sanitize()+" WHERE rail = 'solana' AND NOT archived AND account_id = $1", account).Scan(&n))
		return n
	}
	old := solanago.PublicKeyFromBytes(fake.PublicKey(transitKey)).String()

	first, err := boot()
	require.NoError(t, err)
	require.Eventually(t, func() bool { return activeFor(old) == 1 }, 30*time.Second, 50*time.Millisecond)
	require.NoError(t, first.Close(context.Background()))

	fake.SetUp(false)
	start := time.Now()
	down, err := boot()
	require.NoError(t, err, "a serving boot never waits on Vault")
	require.Less(t, time.Since(start), 20*time.Second)
	require.NoError(t, down.Close(context.Background()))

	fake.SetUp(true)
	fake.Rotate(transitKey)
	rotated := solanago.PublicKeyFromBytes(fake.PublicKey(transitKey)).String()
	srv, err := boot()
	require.NoError(t, err)
	rt := srv.Client()
	graph := engine.Graph(rt)
	m, err := graph.Runtime.Merchants.GetBySlug(t.Context(), slug)
	require.NoError(t, err)
	railConfig := func() error {
		_, err := graph.Runtime.RailConfigs.RailConfig(merchant.WithID(t.Context(), m.ID), "solana", "")
		return err
	}
	require.Eventually(t, func() bool { return probe(t, rt, "openrails_solana_signer_identity") != nil }, 30*time.Second, 50*time.Millisecond)
	require.ErrorIs(t, railConfig(), vault.ErrSignerUnapproved)
	require.Zero(t, activeFor(rotated), "no new, newest Solana PSP for an unapproved key")
	require.Equal(t, 1, activeFor(old))

	require.NoError(t, graph.Runtime.ApproveSolanaSigner(t.Context(), m.ID, transitKey))
	require.Equal(t, 1, activeFor(rotated))
	require.Zero(t, activeFor(old))
	require.NoError(t, railConfig())
	require.NoError(t, probe(t, rt, "openrails_solana_signer_identity"))
}
