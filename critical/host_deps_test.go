//go:build integration

package critical_test

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/vaulttest"
	"github.com/stretchr/testify/require"
)

// configurationState is the engine's openrails_configuration probe: what it
// does not serve, with why.
func configurationState(t *testing.T, engine *openrails.Engine) error {
	t.Helper()
	for _, p := range engine.Probes() {
		if p.Name == "openrails_configuration" {
			return p.Check(t.Context())
		}
	}
	t.Fatal("no openrails_configuration probe")
	return nil
}

func TestHostDeps(t *testing.T) {
	t.Run("OR-J04-S72/boots-on-a-pool-without-a-connection-string", func(t *testing.T) {
		w := newWorld(t, func(w *world) {
			// The host builds its pool from settings, not a URL: nothing
			// names the database but the pool's own configuration.
			parsed, err := pgxpool.ParseConfig(os.Getenv("OPENRAILS_CRITICAL_DSN"))
			require.NoError(t, err)
			pc, err := pgxpool.ParseConfig("")
			require.NoError(t, err)
			pc.ConnConfig.Config = parsed.ConnConfig.Config
			pc.MaxConns = 8
			pool, err := pgxpool.NewWithConfig(t.Context(), pc)
			require.NoError(t, err)
			t.Cleanup(pool.Close)
			require.Empty(t, pool.Config().ConnString())
			w.deps.Postgres = pool
			w.cfg.Database.LeaseConns = 2
		})
		product, err := w.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "settings-pool", DisplayName: "Settings pool"})
		require.NoError(t, err)
		got, err := w.alpha.GetProduct(t.Context(), product.ID)
		require.NoError(t, err)
		require.Equal(t, "Settings pool", got.DisplayName)
		require.NoError(t, configurationState(t, w.engine))

		_, err = openrails.New(t.Context(), w.cfg, openrails.Deps{Clock: w.clock})
		require.ErrorContains(t, err, "Deps.Postgres is required")
	})
	t.Run("OR-J04-S73/a-kv-mount-without-a-vault-client-is-refused-and-reported", func(t *testing.T) {
		w := newWorld(t, func(w *world) {
			w.cfg.Vault = &openrails.VaultConfig{KVMount: "secret"}
			for i := range w.cfg.Merchants {
				w.cfg.Merchants[i].MerchantDocument = billing.MerchantDocument{Slug: w.cfg.Merchants[i].Slug}
			}
		})
		state := configurationState(t, w.engine)
		require.ErrorContains(t, state, "vault: Config names a Vault mount but Deps.Vault is nil")
		require.ErrorContains(t, state, "vault.merchant_configuration")
		_, err := w.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
		var refused *billing.StatusError
		require.ErrorAs(t, err, &refused, "Vault holds the configuration, and no file stands in for it")
		require.Equal(t, http.StatusServiceUnavailable, refused.Status)
		require.Equal(t, billing.CodeServiceUnavailable, refused.Code)
	})
	t.Run("OR-J04-S74/a-slug-declared-twice-provisions-neither-entry", func(t *testing.T) {
		w := newWorld(t, func(w *world) {
			w.cfg.Merchants = append(w.cfg.Merchants,
				billing.MerchantDeclaration{MerchantDocument: billing.MerchantDocument{Slug: "gamma", DisplayName: "First"}},
				billing.MerchantDeclaration{MerchantDocument: billing.MerchantDocument{Slug: "Gamma", DisplayName: "Second"}},
			)
		})
		require.ErrorContains(t, configurationState(t, w.engine), "merchants.gamma: declared by merchants[2], merchants[3]")
		_, err := w.engine.DeclaredMerchant("gamma")
		require.ErrorIs(t, err, billing.ErrMerchantUnresolved, "neither declaration is guessed to be the merchant")
		alpha, err := w.engine.DeclaredMerchant("alpha")
		require.NoError(t, err, "the other declarations serve")
		require.Equal(t, w.alpha.MerchantID(), alpha)
	})
	t.Run("OR-J04-S77/a-vault-merchant-document-with-a-retired-key-names-its-replacement", func(t *testing.T) {
		t.Setenv(vaulttest.AddrEnv, os.Getenv("OPENRAILS_CRITICAL_VAULT_ADDR"))
		t.Setenv(vaulttest.TokenEnv, os.Getenv("OPENRAILS_CRITICAL_VAULT_TOKEN"))
		vault := vaulttest.New(t)
		w := newWorld(t, func(w *world) {
			w.cfg.Vault = vault.Config()
			w.deps.Vault = vault.Client()
			for i := range w.cfg.Merchants {
				w.cfg.Merchants[i].MerchantDocument = billing.MerchantDocument{Slug: w.cfg.Merchants[i].Slug}
			}
		})
		vault.Put(t, "openrails/merchants/"+w.alpha.MerchantID().UUID().String()+"/merchant", map[string]any{"display_name": "Alpha", "monthly_floor": "5000000"})
		// The next process to load it refuses it, saying what replaced the key.
		engine, err := openrails.New(t.Context(), w.cfg, w.deps)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, engine.Close(context.Background())) })
		require.ErrorContains(t, configurationState(t, engine), "monthly_floor was replaced by usage_billing.currencies[].delinquency_floor")
	})
}
