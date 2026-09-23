package merchants

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/models"
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
}

// railPublicProfile is a rail's browser-facing contract.
type railPublicProfile struct {
	Flow     string
	Settings []publicSetting
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
			{Setting: "tokenization_url", Field: "tokenization_url", Default: DefaultNMICollectJSURL},
		},
	},
	// Hosted-redirect rails: OpenRails builds the URL server-side, so the
	// browser needs no key at all.
	string(models.RailStripe): {Flow: FlowRedirect},
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
		if value == "" {
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
		return nil, err
	}
	byID := make(map[uuid.UUID]CustodianScope, len(declared))
	for _, c := range declared {
		byID[c.ID] = c
	}
	out := make([]PublicPSPConfig, 0, len(scopes))
	for _, scope := range scopes {
		if armed != nil {
			ok, err := armed(ctx, scope)
			if err != nil {
				return nil, fmt.Errorf("resolve %s account %s: %w", scope.Rail, scope.AccountID, err)
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
		var custodian *CustodianScope
		if scope.CustodianID != nil {
			if c, ok := byID[*scope.CustodianID]; ok {
				custodian = &c
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
	return out, nil
}
