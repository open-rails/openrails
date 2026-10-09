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

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/railresolve"
	billingservice "github.com/open-rails/openrails/internal/service"
	log "github.com/sirupsen/logrus"
)

// publicConfigMaxAge is how long a browser may reuse GET /v1/config. The
// document changes when a deployment changes or an operator arms a PSP; its
// ETag makes a revalidation a 304.
const publicConfigMaxAge = "public, max-age=300"

// GetPublicConfig serves GET /v1/config, unauthenticated: what the mount
// serves, the currency registry and, when the request resolves a merchant
// (the Host/api_host mechanism every public route uses), its browser payment
// setup.
//
// Every value is public by nature (an NMI Collect.js key, a Basis Theory
// public application key). That is enforced structurally by
// merchants.publicRailProfiles, a whitelist of settings keys: merchant
// SECRETS live in the secret store, which this path never opens, and a
// settings key that is not on the whitelist cannot reach the response at all.
func GetPublicConfig(capabilities billing.Capabilities) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		doc := billing.PublicConfig{Capabilities: capabilities, Currencies: billing.Currencies()}
		if mid, ok := merchant.FromContext(r.Request.Context()); ok && !mid.IsZero() && r.State != nil && r.State.Merchants != nil {
			payment, ok := loadPaymentConfig(r, mid)
			if !ok {
				return
			}
			doc.Payment = &payment
		}
		// A document listing a temporarily unavailable PSP is never cached:
		// the next request may find it available.
		if doc.Payment != nil && degraded(*doc.Payment) {
			r.SetHeader("Cache-Control", "no-store")
			r.SuccessJSON(doc)
			return
		}
		encoded, err := json.Marshal(doc)
		if err != nil {
			r.ErrorCode(billing.CodeInternalError, "failed to encode the configuration")
			return
		}
		sum := sha256.Sum256(encoded)
		etag := `"` + hex.EncodeToString(sum[:]) + `"`
		r.SetHeader("Cache-Control", publicConfigMaxAge)
		r.SetHeader("ETag", etag)
		if r.Header("If-None-Match") == etag {
			r.Status(http.StatusNotModified)
			return
		}
		r.SuccessJSON(doc)
	}
}

// ServiceGetPublicConfig serves the same document to the merchant, without
// public cache headers.
func ServiceGetPublicConfig(capabilities billing.Capabilities) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		payment, ok := merchantPaymentConfig(r)
		if !ok {
			return
		}
		r.SuccessJSON(billing.PublicConfig{Capabilities: capabilities, Currencies: billing.Currencies(), Payment: &payment})
	}
}

// ListCheckoutOptions serves GET /v1/admin/checkout-options: the ways
// checkout can sell one price, each with the browser driver that renders it.
func ListCheckoutOptions(r *httprequest.Request) {
	rawID, key := strings.TrimSpace(r.Query("price_id")), r.Query("price_key")
	productKey := r.Query("product_key")
	hasKey := strings.TrimSpace(key) != ""
	switch {
	case hasKey != (strings.TrimSpace(productKey) != ""):
		r.ErrorCode(billing.CodeInvalidParam, "product_key and price_key must be supplied together")
		return
	case rawID != "" && hasKey:
		r.ErrorCode(billing.CodeInvalidParam, "price_id and price_key are exclusive")
		return
	case rawID == "" && !hasKey:
		r.ErrorCode(billing.CodeInvalidParam, "price_id or product_key and price_key is required")
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
	payment, ok := merchantPaymentConfig(r)
	if !ok {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	options, err := svc.ListCheckoutOptions(r.Request.Context(), priceID, productKey, key)
	if err != nil {
		writeCheckoutAttemptError(r, err)
		return
	}
	advertiseCheckoutOptions(options, payment)
	r.JSON(http.StatusOK, billing.ListPage[billing.CheckoutOption]{Items: options})
}

// merchantPaymentConfig is the payment setup of the merchant the request
// acts on; it answers the error when there is none.
func merchantPaymentConfig(r *httprequest.Request) (billing.PaymentConfig, bool) {
	mid, ok := merchant.FromContext(r.Request.Context())
	if !ok || mid.IsZero() {
		r.ErrorCode(billing.CodeResourceNotFound, "no merchant for this host")
		return billing.PaymentConfig{}, false
	}
	if r.State == nil || r.State.Merchants == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "merchant configuration unavailable")
		return billing.PaymentConfig{}, false
	}
	return loadPaymentConfig(r, mid)
}

// loadPaymentConfig projects the merchant's armed PSPs and Solana acceptance
// onto the browser-safe payment setup.
func loadPaymentConfig(r *httprequest.Request, mid billing.MerchantID) (billing.PaymentConfig, bool) {
	env := config.ExpectedProviderEnvironment(r.State.Config != nil && config.IsTestMode(r.State.Config))
	psps, err := r.State.Merchants.PublicPSPs(r.Request.Context(), mid, env, pspArmed(r.State.RailConfigs))
	if err != nil {
		log.WithContext(r.Request.Context()).WithError(err).WithField("merchant_id", mid.String()).Error("payment config: PSPs could not be loaded")
		r.ErrorCode(billing.CodeInternalError, "failed to load payment configuration")
		return billing.PaymentConfig{}, false
	}
	solana, err := solanaPaymentConfig(r)
	if err != nil {
		log.WithContext(r.Request.Context()).WithError(err).WithField("merchant_id", mid.String()).Error("payment config: Solana acceptance could not be loaded")
		r.ErrorCode(billing.CodeInternalError, "failed to load solana payment configuration")
		return billing.PaymentConfig{}, false
	}
	return billing.PaymentConfig{PSPs: psps, Solana: solana}, true
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
// /v1/config serves (#1078).
func advertiseCheckoutOptions(options []billing.CheckoutOption, cfg billing.PaymentConfig) {
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
func solanaOptionToken(cfg *billing.SolanaPaymentConfig, bound string) (billing.SolanaPaymentToken, bool) {
	if cfg == nil {
		return billing.SolanaPaymentToken{}, false
	}
	find := func(symbol string) (billing.SolanaPaymentToken, bool) {
		for _, token := range cfg.Tokens {
			if strings.EqualFold(token.Symbol, symbol) {
				return token, true
			}
		}
		return billing.SolanaPaymentToken{}, false
	}
	if bound = strings.ToUpper(strings.TrimSpace(bound)); bound != "" {
		if token, ok := find(bound); ok {
			return token, true
		}
		return billing.SolanaPaymentToken{Symbol: bound}, true
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
	return billing.SolanaPaymentToken{}, false
}

// solanaClusterName is the Solana cluster name browsers expect.
func solanaClusterName(network string) string {
	if network == "mainnet" {
		return "mainnet-beta"
	}
	return network
}

func degraded(cfg billing.PaymentConfig) bool {
	for _, psp := range cfg.PSPs {
		if psp.Status != "" {
			return true
		}
	}
	return false
}
