package merchants

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/pkg/merchant"
	log "github.com/sirupsen/logrus"
)

// Browser checkout flows — how a frontend has to drive a rail. A frontend
// switches on this, not on a hardcoded list of rails.
const (
	// FlowTokenize: the browser tokenizes the card itself against the rail's
	// own public key, then posts the resulting token to /checkout.
	FlowTokenize = "tokenize"
	// FlowRedirect: OpenRails mints a hosted payment URL; the browser sends the
	// buyer there. No card ever touches the frontend, so no public key is needed.
	FlowRedirect = "redirect"
	// FlowWallet: the buyer's wallet signs. Chain/token detail comes from
	// /solana/config + /solana/tokens.
	FlowWallet = "wallet"
	// FlowElements: the browser saves the card in the PSP's own embedded
	// fields, then checkout charges that saved card; any authentication runs
	// in the page. No card data reaches the host.
	FlowElements = "elements"
)

// publicSetting is ONE psps.settings key that is public by nature and may
// therefore be served to an unauthenticated browser.
type publicSetting struct {
	// Setting is the key on the psps row's settings blob.
	Setting string
	// Field is the wire name in the response's config object.
	Field string
	// Required: a PSP missing this value cannot be driven from a browser, so
	// the whole PSP is omitted rather than advertised half-configured (#651 —
	// never invent a value, never advertise a broken one).
	Required bool
	// Default is a DECLARED constant used when the merchant did not override
	// it. Only legitimate where the value is a property of the rail, not of the
	// merchant (NMI's Collect.js URL). "" = no default.
	Default string
	// Allowed, when set, accepts a configured value; anything else is served
	// as Default. Script URLs a browser loads are never merchant-arbitrary.
	Allowed func(string) bool
}

// railPublicProfile is a rail's browser-facing contract.
type railPublicProfile struct {
	Flow     string
	Settings []publicSetting
	// EmbeddedFlow replaces Flow once the public setting EmbeddedSetting is
	// declared: the browser can then drive the PSP's own embedded fields.
	EmbeddedFlow    string
	EmbeddedSetting string
}

// publicRailProfiles is THE WHITELIST. It is the only path by which anything
// stored on a PSP reaches an unauthenticated response: the projection below
// never ranges over a settings map, it only ever asks for the keys named here.
// A settings key added anywhere else — or any merchant secret, which does not
// live in settings at all — is private by default and stays private.
//
// Two live examples of why this is a whitelist and not a denylist: solana's
// settings carry rpc_api_key (a paid Helius key that #352 says must never
// reach a browser) and a custody block carries the custodian's tenant id. Both
// are in the same map as the public keys below.
var publicRailProfiles = map[string]railPublicProfile{
	string(models.RailNMI): {
		Flow: FlowTokenize,
		Settings: []publicSetting{
			// The Collect.js browser key. Public by design — it can only mint
			// single-use tokens; charging needs the security_key, which is a
			// secret and lives in the secret store, not here.
			{Setting: "tokenization_key", Field: "tokenization_key", Required: true},
			{Setting: "tokenization_url", Field: "tokenization_url", Default: DefaultNMICollectJSURL, Allowed: NMICollectJSURLAllowed},
		},
	},
	// Stripe: embedded Elements once its publishable key (public by design) is
	// declared; hosted Checkout redirect without one.
	string(models.RailStripe): {Flow: FlowRedirect, EmbeddedFlow: FlowElements, EmbeddedSetting: "publishable_key", Settings: []publicSetting{
		{Setting: "publishable_key", Field: "publishable_key"},
	}},
	string(models.RailCCBill): {Flow: FlowRedirect},
	// Solana's browser config (network, chain, mints) is already served, fully
	// derived, by GET /solana/config; nothing on the PSP row is public.
	string(models.RailSolana): {Flow: FlowWallet},
}

// A third-party custodian OVERRIDES the rail profile (or#879/or#880). Custody
// changes the browser contract, not the gateway: the PAN goes browser ->
// custodian, so the page needs the CUSTODIAN's public key and never the rail's
// tokenizer key. Which values those are is registry data on the custodian kind
// (internal/custodians), not a second whitelist here — the Public flag on a
// setting slot is the whitelist.

// PublicPSPConfig and PublicCheckoutConfig are the shared client wire types.
type (
	PublicPSPConfig      = openrails.CheckoutPSPConfig
	PublicCheckoutConfig = openrails.CheckoutConfig
)

// PublicPSPConfigFor projects an armed PSP onto its public browser config.
// ok=false means the PSP must not be advertised: an unknown rail, or a
// required public value the operator never declared. The reason is returned so
// the caller can log a misconfiguration loudly instead of serving a PSP a
// frontend cannot drive.
func PublicPSPConfigFor(scope PSPScope, custodian *CustodianScope) (PublicPSPConfig, string, bool) {
	rail := strings.ToLower(strings.TrimSpace(scope.Rail))
	profile, known := publicRailProfiles[rail]
	if !known {
		return PublicPSPConfig{}, "rail has no browser checkout profile", false
	}
	// A PSP that still carries an inline custody block is misconfigured, not
	// custodial: refuse it rather than advertise a tokenizer key the browser
	// must not use (or#880 moved custody into its own declaration).
	if err := config.RejectRetiredCustodySettings(scope.Settings); err != nil {
		return PublicPSPConfig{}, err.Error(), false
	}

	key := strings.ToLower(strings.TrimSpace(scope.Key))
	if key == "" {
		key = rail
	}

	out := PublicPSPConfig{
		PSPID:       scope.ID.String(),
		Key:         key,
		Rail:        rail,
		Custodian:   models.CustodianPSP,
		DisplayName: rails.DisplayName(models.Rail(rail)),
		Flow:        profile.Flow,
	}

	if scope.CustodianID != nil {
		if custodian == nil {
			return PublicPSPConfig{}, "psp references a custodian that could not be resolved", false
		}
		if custodian.Archived {
			// Drain-only (or#655/or#870): the cards it already holds stay
			// chargeable, but a browser must not vault a NEW one into a
			// custodian the operator is draining.
			return PublicPSPConfig{}, "custodian " + custodian.Key + " is archived (drain-only)", false
		}
		d, err := custodians.Require(custodian.Kind)
		if err != nil {
			return PublicPSPConfig{}, err.Error(), false
		}
		if d.BrowserFlow == "" {
			return PublicPSPConfig{}, "custodian " + d.Kind + " has no browser checkout profile", false
		}
		public, reason, ok := d.PublicSettings(custodian.Settings)
		if !ok {
			return PublicPSPConfig{}, reason, false
		}
		out.Custodian = d.Kind
		out.Flow = d.BrowserFlow
		if len(public) > 0 {
			out.Config = public
		}
		return out, "", true
	}

	for _, want := range profile.Settings {
		value := publicSettingValue(scope.Settings, want.Setting)
		if value == "" || (want.Allowed != nil && !want.Allowed(value)) {
			value = want.Default
		}
		if value == "" {
			if want.Required {
				return PublicPSPConfig{}, "missing required public setting " + want.Setting, false
			}
			continue
		}
		if out.Config == nil {
			out.Config = make(map[string]string, len(profile.Settings))
		}
		out.Config[want.Field] = value
	}
	if profile.EmbeddedFlow != "" && publicSettingValue(scope.Settings, profile.EmbeddedSetting) != "" {
		out.Flow = profile.EmbeddedFlow
	}
	return out, "", true
}

// publicSettingValue reads ONE named key out of a settings blob. It is the only
// reader the public projection has — there is deliberately no "copy the rest".
func publicSettingValue(settings map[string]any, key string) string {
	if len(settings) == 0 {
		return ""
	}
	switch v := settings[key].(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

// PublicCheckoutPSPs lists the merchant's ARMED PSPs for environment with each
// one's public browser config. Armed means exactly what checkout means by it:
// a non-archived openrails.psps row for this merchant, rail and environment
// whose full credential shape resolves — armed reports that, with the same
// resolver checkout routes through. A PSP declared without credentials (an
// import attribution) is an identity, never advertised as available.
func (s *Service) PublicCheckoutPSPs(ctx context.Context, id merchant.ID, environment string, armed func(context.Context, PSPScope) (bool, error)) ([]PublicPSPConfig, error) {
	scopes, err := s.activePSPScopes(ctx, id, environment)
	if err != nil {
		return nil, err
	}
	// One extra round trip for the whole catalog, not one per custodial PSP:
	// several PSPs may reference the SAME custodian (that is the point of
	// or#880's registry).
	declared, err := s.ListCustodians(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("list custodians: %w", err)
	}
	byID := make(map[uuid.UUID]CustodianScope, len(declared))
	for _, c := range declared {
		byID[c.ID] = c
	}
	out := make([]PublicPSPConfig, 0, len(scopes))
	for _, scope := range scopes {
		var custodian *CustodianScope
		if scope.CustodianID != nil {
			if c, ok := byID[*scope.CustodianID]; ok {
				custodian = &c
			}
		}
		if armed != nil {
			ok, err := armed(ctx, scope)
			if err != nil && storeFailure(err) {
				return nil, fmt.Errorf("resolve %s account %s: %w", scope.Rail, scope.AccountID, err)
			}
			if err != nil {
				// One PSP's credential check failing never takes the others
				// down: it is listed as temporarily unavailable, without the
				// values a browser would drive it with.
				log.WithContext(ctx).WithError(err).WithFields(log.Fields{
					"merchant_id": id.String(),
					"rail":        scope.Rail,
					"psp":         scope.Key,
				}).Warn("public checkout config: PSP credentials could not be checked; listed as temporarily unavailable")
				cfg, _, _ := PublicPSPConfigFor(scope, custodian)
				if cfg.PSPID == "" {
					cfg.PSPID, cfg.Rail, cfg.Key = scope.ID.String(), strings.ToLower(scope.Rail), strings.ToLower(scope.Key)
				}
				cfg.Config = nil
				cfg.Status, cfg.RetryAfter = openrails.CheckoutPSPTemporarilyUnavailable, checkoutRetryAfterSeconds
				out = append(out, cfg)
				continue
			}
			if !ok {
				log.WithContext(ctx).WithFields(log.Fields{
					"merchant_id": id.String(),
					"rail":        scope.Rail,
					"psp":         scope.Key,
				}).Info("public checkout config: declared PSP is not armed")
				continue
			}
		}
		cfg, reason, ok := PublicPSPConfigFor(scope, custodian)
		if !ok {
			log.WithContext(ctx).WithFields(log.Fields{
				"merchant_id": id.String(),
				"rail":        scope.Rail,
				"psp":         scope.Key,
				"reason":      reason,
			}).Warn("public checkout config: armed PSP withheld from browsers")
			continue
		}
		out = append(out, cfg)
	}
	selectors, err := s.checkoutSelectors(ctx, id)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Checkout = selectors == nil || selectors[out[i].Key] || selectors[out[i].Rail]
	}
	return out, nil
}

// checkoutSelectors is every PSP key or rail the merchant's checkout routing
// can pick; nil means no policy, so every armed PSP takes new checkouts.
func (s *Service) checkoutSelectors(ctx context.Context, id merchant.ID) (map[string]bool, error) {
	if s.database == nil {
		return nil, nil
	}
	conf, found, err := merchantconfig.NewStore(s.database).Get(merchant.WithID(ctx, id))
	if err != nil {
		return nil, fmt.Errorf("load checkout routing policy: %w", err)
	}
	if !found || len(conf.CheckoutRouting) == 0 {
		return nil, nil
	}
	selectors := map[string]bool{}
	for _, rule := range conf.CheckoutRouting {
		for _, selector := range rule.Prefer {
			selectors[strings.ToLower(strings.TrimSpace(selector))] = true
		}
	}
	return selectors, nil
}

// Browser checkout drivers: how @openrails/billing-ui executes one option.
const (
	DriverCollectJS      = "collect_js"
	DriverStripeElements = "stripe_elements"
	DriverRedirect       = "redirect"
	DriverSolanaPay      = "solana_pay"
)

// CheckoutDriver is the browser driver that can sell through psp in mode, or
// "" when none can. An engine-collected subscription charges a saved method,
// so only an in-page card driver can enroll it; a redirect cannot.
func CheckoutDriver(psp PublicPSPConfig, mode string) string {
	if psp.Custodian != models.CustodianPSP {
		return ""
	}
	driver := ""
	switch psp.Flow {
	case FlowTokenize:
		if key := psp.Config["tokenization_key"]; key != "" && !strings.HasPrefix(key, "preview_") && psp.Config["tokenization_url"] != "" {
			driver = DriverCollectJS
		}
	case FlowElements:
		if strings.HasPrefix(psp.Config["publishable_key"], "pk_") {
			driver = DriverStripeElements
		}
	case FlowRedirect:
		driver = DriverRedirect
	case FlowWallet:
		driver = DriverSolanaPay
	}
	if mode == string(models.CheckoutSessionModeSubscription) &&
		rails.NewSubscriptionFor(models.Rail(psp.Rail)) == rails.NewSubscriptionEngine &&
		driver != DriverCollectJS && driver != DriverStripeElements {
		return ""
	}
	return driver
}

// checkoutRetryAfterSeconds is when a browser should ask again for a PSP
// whose credentials could not be checked.
const checkoutRetryAfterSeconds = 30

// storeFailure is an error of OpenRails' own database or of the request
// itself: the document cannot be built at all, so it stays a 5xx.
func storeFailure(err error) bool {
	var pgErr *pgconn.PgError
	var connectErr *pgconn.ConnectError
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.As(err, &pgErr) || errors.As(err, &connectErr) || pgconn.Timeout(err)
}
