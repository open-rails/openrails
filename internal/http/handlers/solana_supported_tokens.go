package handlers

import (
	"context"
	"errors"
	"fmt"
	"github.com/open-rails/openrails/internal/db/models"
	solanarpc "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/merchant"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/api"
	log "github.com/sirupsen/logrus"
)

type SupportedTokensQuery struct {
	PriceID           string `form:"price_id"`
	CheckoutSessionID string `form:"checkout_session_id"`
	Wallet            string `form:"wallet"`
}

type SupportedTokensResponse struct {
	Tokens []TokenInfo `json:"tokens"`
}

type SolanaRuntimeConfigResponse struct {
	Network         string      `json:"network"`
	Chain           string      `json:"chain"`
	RPCURL          string      `json:"rpcUrl,omitempty"`
	ExplorerCluster string      `json:"explorerCluster,omitempty"`
	PreferredToken  string      `json:"preferredToken"`
	Tokens          []TokenInfo `json:"tokens"`
	Features        struct {
		SolanaPay                       bool `json:"solanaPay"`
		RecurringSubscriptions          bool `json:"recurringSubscriptions"`
		SolanaPayRecurringSubscriptions bool `json:"solanaPayRecurringSubscriptions"`
	} `json:"features"`
}

type TokenInfo struct {
	Symbol   string  `json:"symbol"`
	Name     string  `json:"name"`
	Mint     string  `json:"mint"`
	Decimals int     `json:"decimals"`
	Price    float64 `json:"price"`
	// Preferred marks the default stablecoin the frontend should present first
	// for Solana purchase options (USDC).
	Preferred bool `json:"preferred"`
	// RecurringEligible reports whether this token can back a recurring Solana
	// subscription (USDC, USD1) vs one-off only.
	RecurringEligible bool          `json:"recurring_eligible"`
	Quote             *TokenQuote   `json:"quote,omitempty"`
	Balance           *TokenBalance `json:"balance,omitempty"`
}

type TokenQuote struct {
	Amount        string  `json:"amount"`
	Units         uint64  `json:"units"`
	TokenPriceUSD float64 `json:"token_price_usd"`
	FXRate        float64 `json:"fx_rate"`
	FXCurrency    string  `json:"fx_currency"`
	QuotedAt      string  `json:"quoted_at"`
	ExpiresAt     string  `json:"expires_at"`
}

type TokenBalance struct {
	Amount     string `json:"amount"`
	Units      uint64 `json:"units"`
	Sufficient bool   `json:"sufficient"`
}

// effectiveSolanaRailConfig resolves the Solana runtime knobs for THIS request's
// merchant (#711): the boot-plane rail config overlaid with the merchant's
// declared rail-account `settings` (store-wins, #699). This is how standalone
// reaches the knobs — its boot plane is empty; the manifest declares them per
// merchant. nil (with nil error) means no Solana account on either plane.
// effectiveSolanaRailConfig resolves the ctx merchant's armed Solana rail
// config (#775/#788): the rail_merchant_accounts row's settings materialized
// over the test_mode-derived network + curated token defaults. nil = not
// armed (callers fail closed with their "not configured" errors).
func effectiveSolanaRailConfig(r *httprequest.Request) (*config.SolanaRailConfig, error) {
	if r.State.RailConfigs == nil {
		return nil, nil
	}
	proc, err := r.State.RailConfigs.RailConfig(r.Request.Context(), string(models.RailSolana), "")
	if err != nil {
		if errors.Is(err, railresolve.ErrRailNotArmed) {
			return nil, nil
		}
		return nil, fmt.Errorf("resolve merchant solana account: %w", err)
	}
	return proc.Solana, nil
}

func GetSupportedTokens(r *httprequest.Request) {
	cfg := r.State.Config
	if cfg == nil {
		r.ErrorJSON(http.StatusInternalServerError, "Solana configuration missing")
		return
	}
	solanaConf, err := effectiveSolanaRailConfig(r)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	if solanaConf == nil {
		r.ErrorJSON(http.StatusInternalServerError, "Solana configuration missing")
		return
	}

	var query SupportedTokensQuery
	if !r.BindQuery(&query) {
		return
	}

	tokenMap := normalizeTokenMap(solanaConf.Tokens)
	if len(tokenMap) == 0 {
		tokenMap = normalizeTokenMap(solanatokens.ForNetwork(solanaConf.Network))
	}

	mintSet := make(map[string]struct{})
	symbols := make([]string, 0, len(tokenMap))
	for symbol, t := range tokenMap {
		normalizedSymbol := strings.ToUpper(strings.TrimSpace(symbol))
		if normalizedSymbol == "" {
			continue
		}
		symbols = append(symbols, normalizedSymbol)
		if mint := strings.TrimSpace(t.Mint); mint != "" {
			mintSet[mint] = struct{}{}
		}
	}

	mints := make([]string, 0, len(mintSet))
	for mint := range mintSet {
		mints = append(mints, mint)
	}

	ctx, cancel := context.WithTimeout(r.Request.Context(), 10*time.Second)
	defer cancel()

	prices := make(map[string]float64, len(symbols))
	for _, symbol := range symbols {
		if r.State.SolanaPriceProvider == nil {
			continue
		}
		price, err := r.State.SolanaPriceProvider.PriceUSD(ctx, symbol)
		if err != nil {
			log.WithError(err).WithField("token", symbol).Warn("Failed to fetch Solana token price from Pyth")
			continue
		}
		prices[symbol] = price
	}

	var priceAmount int64
	var priceCurrency string
	var quoteError string

	if query.PriceID != "" {
		priceAmount, priceCurrency, quoteError = resolvePriceFromID(ctx, r, query.PriceID)
	} else if query.CheckoutSessionID != "" {
		priceAmount, priceCurrency, quoteError = resolvePriceFromSession(ctx, r, query.CheckoutSessionID)
	}

	var balances map[string]uint64
	var solBalance uint64
	var walletError string
	if query.Wallet != "" {
		balances, solBalance, walletError = fetchWalletBalances(ctx, r, query.Wallet, mints)
	}

	sort.Strings(symbols)
	tokens := make([]TokenInfo, 0, len(symbols))
	quotedAt := time.Now()
	quoteExpiry := quotedAt.Add(15 * time.Minute)

	for _, symbol := range symbols {
		t := tokenMap[symbol]
		name := t.Name
		if name == "" {
			name = symbol
		}
		mint := strings.TrimSpace(t.Mint)
		price := prices[symbol]

		tokenInfo := TokenInfo{
			Symbol:            symbol,
			Name:              name,
			Mint:              mint,
			Decimals:          t.Decimals,
			Price:             price,
			Preferred:         symbol == solanatokens.PreferredStablecoin,
			RecurringEligible: recurring.IsRecurringStablecoinSymbol(symbol),
		}

		if priceAmount > 0 && price > 0 && quoteError == "" {
			tokenInfo.Quote = calculateQuoteForToken(ctx, r, symbol, t, priceAmount, priceCurrency, quotedAt, quoteExpiry)
		}

		if query.Wallet != "" && walletError == "" {
			tokenInfo.Balance = calculateBalanceForToken(symbol, t, mint, balances, solBalance)
			if tokenInfo.Quote != nil && tokenInfo.Balance != nil {
				tokenInfo.Balance.Sufficient = tokenInfo.Balance.Units >= tokenInfo.Quote.Units
			}
		}

		tokens = append(tokens, tokenInfo)
	}

	r.SuccessJSON(SupportedTokensResponse{Tokens: tokens})
}

func GetSolanaConfig(r *httprequest.Request) {
	cfg := r.State.Config
	if cfg == nil {
		r.ErrorJSON(http.StatusInternalServerError, "Solana configuration missing")
		return
	}
	solanaConf, err := effectiveSolanaRailConfig(r)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	if solanaConf == nil {
		r.ErrorJSON(http.StatusInternalServerError, "Solana configuration missing")
		return
	}

	network := normalizeSolanaNetwork(solanaConf.Network)
	tokenMap := normalizeTokenMap(solanaConf.Tokens)
	if len(tokenMap) == 0 {
		tokenMap = normalizeTokenMap(solanatokens.ForNetwork(network))
	}

	symbols := make([]string, 0, len(tokenMap))
	for symbol := range tokenMap {
		symbols = append(symbols, symbol)
	}
	sort.Strings(symbols)

	tokens := make([]TokenInfo, 0, len(symbols))
	for _, symbol := range symbols {
		t := tokenMap[symbol]
		name := t.Name
		if name == "" {
			name = symbol
		}
		tokens = append(tokens, TokenInfo{
			Symbol:            symbol,
			Name:              name,
			Mint:              strings.TrimSpace(t.Mint),
			Decimals:          t.Decimals,
			Preferred:         symbol == solanatokens.PreferredStablecoin,
			RecurringEligible: recurring.IsRecurringStablecoinSymbol(symbol),
		})
	}

	resp := SolanaRuntimeConfigResponse{
		Network: network,
		Chain:   "solana:" + network,
		// RPCURL is intentionally empty (#352): there is no rpc_endpoint knob
		// anymore, and the server-side Helius key must never reach a browser.
		// Wallets/frontends bring their own RPC.
		RPCURL:          "",
		ExplorerCluster: explorerCluster(network),
		PreferredToken:  solanatokens.PreferredStablecoin,
		Tokens:          tokens,
	}
	resp.Features.SolanaPay = true
	resp.Features.RecurringSubscriptions = true
	// Always supported: rebillability is a property of the PRICE (catalog
	// auto_renew), never merchant config (v2 transaction system).
	resp.Features.SolanaPayRecurringSubscriptions = true

	r.SuccessJSON(resp)
}

func normalizeSolanaNetwork(network string) string {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case "devnet":
		return "devnet"
	case "testnet":
		return "testnet"
	default:
		return "mainnet"
	}
}

func explorerCluster(network string) string {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case "devnet", "testnet":
		return strings.ToLower(strings.TrimSpace(network))
	default:
		return ""
	}
}

func normalizeTokenMap(tokens map[string]config.TokenConfig) map[string]config.TokenConfig {
	normalized := make(map[string]config.TokenConfig, len(tokens))
	for symbol, token := range tokens {
		normalizedSymbol := strings.ToUpper(strings.TrimSpace(symbol))
		if normalizedSymbol == "" {
			continue
		}
		normalized[normalizedSymbol] = token
	}
	return normalized
}

func resolvePriceFromID(ctx context.Context, r *httprequest.Request, priceIDStr string) (int64, string, string) {
	if r.State.PriceService == nil {
		return 0, "", "price service unavailable"
	}

	priceID, err := api.ParsePriceID(priceIDStr)
	if err != nil {
		return 0, "", fmt.Sprintf("invalid price_id: %v", err)
	}

	price, err := r.State.PriceService.GetByID(ctx, priceID)
	if err != nil {
		return 0, "", fmt.Sprintf("price not found: %v", err)
	}

	return price.Amount, price.Currency, ""
}

func resolvePriceFromSession(ctx context.Context, r *httprequest.Request, sessionIDStr string) (int64, string, string) {
	if r.State.CheckoutSessionService == nil {
		return 0, "", "checkout session service unavailable"
	}

	sessionID, err := uuid.Parse(strings.TrimPrefix(sessionIDStr, "cs_"))
	if err != nil {
		return 0, "", fmt.Sprintf("invalid checkout_session_id: %v", err)
	}

	user := r.GetUser()
	if user == nil {
		return 0, "", "authentication required for checkout_session_id"
	}

	session, err := r.State.CheckoutSessionService.GetSession(ctx, sessionID, user)
	if err != nil {
		return 0, "", fmt.Sprintf("session not found: %v", err)
	}

	return resolvePriceFromID(ctx, r, session.PriceID)
}

// merchantSolanaRPC arms the ctx merchant's Solana RPC client through the
// #728 resolver. nil = not armed (callers answer "unavailable" — fail closed).
func merchantSolanaRPC(ctx context.Context, r *httprequest.Request) *solanarpc.RPCClient {
	if r.State == nil || r.State.SolanaRPCResolver == nil {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil
	}
	client, err := r.State.SolanaRPCResolver.Resolve(ctx, mid)
	if err != nil {
		log.WithContext(ctx).WithError(err).Warn("solana rpc resolution failed (fail closed)")
		return nil
	}
	return client
}

func fetchWalletBalances(ctx context.Context, r *httprequest.Request, walletStr string, mints []string) (map[string]uint64, uint64, string) {
	// #788: chain reads arm from the ctx merchant's declared solana account.
	rpc := merchantSolanaRPC(ctx, r)
	if rpc == nil {
		return nil, 0, "solana rpc unavailable"
	}

	wallet, err := solanago.PublicKeyFromBase58(strings.TrimSpace(walletStr))
	if err != nil {
		return nil, 0, fmt.Sprintf("invalid wallet address: %v", err)
	}

	solBalance, err := rpc.GetBalance(ctx, wallet)
	if err != nil {
		log.WithError(err).Warn("Failed to fetch SOL balance")
	}

	tokenAccounts, err := rpc.GetTokenBalances(ctx, wallet, mints)
	if err != nil {
		log.WithError(err).Warn("Failed to fetch token balances")
		return nil, solBalance, ""
	}

	balances := make(map[string]uint64)
	for _, acc := range tokenAccounts {
		balances[acc.Mint] = acc.Balance
	}

	return balances, solBalance, ""
}

func calculateQuoteForToken(ctx context.Context, r *httprequest.Request, tokenSymbol string, tokenCfg config.TokenConfig, amountMicros int64, currency string, quotedAt, expiresAt time.Time) *TokenQuote {
	quote, err := solanamodule.CalculateTokenQuote(ctx, tokenSymbol, tokenCfg, moneyutil.Micros(amountMicros), currency, r.State.FXProvider, r.State.SolanaPriceProvider)
	if err != nil {
		log.WithError(err).WithField("token", tokenSymbol).Warn("Failed to calculate token quote")
		return nil
	}

	return &TokenQuote{
		Amount:        fmt.Sprintf("%.6f", quote.Decimal),
		Units:         quote.Units,
		TokenPriceUSD: quote.TokenPriceUSD,
		FXRate:        quote.FXRate,
		FXCurrency:    quote.FXCurrency,
		QuotedAt:      quotedAt.Format(time.RFC3339),
		ExpiresAt:     expiresAt.Format(time.RFC3339),
	}
}

func calculateBalanceForToken(tokenSymbol string, tokenCfg config.TokenConfig, mint string, balances map[string]uint64, solBalance uint64) *TokenBalance {
	var units uint64

	if strings.EqualFold(tokenSymbol, "SOL") {
		units = solBalance
	} else {
		units = balances[mint]
	}

	scale := math.Pow10(tokenCfg.Decimals)
	amount := float64(units) / scale

	return &TokenBalance{
		Amount:     fmt.Sprintf("%.6f", amount),
		Units:      units,
		Sufficient: false,
	}
}
