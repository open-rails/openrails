package funding

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
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

func TestCoinbaseSessionCreationGeneratesCDPJWT(t *testing.T) {
	seed := []byte("0123456789abcdef0123456789abcdef")
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	keyID := "merchant/apiKeys/key"
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"session":{"onrampUrl":"https://pay.coinbase.com/buy?sessionToken=jwt"}}`))
	}))
	defer server.Close()

	svc := NewService(nil, &config.Config{USDCFunding: &config.USDCFundingConfig{Providers: map[string]*config.USDCFundingProviderConfig{
		ProviderCoinbase: {
			Enabled:      true,
			APIBaseURL:   server.URL,
			APIKeyID:     keyID,
			APIKeySecret: base64.StdEncoding.EncodeToString(privateKey),
		},
	}}})
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }

	url, _, err := svc.createProviderURL(context.Background(), ProviderCoinbase, providerURLRequest{
		SessionID:      "ufs_test",
		WalletAddress:  testSolanaWallet,
		Network:        "solana",
		Asset:          "USDC",
		Amount:         "12.5",
		PartnerUserRef: "ufs_test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://pay.coinbase.com/buy?sessionToken=jwt" {
		t.Fatalf("url = %q", url)
	}
	rawToken := strings.TrimPrefix(gotAuth, "Bearer ")
	if rawToken == gotAuth || rawToken == "" {
		t.Fatalf("missing bearer jwt: %q", gotAuth)
	}
	parsed, err := jwt.Parse(rawToken, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodEdDSA {
			t.Fatalf("method = %v, want EdDSA", token.Method)
		}
		return publicKey, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Valid {
		t.Fatal("jwt is invalid")
	}
	claims := parsed.Claims.(jwt.MapClaims)
	if claims["iss"] != "cdp" || claims["sub"] != keyID {
		t.Fatalf("unexpected claims: %#v", claims)
	}
	if claims["uri"] != "POST "+strings.TrimPrefix(server.URL, "http://")+"/platform/v2/onramp/sessions" {
		t.Fatalf("uri claim = %#v", claims["uri"])
	}
	if parsed.Header["kid"] != keyID {
		t.Fatalf("kid header = %#v", parsed.Header["kid"])
	}
	if parsed.Header["nonce"] == "" {
		t.Fatalf("nonce header missing: %#v", parsed.Header)
	}
}

func TestRefreshSolanaFundingStatusMarksFundedFromWalletBalance(t *testing.T) {
	svc := NewService(nil, testFundingConfig(), testFundingRails()).WithSolanaBalanceReader(fakeSolanaBalanceReader{
		balance: 12_500_000,
	})
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	session := &models.USDCFundingSession{
		Status:          models.USDCFundingSessionCreated,
		WalletAddress:   testSolanaWallet,
		Network:         "solana",
		Asset:           "USDC",
		RequestedAmount: "12.50",
		Metadata:        map[string]any{"funding_only": true},
	}

	if err := svc.refreshSolanaFundingStatus(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	if session.Status != models.USDCFundingSessionFunded {
		t.Fatalf("status = %q, want funded", session.Status)
	}
	if session.LastCheckedAt == nil || !session.LastCheckedAt.Equal(now) {
		t.Fatalf("last_checked_at = %v, want %v", session.LastCheckedAt, now)
	}
	check, ok := session.Metadata["balance_check"].(map[string]any)
	if !ok {
		t.Fatalf("balance_check metadata missing: %#v", session.Metadata)
	}
	if check["funded"] != true {
		t.Fatalf("balance_check.funded = %#v, want true", check["funded"])
	}
}

func TestRefreshSolanaFundingStatusDoesNotMarkFundedWhenBalanceShort(t *testing.T) {
	svc := NewService(nil, testFundingConfig(), testFundingRails()).WithSolanaBalanceReader(fakeSolanaBalanceReader{
		balance: 12_499_999,
	})
	session := &models.USDCFundingSession{
		Status:          models.USDCFundingSessionCreated,
		WalletAddress:   testSolanaWallet,
		Network:         "solana",
		Asset:           "USDC",
		RequestedAmount: "12.50",
		Metadata:        map[string]any{},
	}

	if err := svc.refreshSolanaFundingStatus(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	if session.Status != models.USDCFundingSessionPendingSettlement {
		t.Fatalf("status = %q, want pending_settlement", session.Status)
	}
	check := session.Metadata["balance_check"].(map[string]any)
	if check["funded"] != false {
		t.Fatalf("balance_check.funded = %#v, want false", check["funded"])
	}
}

func TestProviderWebhookStatusMapsCoinbaseOnrampStates(t *testing.T) {
	tests := []struct {
		eventType string
		status    string
		want      models.USDCFundingSessionStatus
	}{
		{eventType: "onramp.transaction.created", status: "ONRAMP_TRANSACTION_STATUS_IN_PROGRESS", want: models.USDCFundingSessionPendingProvider},
		{eventType: "onramp.transaction.updated", status: "ONRAMP_ORDER_STATUS_COMPLETED", want: models.USDCFundingSessionPendingSettlement},
		{eventType: "onramp.transaction.success", status: "", want: models.USDCFundingSessionPendingSettlement},
		{eventType: "onramp.transaction.failed", status: "ONRAMP_TRANSACTION_STATUS_FAILED", want: models.USDCFundingSessionFailed},
		{eventType: "", status: "ONRAMP_TRANSACTION_STATUS_CANCELLED", want: models.USDCFundingSessionCancelled},
	}
	for _, tt := range tests {
		if got := providerWebhookStatus(tt.eventType, tt.status); got != tt.want {
			t.Fatalf("providerWebhookStatus(%q, %q) = %q, want %q", tt.eventType, tt.status, got, tt.want)
		}
	}
}

func TestDecimalAmountToBaseUnits(t *testing.T) {
	tests := []struct {
		amount   string
		decimals int
		want     uint64
	}{
		{amount: "12.50", decimals: 6, want: 12_500_000},
		{amount: "0.01", decimals: 6, want: 10_000},
		{amount: "1", decimals: 6, want: 1_000_000},
		{amount: "1.000001", decimals: 6, want: 1_000_001},
	}
	for _, tt := range tests {
		got, err := decimalAmountToBaseUnits(tt.amount, tt.decimals)
		if err != nil {
			t.Fatalf("decimalAmountToBaseUnits(%q): %v", tt.amount, err)
		}
		if got != tt.want {
			t.Fatalf("decimalAmountToBaseUnits(%q) = %d, want %d", tt.amount, got, tt.want)
		}
	}
	if _, err := decimalAmountToBaseUnits("1.0000001", 6); err == nil {
		t.Fatal("expected precision error")
	}
}

type fakeSolanaBalanceReader struct {
	balance uint64
	err     error
}

func (f fakeSolanaBalanceReader) GetTokenBalanceForMint(context.Context, solanago.PublicKey, solanago.PublicKey) (uint64, error) {
	return f.balance, f.err
}

func testFundingConfig() *config.Config {
	return &config.Config{}
}

func testFundingRails() config.RailSet {
	return config.RailSet{
		"solana": {
			Type: config.RailTypeSolana,
			Solana: &config.SolanaRailConfig{
				Tokens: map[string]config.TokenConfig{
					"USDC": {
						Mint:     "11111111111111111111111111111111",
						Decimals: 6,
					},
				},
			},
		},
	}
}
