package merchants

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
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
// reach a browser) and vaulted_card's carry gateway_account. Both are in the
// same map as the public keys below.
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
	string(models.RailVaultedCard): {
		Flow: FlowTokenize,
		Settings: []publicSetting{
			// The Basis Theory PUBLIC application key (#795). The private
			// application key is secrets.api_key.
			{Setting: "public_api_key", Field: "public_api_key", Required: true},
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

// PublicPSPConfig is the browser-facing description of one ARMED PSP: the
// value checkout's payment.rail selector takes, the rail it runs on, how a
// frontend must drive it, and the public-by-nature values it needs to do so.
type PublicPSPConfig struct {
	// Key is the checkout wire selector (psps.key, e.g. "mobius"). Falls back
	// to the rail kind for accounts declared without a key — the same value
	// checkout's resolveRailTarget accepts for a single-account rail.
	Key string `json:"key"`
	// Rail is the gateway kind: nmi, ccbill, stripe, solana, vaulted_card.
	Rail string `json:"rail"`
	// DisplayName is the buyer-facing rail name ("Credit Card", "Stripe").
	DisplayName string `json:"display_name"`
	// Flow is how a browser drives this rail: tokenize | redirect | wallet.
	Flow string `json:"flow"`
	// Config holds the whitelisted public values for this rail's flow. Empty
	// for redirect/wallet flows.
	Config map[string]string `json:"config,omitempty"`
}

// PublicCheckoutConfig is the response body of the public discovery endpoint.
type PublicCheckoutConfig struct {
	Object string            `json:"object"`
	PSPs   []PublicPSPConfig `json:"psps"`
}

// PublicPSPConfigFor projects an armed PSP onto its public browser config.
// ok=false means the PSP must not be advertised: an unknown rail, or a
// required public value the operator never declared. The reason is returned so
// the caller can log a misconfiguration loudly instead of serving a PSP a
// frontend cannot drive.
func PublicPSPConfigFor(scope PSPScope) (PublicPSPConfig, string, bool) {
	rail := strings.ToLower(strings.TrimSpace(scope.Rail))
	profile, known := publicRailProfiles[rail]
	if !known {
		return PublicPSPConfig{}, "rail has no browser checkout profile", false
	}

	key := strings.ToLower(strings.TrimSpace(scope.Key))
	if key == "" {
		key = rail
	}

	out := PublicPSPConfig{
		Key:         key,
		Rail:        rail,
		DisplayName: rails.DisplayName(models.Rail(rail)),
		Flow:        profile.Flow,
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
// (the same rows resolveRailTarget resolves through). A rail with no such row
// is absent from the result, so it is never advertised as available.
func (s *Service) PublicCheckoutPSPs(ctx context.Context, id merchant.ID, environment string) ([]PublicPSPConfig, error) {
	scopes, err := s.activePSPScopes(ctx, id, environment)
	if err != nil {
		return nil, err
	}
	out := make([]PublicPSPConfig, 0, len(scopes))
	for _, scope := range scopes {
		cfg, reason, ok := PublicPSPConfigFor(scope)
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

// activePSPScopes lists every non-archived PSP for merchant+environment in ONE
// query — the whole-catalog sibling of ActivePSPScopesForRail, so the public
// endpoint costs one round trip rather than one per known rail.
func (s *Service) activePSPScopes(ctx context.Context, id merchant.ID, environment string) ([]PSPScope, error) {
	if s == nil || s.pool == nil || id.IsZero() {
		return nil, nil
	}
	environment = normalizeProviderSecretEnvironment(environment)
	if environment == "" {
		return nil, fmt.Errorf("provider account environment must be live or test")
	}
	var out []PSPScope
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
				SELECT id, rail, environment, account_id, COALESCE(key, ''), evidence
				  FROM openrails.psps
				 WHERE merchant_id = $1::uuid
				   AND environment = $2
				   AND archived = false
				 ORDER BY rail ASC, created_at DESC, id DESC
			`, id.String(), environment)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = out[:0]
		for rows.Next() {
			var scope pspSecretScope
			var evidence []byte
			if err := rows.Scan(&scope.id, &scope.rail, &scope.environment, &scope.accountID, &scope.key, &evidence); err != nil {
				return err
			}
			scope.settings = pspSettings(evidence)
			out = append(out, scope.exported())
		}
		return rows.Err()
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list provider accounts for %s: %w", environment, err)
	}
	return out, nil
}
