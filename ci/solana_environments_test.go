//go:build e2e && integration

package ci_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/solanafake"
)

// A Solana PSP's environment is its network, and every endpoint it uses must
// report that network's genesis hash before the PSP arms. Its rpc_url is the
// primary endpoint and the network's public one its fallback; the URL, which
// carries the provider's key, reaches no log, error or browser document.
func TestSolanaNetworkIsVerifiedOnEveryEndpoint(t *testing.T) {
	pool := openPool(t, emptyDatabase(t))
	ctx := t.Context()
	public, primary := solanafake.New(), solanafake.New()
	t.Cleanup(public.Close)
	t.Cleanup(primary.Close)
	const token = "e2e-rpc-provider-key"
	treasury := solanago.NewWallet().PublicKey()
	logs := test.NewGlobal()
	t.Cleanup(logs.Reset)

	boot := func(environment, rpcURL string) (*openrails.Engine, func() error) {
		t.Helper()
		cfg := ownIssuer(openrails.Config{
			ProviderWriteMode: openrails.ProviderWritesFull,
			ProviderSandbox:   &openrails.ProviderSandboxConfig{SolanaRPCURL: public.URL()},
			Merchants: map[string]openrails.MerchantDeclaration{"wallet-shop": {DisplayName: "Wallet Shop", PSPs: openrails.PSPs{
				"solana": openrails.SolanaPSP{Environment: environment, Treasury: treasury.String(), RPCURL: rpcURL}.PSPConfig(),
			}}},
		})
		bill, err := openrails.New(ctx, cfg, openrails.Deps{Postgres: pool, FXTransport: testFX.Transport()})
		require.NoError(t, err)
		posture := func() error {
			for _, p := range bill.Probes() {
				if p.Name == "openrails_psp_posture" {
					return p.Check(ctx)
				}
			}
			return nil
		}
		return bill, posture
	}
	id := merchants.PspID("solana", openrails.EnvironmentDevnet, treasury.String())
	require.Equal(t, id, merchants.PspID("solana", openrails.EnvironmentTestnet, treasury.String()), "devnet and testnet are one money class")

	// devnet on both endpoints arms; reads use the rpc_url, then fall back.
	bill, posture := boot(openrails.EnvironmentDevnet, primary.URL()+"/"+token)
	rt := engine.Graph(bill).Runtime
	require.Eventually(t, func() bool { return posture() == nil }, 20*time.Second, 50*time.Millisecond)
	require.False(t, rt.PSPPostureDisarmed(id))
	cfg := customerConfig(t, bill)
	require.NotNil(t, cfg.Payment.Solana)
	require.Equal(t, "devnet", cfg.Payment.Solana.Network)
	symbols := []string{}
	for _, token := range cfg.Payment.Solana.Tokens {
		symbols = append(symbols, token.Symbol)
	}
	require.ElementsMatch(t, []string{"SOL", "USDC"}, symbols, "devnet accepts SOL and USDC, nothing else")
	browser, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.NotContains(t, string(browser), token)

	shop, err := bill.MerchantBySlug("wallet-shop")
	require.NoError(t, err)
	mctx := merchant.WithID(ctx, shop.MerchantID())
	client, err := rt.SolanaRPCResolver.Resolve(mctx, shop.MerchantID())
	require.NoError(t, err)
	funded := solanago.NewWallet().PublicKey()
	public.Fund(funded, solanago.MustPublicKeyFromBase58(solanafake.DevnetUSDCMint), 7)
	primary.Close()
	balance, err := client.GetTokenBalanceForMint(mctx, funded, solanago.MustPublicKeyFromBase58(solanafake.DevnetUSDCMint))
	require.NoError(t, err, "the public endpoint answers while the rpc_url is down")
	require.Equal(t, uint64(7), balance)
	require.NoError(t, bill.Close(ctx))

	// An rpc_url on another network disarms the PSP, saying which endpoint.
	elsewhere := solanafake.New()
	t.Cleanup(elsewhere.Close)
	elsewhere.SetNetwork("testnet")
	bill, posture = boot(openrails.EnvironmentDevnet, elsewhere.URL()+"/"+token)
	require.Eventually(t, func() bool { return engine.Graph(bill).Runtime.PSPPostureDisarmed(id) }, 20*time.Second, 50*time.Millisecond)
	err = posture()
	require.ErrorContains(t, err, "solana endpoint rpc_url is on testnet; the PSP declares devnet")
	require.NotContains(t, err.Error(), token)
	require.NoError(t, bill.Close(ctx))

	// A different account declared testnet whose public endpoint is devnet
	// is disarmed too. Existing account networks cannot be changed.
	treasury = solanago.NewWallet().PublicKey()
	id = merchants.PspID("solana", openrails.EnvironmentTestnet, treasury.String())
	bill, posture = boot(openrails.EnvironmentTestnet, "")
	require.Eventually(t, func() bool { return engine.Graph(bill).Runtime.PSPPostureDisarmed(id) }, 20*time.Second, 50*time.Millisecond)
	require.ErrorContains(t, posture(), "solana endpoint Solana testnet public is on devnet; the PSP declares testnet")
	shop, err = bill.MerchantBySlug("wallet-shop")
	require.NoError(t, err)
	psps, err := shop.ListPSPs(ctx, billing.PSPListParams{})
	require.NoError(t, err)
	require.Len(t, psps.Items, 2)
	var found bool
	for _, psp := range psps.Items {
		if psp.ID.UUID() == id {
			require.Equal(t, openrails.EnvironmentTestnet, psp.Environment)
			require.Equal(t, treasury.String(), psp.Treasury)
			found = true
		}
	}
	require.True(t, found)
	require.NoError(t, bill.Close(ctx))

	for _, entry := range logs.AllEntries() {
		line, err := entry.String()
		require.NoError(t, err)
		require.NotContains(t, line, token, "the rpc_url never reaches a log line")
		require.False(t, entry.Level <= logrus.ErrorLevel && strings.Contains(line, token))
	}
}

// A Solana PSP omitting its environment is on mainnet, its rail's real-money
// network; a rail refuses another rail's environment; and each PSP picks its
// own environment, so live and test PSPs mix on one merchant. Each refusal is
// the PSP's alone.
func TestSolanaEnvironmentsAreTheRails(t *testing.T) {
	ctx := t.Context()
	treasury := solanago.NewWallet().PublicKey().String()
	declare := func(psps openrails.PSPs) openrails.Config {
		return openrails.Config{ProviderWriteMode: openrails.ProviderWritesReadOnly,
			Merchants: map[string]openrails.MerchantDeclaration{"shop": {DisplayName: "Shop", PSPs: psps}}}
	}
	for name, row := range map[string]struct {
		psps openrails.PSPs
		want string
	}{
		"solana sandbox": {openrails.PSPs{"wallet": openrails.SolanaPSP{Environment: openrails.EnvironmentSandbox, Treasury: treasury}.PSPConfig()},
			`psps.wallet.environment: solana has no "sandbox" environment (mainnet, devnet, testnet)`},
		"nmi devnet": {openrails.PSPs{"mobius": openrails.NMIPSP{Environment: openrails.EnvironmentDevnet, GatewayID: "100001"}.PSPConfig()},
			`psps.mobius.environment: nmi has no "devnet" environment (live, test, sandbox)`},
		"a live Stripe PSP holding a test key": {openrails.PSPs{"stripe": openrails.StripePSP{AccountID: "acct_1", SecretKey: "sk_test_abc", WebhookSigningSecret: "whsec_1"}.PSPConfig()},
			"a test key (sk_test_/rk_test_) moves no money; the PSP declares environment live"},
	} {
		bill, err := openrails.New(ctx, declare(row.psps), openrails.Deps{Postgres: openPool(t, emptyDatabase(t))})
		require.NoError(t, err, name)
		require.ErrorContains(t, probe(t, bill, "openrails_configuration"), row.want, name)
		require.NoError(t, bill.Close(ctx))
	}

	pool := openPool(t, emptyDatabase(t))
	bill, err := openrails.New(ctx, declare(openrails.PSPs{
		"wallet": openrails.SolanaPSP{Treasury: treasury}.PSPConfig(),
		"mobius": openrails.NMIPSP{Environment: openrails.EnvironmentSandbox, GatewayID: "100001"}.PSPConfig(),
	}), openrails.Deps{Postgres: pool})
	require.NoError(t, err)
	t.Cleanup(func() { _ = bill.Close(ctx) })
	if err := probe(t, bill, "openrails_configuration"); err != nil {
		require.NotContains(t, err.Error(), ".environment", "mainnet beside nmi sandbox: each PSP keeps its own environment")
	}
	shop, err := bill.MerchantBySlug("shop")
	require.NoError(t, err)
	psps, err := shop.ListPSPs(ctx, billing.PSPListParams{})
	require.NoError(t, err)
	require.Len(t, psps.Items, 2)
	environments := map[string]string{}
	for _, p := range psps.Items {
		environments[p.Key] = p.Environment
		if p.Key == "wallet" {
			require.Equal(t, merchants.PspID("solana", "mainnet", treasury), p.ID.UUID())
		}
	}
	require.Equal(t, map[string]string{"wallet": openrails.EnvironmentMainnet, "mobius": openrails.EnvironmentSandbox}, environments, "omitted is mainnet; each PSP keeps its own")
	for rail, want := range map[string]bool{"solana": true, "nmi": false} {
		var live bool
		require.NoError(t, pool.QueryRow(ctx, "SELECT live FROM billing.psps WHERE rail = $1", rail).Scan(&live))
		require.Equal(t, want, live, rail)
	}
}
