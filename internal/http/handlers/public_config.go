package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/config"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/railresolve"
	log "github.com/sirupsen/logrus"
)

// publicConfigMaxAge is how long a browser may reuse GET /v1/config. The
// document changes when the deployment does; its ETag makes a revalidation
// a 304.
const publicConfigMaxAge = "public, max-age=300"

// GetPublicConfig serves GET /v1/config, without a credential: what the
// mount serves, the currency and rail registries, the captcha and the
// customer network's sign-in. It names no merchant, so every caller reads
// the same document and a cache may share it.
func GetPublicConfig(capabilities billing.Capabilities) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		doc := PublicConfigDocument(r.State, capabilities)
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

// GetCustomerConfig serves GET /v1/me/config: the payment setup and the
// public name of the merchant the customer's credential is bound to.
//
// Every value is public by nature (an NMI Collect.js key, a Basis Theory
// public application key). That is enforced structurally by
// merchants.publicRailProfiles, a whitelist of settings keys: merchant
// SECRETS live in the secret store, which this path never opens, and a
// settings key that is not on the whitelist cannot reach the response at all.
func GetCustomerConfig(r *httprequest.Request) {
	mid, ok := merchant.FromContext(r.Request.Context())
	if !ok || r.State == nil || r.State.Merchants == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "merchant configuration is unavailable")
		return
	}
	payment, ok := loadPaymentConfig(r, mid)
	if !ok {
		return
	}
	who, ok := loadPublicMerchant(r, mid)
	if !ok {
		return
	}
	// A document listing a temporarily unavailable PSP is never stored: the
	// next request may find it available.
	cache := "private"
	if degraded(payment) {
		cache = "no-store"
	}
	r.SetHeader("Cache-Control", cache)
	r.SuccessJSON(billing.CustomerConfig{Payment: payment, Merchant: who, ClientCountry: r.ClientCountry()})
}

// PublicConfigDocument is GET /v1/config on a mount reporting capabilities:
// the configuration every merchant of the deployment shares.
func PublicConfigDocument(rt *app.Runtime, capabilities billing.Capabilities) billing.PublicConfig {
	doc := billing.PublicConfig{Capabilities: capabilities, Currencies: billing.Currencies(), Rails: merchants.RailDefinitions()}
	if rt != nil {
	}
	if rt != nil && rt.Config != nil && config.CaptchaEnabled(rt.Config.Captcha) {
		cfg := rt.Config.Captcha
		doc.Captcha = &billing.CaptchaConfig{
			Provider: config.CaptchaProvider(cfg), SiteKey: strings.TrimSpace(cfg.SiteKey), ScriptURL: config.CaptchaScriptURL(cfg),
			Action: config.CaptchaAction, TokenHeader: captcha.TokenHeader,
		}
	}
	return doc
}

// loadPaymentConfig projects the merchant's armed PSPs and Solana acceptance
// onto the browser-safe payment setup.
func loadPaymentConfig(r *httprequest.Request, mid billing.MerchantID) (billing.PaymentConfig, bool) {
	psps, err := r.State.Merchants.PublicPSPs(r.Request.Context(), mid, pspArmed(r.State.RailConfigs))
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

// loadPublicMerchant is the merchant's display name, and its logo and
// support page when they are https URLs.
func loadPublicMerchant(r *httprequest.Request, mid billing.MerchantID) (billing.PublicMerchant, bool) {
	name, settings, err := r.State.Merchants.MerchantSettings(r.Request.Context(), mid)
	if err != nil {
		log.WithContext(r.Request.Context()).WithError(err).WithField("merchant_id", mid.String()).Error("customer config: merchant settings could not be loaded")
		r.ErrorCode(billing.CodeInternalError, "failed to load the merchant")
		return billing.PublicMerchant{}, false
	}
	who := billing.PublicMerchant{DisplayName: name, EmbedOrigins: merchantconfig.EmbedOrigins(settings)}
	if p := settings.Profile; p != nil {
		who.LogoURL, who.SupportURL = httpsURL(p.LogoURL), httpsURL(p.SupportURL)
		who.TermsURL, who.PrivacyURL = httpsURL(p.TermsURL), httpsURL(p.PrivacyURL)
		if d := strings.TrimSpace(p.StatementDescriptor); d != "" {
			who.StatementDescriptor = &d
		}
	}
	return who, true
}

func httpsURL(raw string) *string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil
	}
	out := u.String()
	return &out
}

// pspArmed reports whether a declared account resolves with its full credential
// shape — the same question PSP routing asks before charging. Resolution
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

func degraded(cfg billing.PaymentConfig) bool {
	for _, psp := range cfg.PSPs {
		if psp.Status != "" {
			return true
		}
	}
	return false
}
