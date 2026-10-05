package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/railresolve"
	billingservice "github.com/open-rails/openrails/internal/service"
	log "github.com/sirupsen/logrus"
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
		r.ErrorCode(billing.CodeInternalError, "failed to encode checkout configuration")
		return
	}

	// max-age carries the caching: a browser rendering checkout repeatedly
	// reuses the document with no round trip at all. The ETag is content-derived
	// so a shared cache can still revalidate; the neutral request transport has
	// no bodyless-response primitive, so this handler does not itself answer 304
	// (a revalidation gets a normal 200 with the same ETag, which is correct).
	// A document listing a temporarily unavailable PSP is never cached: the
	// next request may find it available.
	if degraded(body) {
		r.SetHeader("Cache-Control", "no-store")
		r.SuccessJSON(body)
		return
	}
	sum := sha256.Sum256(encoded)
	r.SetHeader("Cache-Control", checkoutConfigMaxAge)
	r.SetHeader("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	r.SuccessJSON(body)
}

// ServiceGetCheckoutConfig serves the same document to the merchant, without
// public cache headers. With price_id or price_key it also lists the ways
// checkout can sell that price, each with the browser driver that renders it.
func ServiceGetCheckoutConfig(r *httprequest.Request) {
	body, ok := checkoutConfig(r)
	if !ok {
		return
	}
	rawID, key := strings.TrimSpace(r.Query("price_id")), r.Query("price_key")
	if rawID == "" && strings.TrimSpace(key) == "" {
		r.SuccessJSON(body)
		return
	}
	if rawID != "" && strings.TrimSpace(key) != "" {
		r.ErrorCode(billing.CodeInvalidParam, "price_id and price_key are exclusive")
		return
	}
	var priceID billing.PriceID
	if rawID != "" {
		parsed, err := billing.ParsePriceID(rawID)
		if err != nil || parsed.IsZero() {
			r.ErrorCode(billing.CodeInvalidParam, "invalid price_id")
			return
		}
		priceID = parsed
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	options, err := svc.ListCheckoutOptions(r.Request.Context(), priceID, key)
	if err != nil {
		writeCheckoutAttemptError(r, err, checkoutAttemptErrorContext{})
		return
	}
	advertiseCheckoutOptions(options, body)
	body.Options = options
	r.SuccessJSON(body)
}

func checkoutConfig(r *httprequest.Request) (merchants.PublicCheckoutConfig, bool) {
	mid, ok := merchant.FromContext(r.Request.Context())
	if !ok || mid.IsZero() {
		r.ErrorCode(billing.CodeResourceNotFound, "no merchant for this host")
		return merchants.PublicCheckoutConfig{}, false
	}
	if r.State == nil || r.State.Merchants == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "merchant configuration unavailable")
		return merchants.PublicCheckoutConfig{}, false
	}
	env := config.ExpectedProviderEnvironment(r.State.Config != nil && config.IsTestMode(r.State.Config))
	psps, err := r.State.Merchants.PublicCheckoutPSPs(r.Request.Context(), mid, env, pspArmed(r.State.RailConfigs))
	if err != nil {
		log.WithContext(r.Request.Context()).WithError(err).WithField("merchant_id", mid.String()).Error("checkout config: PSPs could not be loaded")
		r.ErrorCode(billing.CodeInternalError, "failed to load checkout configuration")
		return merchants.PublicCheckoutConfig{}, false
	}
	solana, err := solanaCheckoutConfig(r)
	if err != nil {
		log.WithContext(r.Request.Context()).WithError(err).WithField("merchant_id", mid.String()).Error("checkout config: Solana acceptance could not be loaded")
		r.ErrorCode(billing.CodeInternalError, "failed to load solana checkout configuration")
		return merchants.PublicCheckoutConfig{}, false
	}
	return merchants.PublicCheckoutConfig{PSPs: psps, Solana: solana}, true
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
// /checkout-config serves (#1078).
func advertiseCheckoutOptions(options []billing.CheckoutOption, cfg merchants.PublicCheckoutConfig) {
	byID := make(map[billing.PSPID]merchants.PublicPSPConfig, len(cfg.PSPs))
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
		if psp.Status != "" {
			option.Status, option.RetryAfter = psp.Status, psp.RetryAfter
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
func solanaOptionToken(cfg *billing.SolanaCheckoutConfig, bound string) (billing.SolanaCheckoutToken, bool) {
	if cfg == nil {
		return billing.SolanaCheckoutToken{}, false
	}
	find := func(symbol string) (billing.SolanaCheckoutToken, bool) {
		for _, token := range cfg.Tokens {
			if strings.EqualFold(token.Symbol, symbol) {
				return token, true
			}
		}
		return billing.SolanaCheckoutToken{}, false
	}
	if bound = strings.ToUpper(strings.TrimSpace(bound)); bound != "" {
		if token, ok := find(bound); ok {
			return token, true
		}
		return billing.SolanaCheckoutToken{Symbol: bound}, true
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
	return billing.SolanaCheckoutToken{}, false
}

// solanaClusterName is the Solana cluster name browsers expect.
func solanaClusterName(network string) string {
	if network == "mainnet" {
		return "mainnet-beta"
	}
	return network
}

func degraded(cfg merchants.PublicCheckoutConfig) bool {
	for _, psp := range cfg.PSPs {
		if psp.Status != "" {
			return true
		}
	}
	return false
}
