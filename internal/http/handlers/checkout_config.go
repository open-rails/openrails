package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"strings"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/merchant"
)

// checkoutConfigMaxAge is how long a browser may reuse this document. Arming a
// PSP is an operator action measured in minutes, and a stale answer only ever
// costs one failed checkout that a reload fixes — but an ETag makes the common
// case a 304 anyway, so the window stays short.
const checkoutConfigMaxAge = "public, max-age=60"

// GetCheckoutConfig serves the PUBLIC per-merchant checkout discovery document
// (#829): the merchant's armed PSPs, and for each the public-by-nature values a
// browser needs to tokenize on it.
//
// Unauthenticated by design — every value it can serve is already public (an
// NMI Collect.js key, a Basis Theory public application key). That is enforced
// structurally by merchants.publicRailProfiles, a whitelist of settings keys:
// merchant SECRETS live in the secret store, which this path never opens, and a
// settings key that is not on the whitelist cannot reach the response at all.
//
// The merchant is whichever one the request resolved to — the Host/api_host
// mechanism (#734) that already governs every public route, applied by the
// server's middleware chain before the handler runs.
func GetCheckoutConfig(r *httprequest.Request) {
	body, ok := checkoutConfig(r)
	if !ok {
		return
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to encode checkout configuration")
		return
	}

	// max-age carries the caching: a browser rendering checkout repeatedly
	// reuses the document with no round trip at all. The ETag is content-derived
	// so a shared cache can still revalidate; the neutral request transport has
	// no bodyless-response primitive, so this handler does not itself answer 304
	// (a revalidation gets a normal 200 with the same ETag, which is correct).
	sum := sha256.Sum256(encoded)
	r.SetHeader("Cache-Control", checkoutConfigMaxAge)
	r.SetHeader("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	r.SuccessJSON(body)
}

// ServiceGetCheckoutConfig serves the same document to the shared client for
// the credential's merchant, without public cache headers.
func ServiceGetCheckoutConfig(r *httprequest.Request) {
	if body, ok := checkoutConfig(r); ok {
		r.SuccessJSON(body)
	}
}

func checkoutConfig(r *httprequest.Request) (merchants.PublicCheckoutConfig, bool) {
	mid, ok := merchant.FromContext(r.Request.Context())
	if !ok || mid.IsZero() {
		r.ErrorJSON(http.StatusNotFound, "no merchant for this host")
		return merchants.PublicCheckoutConfig{}, false
	}
	if r.State == nil || r.State.Merchants == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "merchant configuration unavailable")
		return merchants.PublicCheckoutConfig{}, false
	}
	env := config.ExpectedProviderEnvironment(r.State.Config != nil && r.State.Config.IsTestMode())
	psps, err := r.State.Merchants.PublicCheckoutPSPs(r.Request.Context(), mid, env, pspArmed(r.State.RailConfigs))
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to load checkout configuration")
		return merchants.PublicCheckoutConfig{}, false
	}
	solana, err := solanaCheckoutConfig(r)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to load solana checkout configuration")
		return merchants.PublicCheckoutConfig{}, false
	}
	return merchants.PublicCheckoutConfig{Object: "checkout_config", PSPs: psps, Solana: solana}, true
}

// pspArmed reports whether a declared account resolves with its full credential
// shape — the same question checkout routing asks before charging. Resolution
// errors fail closed.
func pspArmed(rails railresolve.Source) func(context.Context, merchants.PSPScope) (bool, error) {
	return func(ctx context.Context, scope merchants.PSPScope) (bool, error) {
		if rails == nil {
			return false, nil
		}
		if _, err := rails.RailConfig(ctx, scope.Rail, scope.AccountID); err != nil {
			if errors.Is(err, railresolve.ErrRailNotArmed) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}
}

// advertiseCheckoutOptions attaches to each option the browser driver and
// public values that render it, from the same armed-PSP projection
// /checkout-config serves (#1078). Hosts pass options through unchanged.
func advertiseCheckoutOptions(options []openrails.CheckoutRailOption, cfg merchants.PublicCheckoutConfig) {
	byID := make(map[string]merchants.PublicPSPConfig, len(cfg.PSPs))
	for _, psp := range cfg.PSPs {
		byID[psp.PSPID] = psp
	}
	for i := range options {
		option := &options[i]
		bound := option.PublicConfig["token_symbol"]
		option.PublicConfig = nil
		psp, ok := byID[option.PSPID]
		if !ok {
			continue
		}
		public := maps.Clone(psp.Config)
		if psp.Flow == merchants.FlowWallet {
			token, ok := solanaOptionToken(cfg.Solana, bound)
			if !ok {
				continue
			}
			if public == nil {
				public = map[string]string{}
			}
			public["token_symbol"] = token.Symbol
			if token.Name != "" && token.Name != token.Symbol {
				public["token_name"] = token.Name
			}
			public["network"] = solanaClusterName(cfg.Solana.Network)
		}
		if option.Driver = merchants.CheckoutDriver(psp, option.Mode); option.Driver != "" {
			option.PublicConfig = public
		}
	}
}

// solanaOptionToken is the token a Solana option settles in: the one the price
// binds, else the merchant's preferred accepted token, else its first accepted
// stablecoin. A bound token the merchant does not list keeps its symbol.
func solanaOptionToken(cfg *openrails.SolanaCheckoutConfig, bound string) (openrails.SolanaCheckoutToken, bool) {
	if cfg == nil {
		return openrails.SolanaCheckoutToken{}, false
	}
	find := func(symbol string) (openrails.SolanaCheckoutToken, bool) {
		for _, token := range cfg.Tokens {
			if strings.EqualFold(token.Symbol, symbol) {
				return token, true
			}
		}
		return openrails.SolanaCheckoutToken{}, false
	}
	if bound = strings.ToUpper(strings.TrimSpace(bound)); bound != "" {
		if token, ok := find(bound); ok {
			return token, true
		}
		return openrails.SolanaCheckoutToken{Symbol: bound}, true
	}
	if token, ok := find(cfg.PreferredToken); ok {
		return token, true
	}
	for _, token := range cfg.Tokens {
		if token.RecurringEligible {
			return token, true
		}
	}
	if len(cfg.Tokens) > 0 {
		return cfg.Tokens[0], true
	}
	return openrails.SolanaCheckoutToken{}, false
}

// solanaClusterName is the Solana cluster name browsers expect.
func solanaClusterName(network string) string {
	if network == "mainnet" {
		return "mainnet-beta"
	}
	return network
}
