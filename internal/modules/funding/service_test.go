package funding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/openrails/config"
)

const testSolanaWallet = "11111111111111111111111111111111"

func TestOptionsAreSolanaOnly(t *testing.T) {
	svc := NewService(nil, &config.Config{USDCFunding: &config.USDCFundingConfig{Providers: map[string]*config.USDCFundingProviderConfig{
		ProviderRobinhood: {Enabled: true, LaunchURLTemplate: "https://example.com?wallet={wallet}"},
		ProviderCoinbase:  {Enabled: true, LaunchURLTemplate: "https://example.com?wallet={wallet}"},
	}}})

	opts := svc.Options(OptionsRequest{WalletAddress: testSolanaWallet, Network: "solana", Asset: "USDC", Amount: "10"})
	if len(opts) != 2 {
		t.Fatalf("expected 2 options, got %d", len(opts))
	}
	for _, opt := range opts {
		if !opt.Enabled {
			t.Fatalf("%s should be enabled: %s", opt.Provider, opt.Reason)
		}
		if len(opt.Networks) != 1 || opt.Networks[0] != "solana" || opt.Network != "solana" {
			t.Fatalf("%s networks = %+v / %q, want solana only", opt.Provider, opt.Networks, opt.Network)
		}
	}

	baseOpts := svc.Options(OptionsRequest{WalletAddress: "0x0000000000000000000000000000000000000001", Network: "base", Asset: "USDC", Amount: "10"})
	for _, opt := range baseOpts {
		if opt.Enabled {
			t.Fatalf("%s must not be enabled for base", opt.Provider)
		}
	}
}

func TestCoinbaseSessionCreationUsesSolanaDestination(t *testing.T) {
	var got map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/platform/v2/onramp/sessions" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("missing bearer auth: %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"session":{"onrampUrl":"https://pay.coinbase.com/buy?sessionToken=abc"}}`))
	}))
	defer server.Close()

	svc := NewService(nil, &config.Config{USDCFunding: &config.USDCFundingConfig{Providers: map[string]*config.USDCFundingProviderConfig{
		ProviderCoinbase: {Enabled: true, APIBaseURL: server.URL, APIKey: "secret"},
	}}})

	url, _, err := svc.createProviderURL(context.Background(), ProviderCoinbase, providerURLRequest{
		SessionID:      "ufs_test",
		WalletAddress:  testSolanaWallet,
		Network:        "solana",
		Asset:          "USDC",
		Amount:         "12.5",
		ReturnURL:      "https://doujins.com/subscribe?funding=ufs_test",
		PartnerUserRef: "ufs_test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://pay.coinbase.com/buy?sessionToken=abc" {
		t.Fatalf("url = %q", url)
	}
	if got["destinationNetwork"] != "solana" {
		t.Fatalf("destinationNetwork = %q, want solana", got["destinationNetwork"])
	}
	if got["destinationAddress"] != testSolanaWallet {
		t.Fatalf("destinationAddress = %q", got["destinationAddress"])
	}
	if got["purchaseCurrency"] != "USDC" {
		t.Fatalf("purchaseCurrency = %q", got["purchaseCurrency"])
	}
}
