package handlers

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
	"github.com/open-rails/openrails/internal/railresolve"
	log "github.com/sirupsen/logrus"
)

// effectiveSolanaRailConfig resolves the ctx merchant's armed Solana rail
// config: its PSP's network and the tokens it accepts there. nil = not armed.
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

// acceptedSolanaTokens lists the tokens the merchant accepts on its network,
// dropping any whose mint decimals cannot be read. A token rebills only while
// the deployment has a cranker.
func acceptedSolanaTokens(r *httprequest.Request, solanaConf *config.SolanaRailConfig) []billing.SolanaPaymentToken {
	tokenMap := normalizeTokenMap(solanaConf.Tokens)
	symbols := make([]string, 0, len(tokenMap))
	for symbol := range tokenMap {
		symbols = append(symbols, symbol)
	}
	sort.Strings(symbols)

	tokens := make([]billing.SolanaPaymentToken, 0, len(symbols))
	for _, symbol := range symbols {
		t := tokenMap[symbol]
		name := t.Name
		if name == "" {
			name = symbol
		}
		mint := strings.TrimSpace(t.Mint)
		decimals, err := solanamodule.RequireMintDecimals(r.Request.Context(), r.State.SolanaMintDecimals, mint)
		if err != nil {
			log.WithError(err).WithField("token", symbol).Warn("solana token dropped: mint decimals unreadable on-chain")
			continue
		}
		tokens = append(tokens, billing.SolanaPaymentToken{
			Symbol:            symbol,
			Name:              name,
			Mint:              mint,
			Decimals:          decimals,
			Preferred:         symbol == solanatokens.PreferredStablecoin,
			RecurringEligible: r.State.SolanaPlanService != nil && recurring.IsRecurringStablecoinSymbol(symbol),
		})
	}
	return tokens
}

// solanaPaymentConfig projects the armed Solana rail onto the merchant's
// payment setup; nil when Solana is not armed for the merchant.
func solanaPaymentConfig(r *httprequest.Request) (*billing.SolanaPaymentConfig, error) {
	solanaConf, err := effectiveSolanaRailConfig(r)
	if err != nil || solanaConf == nil {
		return nil, err
	}
	network := normalizeSolanaNetwork(solanaConf.Network)
	return &billing.SolanaPaymentConfig{Network: network, Chain: "solana:" + network,
		PreferredToken: solanatokens.PreferredStablecoin, Tokens: acceptedSolanaTokens(r, solanaConf)}, nil
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
