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

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/solanafake"
	"github.com/open-rails/openrails/internal/vaultfake"
	"github.com/open-rails/openrails/server"
	"github.com/open-rails/openrails/server/internal/bootstrap/serverboot"
	"github.com/open-rails/openrails/server/internal/operator"
)

// The standalone server boots from its merchant manifest without waiting on
// Vault. A Solana PSP is its treasury, so rotating the deployment cranker's
// Transit key changes no PSP: the same treasury, under the same id, still
// receives every payment, and the cranker becomes the new key.
func TestStandaloneBootNeverWaitsOnVaultAndKeyChangeKeepsTheTreasury(t *testing.T) {
	t.Setenv("VAULT_MAX_RETRIES", "0")
	f := newFixture(t)
	fake := vaultfake.New("e2e-root")
	t.Cleanup(fake.Close)
	chain := solanafake.New()
	t.Cleanup(chain.Close)
	slug := "standalone-" + uuid.NewString()[:8]
	treasury := solanago.NewWallet().PublicKey().String()
	manifest := filepath.Join(t.TempDir(), "merchants.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(`version: 1
merchants:
  `+slug+`:
    display_name: Standalone
    psps:
      solana:
        rail: solana
        environment: devnet
        treasury: `+treasury+`
`), 0o600))

	boot := func() (*server.Server, error) {
		srv := f.newServer(t, func(cfg *server.Config, _ *server.Deps) {
			cfg.Engine.ProviderWriteMode = openrails.ProviderWritesFull
			cfg.Engine.Vault = &openrails.VaultConfig{Address: fake.URL(), Token: fake.Token}
			cfg.Engine.Solana = &openrails.SolanaConfig{Crankers: []openrails.SolanaCrankerConfig{{Mode: "vault_transit", Key: transitKey}}}
			cfg.Engine.ProviderSandbox = &openrails.ProviderSandboxConfig{SolanaRPCURL: chain.URL()}
			// Keep enrollment closed in this Vault/key-rotation fixture.
			cfg.Auth = authkit.Config{
				Token:        authkit.TokenConfig{Issuer: "http://127.0.0.1/" + slug},
				Keys:         authkit.KeysConfig{AllowEphemeralDevKeys: true, VerifyOnly: true},
				Registration: authkit.RegistrationConfig{NativeUserMode: iam.RegistrationModeClosed},
				HTTP:         &authkit.HTTPConfig{DirectPeerIP: true},
				Database:     authkit.DatabaseConfig{Schema: f.authSchema()},
			}
		})
		graph, cp := operator.Of(srv)
		return srv, serverboot.ReconcileBootMerchantManifest(t.Context(), graph.Config, graph, cp, manifest, nil, "")
	}
	solanaPSPs := func() []string {
		rows, err := f.pool.Query(t.Context(), "SELECT id::text || ' ' || account_id FROM "+pgx.Identifier{f.schema, "psps"}.Sanitize()+" WHERE rail = 'solana' AND superseded_at IS NULL")
		require.NoError(t, err)
		out, err := pgx.CollectRows(rows, pgx.RowTo[string])
		require.NoError(t, err)
		return out
	}

	first, err := boot()
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(solanaPSPs()) == 1 }, 30*time.Second, 50*time.Millisecond)
	stored := solanaPSPs()
	require.Contains(t, stored[0], treasury)
	require.NoError(t, first.Close(context.Background()))

	fake.SetUp(false)
	start := time.Now()
	down, err := boot()
	require.NoError(t, err, "a serving boot never waits on Vault")
	require.Less(t, time.Since(start), 20*time.Second)
	require.NoError(t, down.Close(context.Background()))

	fake.SetUp(true)
	fake.Rotate(transitKey)
	srv, err := boot()
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	graph := engine.Graph(srv.Engine())
	m, err := graph.Runtime.Merchants.GetBySlug(t.Context(), slug)
	require.NoError(t, err)
	require.Equal(t, stored, solanaPSPs(), "the PSP is its treasury, not its cranker")
	psp, err := graph.Runtime.RailConfigs.RailConfig(merchant.WithID(t.Context(), m.ID), "solana", "")
	require.NoError(t, err)
	require.Equal(t, treasury, psp.AccountID)
	require.Eventually(t, func() bool {
		cranker, err := graph.Runtime.SolanaPlanService.Publisher(t.Context())
		return err == nil && cranker.Equals(solanago.PublicKeyFromBytes(fake.PublicKey(transitKey)))
	}, 30*time.Second, 50*time.Millisecond, "the cranker is the rotated key")
}
