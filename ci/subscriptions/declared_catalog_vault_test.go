//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	vaultapi "github.com/hashicorp/vault/api"
	riverkit "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/vaulttest"
)

// heldVault is a Vault that answers nothing until release: OpenRails' first
// login is still in flight while New runs.
func heldVault(t *testing.T, v *vaulttest.Vault) (addr string, release func()) {
	t.Helper()
	target, err := url.Parse(v.Addr)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	open := make(chan struct{})
	front := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		select {
		case <-open:
			proxy.ServeHTTP(rw, r)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(front.Close)
	var once sync.Once
	release = func() { once.Do(func() { close(open) }) }
	t.Cleanup(release)
	return front.URL, release
}

// A catalog selling on a PSP whose account waits on Vault (a Solana Transit
// signer, before OpenRails' first Vault login) does not fail New: it applies
// once Vault answers, and Ready fails until then. Archived prices naming that
// PSP wait too. A PSP the declaration does not name is refused at once.
func TestDeclaredCatalogAwaitsVault(t *testing.T) {
	t.Parallel()
	w := prepareWorld(t, 12)
	v := vaulttest.New(t)
	transit := "transit-" + uuid.NewString()[:8]
	require.NoError(t, v.Client().Sys().MountWithContext(t.Context(), transit, &vaultapi.MountInput{Type: "transit"}))
	t.Cleanup(func() { _ = v.Client().Sys().UnmountWithContext(context.Background(), transit) })
	_, err := v.Client().Logical().WriteWithContext(t.Context(), transit+"/keys/signer", map[string]any{"type": "ed25519"})
	require.NoError(t, err)
	addr, release := heldVault(t, v)

	psps := map[string]openrails.PSPConfig{
		"nmi":    openrails.NMIPSP{AccountID: nmiAcct, SecurityKey: "e2e-nmi-key", WebhookSigningSecret: whsecNMI, TokenizationKey: "e2e-tokenization"}.PSPConfig(),
		"solana": openrails.SolanaPSP{TransitKey: "signer"}.PSPConfig(),
	}
	boot := func(doc string) (*openrails.Client, error) {
		params, err := catalog.ParseApplicationYAML([]byte(doc))
		require.NoError(t, err)
		return openrails.New(t.Context(), openrails.Config{
			Database: openrails.DatabaseConfig{Schema: w.schema, RiverSchema: w.schema}, TestMode: openrails.Sandbox, ProviderWriteMode: openrails.ProviderWritesFull,
			Vault:           &openrails.VaultConfig{Address: addr, Token: v.Token, TransitMount: transit},
			ProviderSandbox: &openrails.ProviderSandboxConfig{SolanaRPCURL: "http://127.0.0.1:1"},
			Merchant:        openrails.MerchantDeclaration{Slug: w.slug, DisplayName: w.slug, PSPs: psps},
			Catalog:         params,
		}, openrails.Deps{FXTransport: testFX.Transport(), Postgres: w.pool, NMITransport: w.nmi})
	}
	key := "vault-" + uuid.NewString()[:8]

	_, err = boot(fmt.Sprintf(`schema_version: 1
products:
  %[1]s:
    display_name: Typo
    prices:
      %[1]s-once:
        currency: usd
        unit_amount: 4990000
        psps: [nmi, nowhere]
`, key))
	require.ErrorContains(t, err, `provider "nowhere" has no matching merchant account`, "a PSP the declaration does not name is refused at once")

	start := time.Now()
	bill, err := boot(fmt.Sprintf(`schema_version: 1
products:
  %[1]s:
    display_name: Premium
    entitlements: ["%[1]s"]
    prices:
      %[1]s-once:
        currency: usd
        unit_amount: 4990000
        psps: [nmi, solana]
      %[1]s-legacy:
        currency: usd
        unit_amount: 1990000
        archived: true
        psps: [solana]
`, key))
	require.NoError(t, err, "a PSP whose account waits on Vault never fails New")
	require.Less(t, time.Since(start), 20*time.Second, "New never waits on Vault")
	t.Cleanup(func() { _ = bill.Close(context.Background()) })
	_, err = riverkit.New(t.Context(), w.pool, &river.Config{Schema: w.schema, Queues: map[string]river.QueueConfig{openrails.QueueBilling: {MaxWorkers: 1}}}, bill.RiverJobs())
	require.NoError(t, err)
	require.ErrorContains(t, bill.Ready(t.Context()), `declared PSP "solana" not provisioned yet`)
	_, err = priceByKey(t.Context(), bill, key, key+"-once")
	require.ErrorIs(t, err, billing.ErrNotFound)

	release()
	require.Eventually(t, func() bool { return bill.Ready(t.Context()) == nil }, 30*time.Second, 50*time.Millisecond, "the catalog applies once Vault answers")
	price, err := priceByKey(t.Context(), bill, key, key+"-once")
	require.NoError(t, err)
	require.Contains(t, price.PSPs, "solana")
	archived := true
	legacy, err := bill.ListPrices(t.Context(), billing.PriceListParams{ProductKey: key, Key: key + "-legacy", Archived: &archived})
	require.NoError(t, err)
	require.Len(t, legacy.Items, 1)
	require.Contains(t, legacy.Items[0].PSPs, "solana")
}
