//go:build integration

package critical_test

import (
	"context"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/vaulttest"
	"github.com/stretchr/testify/require"
)

// configurationState is the engine's openrails_configuration probe: what it
// does not serve, with why.
func configurationState(t *testing.T, engine *openrails.Engine) error {
	t.Helper()
	return configurationProbe(t, engine).Check(t.Context())
}

// configurationProbe is the engine's openrails_configuration probe.
func configurationProbe(t *testing.T, engine *openrails.Engine) openrails.Probe {
	t.Helper()
	for _, p := range engine.Probes() {
		if p.Name == "openrails_configuration" {
			return p
		}
	}
	t.Fatal("no openrails_configuration probe")
	return openrails.Probe{}
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
	t.Run("OR-J04-S82/provider-endpoints-are-loopback-fakes-never-beside-a-live-psp", func(t *testing.T) {
		gateway := nmimock.New(nmimock.Options{})
		t.Cleanup(gateway.Close)
		nmi := func(environment billing.Environment) billing.PSPs {
			return billing.PSPs{"nmi": openrails.NMIPSP{Environment: environment, GatewayID: "endpoints", SecurityKey: "critical-endpoints", TokenizationKey: "critical-endpoints-tokenization"}.PSPConfig()}
		}
		w := newWorld(t, func(w *world) {
			w.deps.ProviderEndpoints = openrails.ProviderEndpoints{NMIGatewayURL: gateway.URL()}
			m := cfgMerchant(w.cfg, "alpha")
			m.PSPs = nmi(billing.EnvironmentSandbox)
			setCfgMerchant(&w.cfg, "alpha", m)
		})
		require.NoError(t, configurationState(t, w.engine), "a sandbox PSP serves beside a loopback fake")

		for _, c := range []struct {
			endpoints openrails.ProviderEndpoints
			says      string
		}{
			{openrails.ProviderEndpoints{NMIGatewayURL: "http://localhost:9091"}, `NMIGatewayURL: provider endpoint refused: host "localhost" must be a loopback IP literal`},
			{openrails.ProviderEndpoints{StripeAPIURL: "http://192.0.2.1:9092"}, `StripeAPIURL: provider endpoint refused: host "192.0.2.1" is not a loopback address`},
			{openrails.ProviderEndpoints{CCBillDataLinkURL: "http://user:secret@127.0.0.1:9094"}, "CCBillDataLinkURL: provider endpoint refused:"},
			{openrails.ProviderEndpoints{CCBillWebhookSources: []string{"0.0.0.0/0"}}, `CCBillWebhookSources: CIDR "0.0.0.0/0" matches every address`},
		} {
			_, err := openrails.New(t.Context(), w.cfg, openrails.Deps{Postgres: w.pool, ProviderEndpoints: c.endpoints})
			require.ErrorContains(t, err, "Deps.ProviderEndpoints: "+c.says)
		}

		live := w.cfg
		live.Merchants = slices.Clone(w.cfg.Merchants)
		m := cfgMerchant(live, "alpha")
		m.PSPs = nmi(billing.EnvironmentLive)
		setCfgMerchant(&live, "alpha", m)
		_, err := openrails.New(t.Context(), live, openrails.Deps{Postgres: w.pool, ProviderEndpoints: openrails.ProviderEndpoints{NMIGatewayURL: gateway.URL()}})
		require.ErrorContains(t, err, "Deps.ProviderEndpoints.NMIGatewayURL is a test seam and is refused beside a live PSP (alpha/nmi)")
	})
	t.Run("OR-J04-S73/a-kv-mount-without-a-vault-client-is-refused-and-reported", func(t *testing.T) {
		w := newWorld(t, func(w *world) {
			w.cfg.Vault = billing.VaultConfig{KVMount: "secret"}
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
	t.Run("OR-J04-S85/a-vault-login-still-under-way-is-waited-for-never-reported-as-refused", func(t *testing.T) {
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
		require.NoError(t, configurationState(t, w.engine))
		// The host's helpers/vault session: its client holds no token until
		// its first login sets one in place.
		loggingIn := func(after time.Duration) *vaultapi.Client {
			host, err := vault.Client().Clone()
			require.NoError(t, err)
			host.ClearToken()
			login := time.AfterFunc(after, func() { host.SetToken(vault.Token) })
			t.Cleanup(func() { login.Stop() })
			return host
		}
		start := func(client *vaultapi.Client) *openrails.Engine {
			deps := w.deps
			deps.Vault = client
			engine, err := openrails.New(t.Context(), w.cfg, deps)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, engine.Close(context.Background())) })
			return engine
		}
		require.NoError(t, configurationState(t, start(loggingIn(time.Second))), "New waits for a login under way")

		// A login that outlasts New's wait for the configuration is reported
		// as one under way, and served once it completes.
		slow := start(loggingIn(7 * time.Second))
		for served := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			state := configurationState(t, slow)
			if state == nil {
				break
			}
			require.Contains(t, state.Error(), "awaiting the host's first login")
			require.NotContains(t, state.Error(), "credential access denied")
			require.True(t, time.Now().Before(served), "not served once the login completed: %v", state)
		}
	})
	t.Run("OR-J04-S77/a-vault-merchant-document-with-an-unknown-key-is-refused", func(t *testing.T) {
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
		// The next process to load it refuses it, naming the key.
		// The configuration loads after New returns.
		engine, err := openrails.New(t.Context(), w.cfg, w.deps)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, engine.Close(context.Background())) })
		probe := configurationProbe(t, engine)
		const want = `unknown field "monthly_floor"`
		require.Eventually(t, func() bool {
			err := probe.Check(t.Context())
			return err != nil && strings.Contains(err.Error(), want)
		}, 30*time.Second, 50*time.Millisecond, want)
	})
}
