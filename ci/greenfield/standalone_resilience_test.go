//go:build greenfield && integration

package greenfield_test

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

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/embed/controlplane"
	hostconfig "github.com/open-rails/openrails/hostauth/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/bootstrap/serverboot"
	"github.com/open-rails/openrails/internal/integrations/vault"
	"github.com/open-rails/openrails/internal/standalonedb"
	"github.com/open-rails/openrails/internal/vaultfake"
	"github.com/open-rails/openrails/pkg/merchant"
)

// The standalone server boots from its merchant manifest without waiting on
// Vault, and a changed Transit key fails closed there too: no new Solana
// identity is provisioned until an operator approves it.
func TestStandaloneBootNeverWaitsOnVaultAndFailsClosedOnKeyChange(t *testing.T) {
	t.Setenv("VAULT_MAX_RETRIES", "0")
	f := newFixture(t)
	require.NoError(t, standalonedb.ApplyAuthKit(t.Context(), f.pool, f.pool))
	fake := vaultfake.New("greenfield-root")
	t.Cleanup(fake.Close)
	slug := "standalone-" + uuid.NewString()[:8]
	manifest := filepath.Join(t.TempDir(), "merchants.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(`version: 1
merchants:
  `+slug+`:
    display_name: Standalone
    psps:
      solana:
        solana:
          signer: { mode: vault_transit, key: `+transitKey+` }
`), 0o600))

	boot := func() (*embed.Runtime, error) {
		cfg := &config.Config{
			TestMode:          config.CredentialPostureSandbox,
			ProviderWriteMode: config.ProviderWriteModeFull,
			DB:                &config.DBConfig{URL: f.dsn(t), Schema: f.schema},
			ReturnOrigins:     []string{"https://greenfield.test"},
			Vault:             &config.VaultConfig{Enabled: true, Address: fake.URL(), Token: fake.Token},
			ProviderSandbox:   &config.ProviderSandboxConfig{SolanaRPCURL: "http://127.0.0.1:1"},
		}
		rt, err := embed.New(t.Context(), embed.Options{Config: cfg, PGXPool: f.pool, River: embed.RiverManagedByOpenRails(f.schema)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = rt.Close(context.Background()) })
		_, err = controlplane.Attach(t.Context(), rt, controlplane.Options{Auth: &hostconfig.AuthConfig{
			Issuer: "http://127.0.0.1/" + slug, AllowMemory: true, AllowMissingSenders: true, AllowEphemeralSigningKey: true, AllowLoopbackHTTP: true, MintDisabled: true, DirectPeerIP: true,
		}})
		require.NoError(t, err)
		return rt, serverboot.ReconcileBootMerchantManifest(t.Context(), cfg, app.HostGraph(rt), manifest, "")
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
	rt, err := boot()
	require.NoError(t, err)
	graph := app.HostGraph(rt)
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
