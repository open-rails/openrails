package handlers

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	solanago "github.com/doujins-org/solana-go"
	"github.com/open-rails/openrails/config"

	"github.com/google/uuid"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	jupiter "github.com/open-rails/openrails/internal/integrations/jupiter"
	"github.com/open-rails/openrails/internal/services"
	"github.com/open-rails/openrails/pkg/api"
	log "github.com/sirupsen/logrus"
)

// SupportedTokensQuery contains optional query parameters for the tokens endpoint.
type SupportedTokensQuery struct {
	// PriceID - if provided, calculates token quotes for this price
	PriceID string `form:"price_id"`
	// CheckoutSessionID - if provided, uses the price from this checkout session
	CheckoutSessionID string `form:"checkout_session_id"`
	// Wallet - if provided, fetches on-chain balances for this wallet
	Wallet string `form:"wallet"`
}

// GetSupportedTokens lists Solana tokens from the token registry and enriches them with live prices.
// Optional query params:
//   - price_id: Calculate quotes for each token based on a price
//   - checkout_session_id: Use the price from a checkout session
//   - wallet: Fetch on-chain balances for a wallet address
func GetSupportedTokens(r *httprequest.Request) {
	cfg := r.State.Config
	solanaProc := cfg.GetSolanaProcessor()
	if cfg == nil || solanaProc == nil {
		r.ErrorJSON(http.StatusInternalServerError, "Solana configuration missing")
		return
	}

	// Parse optional query params
	var query SupportedTokensQuery
	if !r.BindQuery(&query) {
		return
	}

	var tokenMap map[string]config.SolanaToken
	isDevnet := strings.ToLower(solanaProc.Network) == "devnet"

	if r.State.SolanaTokenRegistry != nil && r.State.SolanaTokenRegistry.Count() > 0 {
		registryTokens := r.State.SolanaTokenRegistry.All()
		tokenMap = make(map[string]config.SolanaToken, len(registryTokens))
		for symbol, rt := range registryTokens {
			mint := rt.MainnetMint
			if isDevnet && rt.DevnetMint != "" {
				mint = rt.DevnetMint
			}
			tokenMap[symbol] = config.SolanaToken{
				Symbol:      rt.Symbol,
				Name:        rt.Name,
				Decimals:    rt.Decimals,
				Mint:        mint,
				MainnetMint: rt.MainnetMint,
				Enabled:     true,
			}
		}
	} else {
		tokenMap = config.TokensForNetwork(solanaProc.Network)
	}

	mainnetMintSet := make(map[string]struct{})
	symbols := make([]string, 0, len(tokenMap))
	for symbol, t := range tokenMap {
		symbols = append(symbols, symbol)
		mint := t.MainnetMint
		if mint == "" {
			mint = t.Mint
		}
		if mint != "" {
			mainnetMintSet[mint] = struct{}{}
		}
	}

	mainnetMints := make([]string, 0, len(mainnetMintSet))
	for mint := range mainnetMintSet {
		mainnetMints = append(mainnetMints, mint)
	}

	ctx, cancel := context.WithTimeout(r.Request.Context(), 10*time.Second)
	defer cancel()

	prices, err := jupiter.FetchJupiterPrices(ctx, mainnetMints)
	if err != nil {
		log.WithError(err).Warn("Failed to fetch Solana token prices from Jupiter")
		prices = map[string]float64{}
	}

	// Get price info if requested (for quotes)
	var priceAmount int64
	var priceCurrency string
	var quoteError string

	if query.PriceID != "" {
		priceAmount, priceCurrency, quoteError = resolvePriceFromID(ctx, r, query.PriceID)
	} else if query.CheckoutSessionID != "" {
		priceAmount, priceCurrency, quoteError = resolvePriceFromSession(ctx, r, query.CheckoutSessionID)
	}

	// Get wallet balances if requested
	var balances map[string]uint64
	var solBalance uint64
	var walletError string
	if query.Wallet != "" {
		balances, solBalance, walletError = fetchWalletBalances(ctx, r, query.Wallet, mainnetMints)
	}

	sort.Strings(symbols)
	tokens := make([]TokenInfo, 0, len(symbols))
	quotedAt := time.Now()
	quoteExpiry := quotedAt.Add(15 * time.Minute)

	for _, symbol := range symbols {
		t := tokenMap[symbol]
		name := t.Name
		if name == "" {
			name = t.Symbol
		}
		mainnetMint := t.MainnetMint
		if mainnetMint == "" {
			mainnetMint = t.Mint
		}
		price := prices[mainnetMint]

		tokenInfo := TokenInfo{
			Symbol:   t.Symbol,
			Name:     name,
			Mint:     t.Mint,
			Decimals: t.Decimals,
			Price:    price,
		}

		// Add quote if price info was provided and we have token price
		if priceAmount > 0 && price > 0 && quoteError == "" {
			tokenInfo.Quote = calculateQuoteForToken(ctx, r, t, priceAmount, priceCurrency, price, quotedAt, quoteExpiry)
		}

		// Add balance if wallet was provided
		if query.Wallet != "" && walletError == "" {
			tokenInfo.Balance = calculateBalanceForToken(t, mainnetMint, balances, solBalance)

			// Set Sufficient flag if we have both quote and balance
			if tokenInfo.Quote != nil && tokenInfo.Balance != nil {
				tokenInfo.Balance.Sufficient = tokenInfo.Balance.Units >= tokenInfo.Quote.Units
			}
		}

		tokens = append(tokens, tokenInfo)
	}

	r.SuccessJSON(SupportedTokensResponse{Tokens: tokens})
}

// resolvePriceFromID fetches price amount and currency from a price ID.
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

// resolvePriceFromSession fetches price amount and currency from a checkout session.
func resolvePriceFromSession(ctx context.Context, r *httprequest.Request, sessionIDStr string) (int64, string, string) {
	if r.State.CheckoutSessionService == nil {
		return 0, "", "checkout session service unavailable"
	}

	sessionID, err := uuid.Parse(strings.TrimPrefix(sessionIDStr, "cs_"))
	if err != nil {
		return 0, "", fmt.Sprintf("invalid checkout_session_id: %v", err)
	}

	// Get user for session lookup
	user := r.GetUser()
	if user == nil {
		return 0, "", "authentication required for checkout_session_id"
	}

	session, err := r.State.CheckoutSessionService.GetSession(ctx, sessionID, user)
	if err != nil {
		return 0, "", fmt.Sprintf("session not found: %v", err)
	}

	// Get price from the session's price_id
	return resolvePriceFromID(ctx, r, session.PriceID)
}

// fetchWalletBalances fetches SOL and SPL token balances for a wallet.
func fetchWalletBalances(ctx context.Context, r *httprequest.Request, walletStr string, mints []string) (map[string]uint64, uint64, string) {
	if r.State.SolanaRPC == nil {
		return nil, 0, "solana rpc unavailable"
	}

	wallet, err := solanago.PublicKeyFromBase58(strings.TrimSpace(walletStr))
	if err != nil {
		return nil, 0, fmt.Sprintf("invalid wallet address: %v", err)
	}

	// Fetch SOL balance
	solBalance, err := r.State.SolanaRPC.GetBalance(ctx, wallet)
	if err != nil {
		log.WithError(err).Warn("Failed to fetch SOL balance")
		// Continue - we can still try to get token balances
	}

	// Fetch SPL token balances
	tokenAccounts, err := r.State.SolanaRPC.GetTokenBalances(ctx, wallet, mints)
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

// calculateQuoteForToken calculates the quote for a single token.
func calculateQuoteForToken(ctx context.Context, r *httprequest.Request, tokenCfg config.SolanaToken, amountCents int64, currency string, tokenPriceUSD float64, quotedAt, expiresAt time.Time) *TokenQuote {
	// Calculate using the service function
	quote, err := services.CalculateTokenQuote(ctx, tokenCfg, amountCents, currency, r.State.FXProvider)
	if err != nil {
		log.WithError(err).WithField("token", tokenCfg.Symbol).Warn("Failed to calculate token quote")
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

// calculateBalanceForToken calculates the balance info for a single token.
func calculateBalanceForToken(tokenCfg config.SolanaToken, mint string, balances map[string]uint64, solBalance uint64) *TokenBalance {
	var units uint64

	// Special handling for native SOL
	if tokenCfg.Symbol == "SOL" {
		units = solBalance
	} else {
		units = balances[mint]
	}

	scale := math.Pow10(tokenCfg.Decimals)
	amount := float64(units) / scale

	return &TokenBalance{
		Amount:     fmt.Sprintf("%.6f", amount),
		Units:      units,
		Sufficient: false, // Will be set by caller if quote is available
	}
}
