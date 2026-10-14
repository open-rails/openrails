package merchants

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

var (
	// ErrPlatformVaultUnavailable refuses card_vault platform where the
	// deployment has no platform vault, or the merchant's identity in it names
	// another tenant.
	ErrPlatformVaultUnavailable = apperr.New(http.StatusConflict, "platform_vault_unavailable",
		"this deployment has no platform vault for its posture; save new cards at a card PSP's vault").WithParam("settings.card_vault")
	// ErrPlatformVaultUnreachable refuses card_vault platform beside a charge
	// PSP that cannot charge the platform vault's cards.
	ErrPlatformVaultUnreachable = apperr.New(http.StatusConflict, "platform_vault_unreachable",
		"a charge PSP cannot charge cards from the platform vault").WithParam("settings.charge_psps")
	// ErrCardVaultUnknown refuses a card_vault naming no live card PSP.
	ErrCardVaultUnknown = apperr.New(http.StatusConflict, "card_vault_unknown",
		"card_vault names no live card PSP of the merchant, nor platform").WithParam("settings.card_vault")
	// ErrChargePSPUnknown refuses charge_psps naming no live card PSP.
	ErrChargePSPUnknown = apperr.New(http.StatusConflict, "charge_psp_unknown",
		"charge_psps names a PSP that is not a live card PSP of the merchant").WithParam("settings.charge_psps")
	// ErrChargePSPIncompatible refuses a charge PSP that cannot charge cards
	// from the card vault.
	ErrChargePSPIncompatible = apperr.New(http.StatusConflict, "charge_psp_incompatible",
		"a charge PSP cannot charge cards from the card vault").WithParam("settings.charge_psps")
)

// PlatformVaultAvailable reports whether card_vault platform can be chosen.
func (s *Service) PlatformVaultAvailable() bool { return s != nil && s.platformVault != nil }

// CardVaultRefusal is why the deployment cannot serve settings' card vault
// and charge PSPs beside psps; nil when it can.
func (s *Service) CardVaultRefusal(settings billing.MerchantSettings, psps map[string]merchantdocs.Doc[merchantdocs.PSP]) error {
	return CardVaultRefusal(s.platformVault, settings, psps)
}

// CardVaultRefusal is why a deployment with vault (nil: none) cannot serve
// settings' card vault and charge PSPs beside psps; nil when it can. Every
// charge PSP must charge cards from the vault.
func CardVaultRefusal(vault *config.PlatformVaultConfig, settings billing.MerchantSettings, psps map[string]merchantdocs.Doc[merchantdocs.PSP]) error {
	cards := liveCardPSPs(psps)
	for _, key := range settings.ChargePSPs {
		if _, ok := cards[key]; !ok {
			return fmt.Errorf("psp %s: %w", key, ErrChargePSPUnknown)
		}
	}
	charging := cards
	if len(settings.ChargePSPs) > 0 {
		charging = map[string]merchantdocs.PSP{}
		for _, key := range settings.ChargePSPs {
			charging[key] = cards[key]
		}
	}
	switch key := merchantconfig.CardVault(settings); key {
	case "":
		return nil
	case billing.CardVaultPlatform:
		if vault == nil {
			return ErrPlatformVaultUnavailable
		}
		if keys := platformUnreachable(vault, charging); len(keys) > 0 {
			return fmt.Errorf("psps %s: %w", strings.Join(keys, ", "), ErrPlatformVaultUnreachable)
		}
		return nil
	default:
		held, ok := cards[key]
		if !ok {
			return fmt.Errorf("psp %s: %w", key, ErrCardVaultUnknown)
		}
		var refused []string
		for _, charge := range settings.ChargePSPs {
			if !ChargesVaultedCards(held, cards[charge]) {
				refused = append(refused, charge)
			}
		}
		if len(refused) > 0 {
			return fmt.Errorf("psps %s cannot charge cards vaulted at %s: %w", strings.Join(refused, ", "), key, ErrChargePSPIncompatible)
		}
		return nil
	}
}

// ChargesVaultedCards reports whether psp can charge the cards vault holds. A
// custodian's cards go through any PSP naming that custodian. A card in a
// PSP's own vault charges only through that account: a Stripe customer
// belongs to its account, and NMI gateway-issued Customer Vault tokens are valid
// only within their merchant account. See NMI's Customer Vault FAQ:
// https://support.nmi.com/hc/en-gb/articles/33994528786705-Customer-Vault
func ChargesVaultedCards(vault, psp merchantdocs.PSP) bool {
	if vault.Custodian != "" {
		return psp.Custodian == vault.Custodian
	}
	return psp.Custodian == "" && psp.Rail == vault.Rail && psp.Environment == vault.Environment && psp.AccountID == vault.AccountID
}

// liveCardPSPs are the merchant's live PSPs whose rail charges saved cards.
func liveCardPSPs(psps map[string]merchantdocs.Doc[merchantdocs.PSP]) map[string]merchantdocs.PSP {
	out := map[string]merchantdocs.PSP{}
	for key, doc := range psps {
		d, ok := rails.Lookup(models.Rail(doc.Value.Rail))
		if !doc.Value.Archived && ok && d.SupportsChargeSavedMethod {
			out[key] = doc.Value
		}
	}
	return out
}

// CheckCardVault is CardVaultRefusal, plus the merchant's identity in the
// platform vault, which must name this deployment's tenant.
func (s *Service) CheckCardVault(ctx context.Context, id billing.MerchantID, doc merchantdocs.Merchant, psps map[string]merchantdocs.Doc[merchantdocs.PSP]) error {
	if err := s.CardVaultRefusal(doc.MerchantSettings, psps); err != nil || !merchantconfig.PlatformCardVault(doc.MerchantSettings) {
		return err
	}
	return s.database.RunInMerchantConn(merchant.WithID(ctx, id), func(ctx context.Context) error {
		return s.platformIdentityRefusal(ctx, s.database.Gen(ctx), id, merchantClass(doc.Sandbox))
	})
}

// refusePSPForCardVault refuses a new PSP document that would break the
// merchant's card vault: one it makes a charge PSP that cannot charge it.
func (s *Service) refusePSPForCardVault(settings billing.MerchantSettings, psps map[string]merchantdocs.Doc[merchantdocs.PSP], key string, doc merchantdocs.PSP) error {
	if s.CardVaultRefusal(settings, psps) != nil {
		return nil
	}
	next := maps.Clone(psps)
	if next == nil {
		next = map[string]merchantdocs.Doc[merchantdocs.PSP]{}
	}
	next[key] = merchantdocs.Doc[merchantdocs.PSP]{Value: doc}
	return s.CardVaultRefusal(settings, next)
}

// platformUnreachable are the PSPs, in key order, whose cards the platform
// vault cannot reach: its proxy cannot present a card to their rail, or their
// own server takes the card.
func platformUnreachable(vault *config.PlatformVaultConfig, psps map[string]merchantdocs.PSP) []string {
	var proxy custodians.Descriptor
	if vault != nil {
		proxy, _ = custodians.Get(vault.Kind)
	}
	var out []string
	for key, p := range psps {
		d, _ := rails.Lookup(models.Rail(p.Rail))
		entry, err := config.CardEntry(p.Rail, p.Settings, false)
		if !proxy.SupportsRail(d.Rail) || err != nil || entry == config.CardEntryServer {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}

// platformIdentityRefusal refuses when the merchant's platform key names an
// identity other than this deployment's vault partition in class.
func (s *Service) platformIdentityRefusal(ctx context.Context, q *gen.Queries, id billing.MerchantID, class string) error {
	row, err := q.GetCustodianByKey(ctx, gen.GetCustodianByKeyParams{MerchantID: id.UUID(), Key: config.PlatformCustodianKey})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if row.Kind != models.CustodianPlatform || row.Environment != class || row.AccountID != config.PlatformCustodianAccountID(s.platformVault, id.UUID()) {
		return fmt.Errorf("the merchant's platform identity is %s; its cards move by custody migration: %w", row.AccountID, ErrPlatformVaultUnavailable)
	}
	return nil
}

// syncCardVault serves the merchant's card vault only where the deployment
// can: a merchant document newly choosing it is refused otherwise, else the
// PSP documents that break it are. A served platform vault records the
// merchant's identity in it.
func (s *Service) syncCardVault(ctx context.Context, q *gen.Queries, id billing.MerchantID, previous merchantdocs.Set, set merchantdocs.Set, reject func(path, why string)) error {
	doc := set.Merchant.Value
	if !set.HasMerchant || merchantconfig.CardVault(doc.MerchantSettings) == "" && len(doc.ChargePSPs) == 0 {
		return nil
	}
	if _, err := merchantconfig.Normalize(doc.DisplayName, doc.MerchantSettings); err != nil {
		return nil // refused for its settings
	}
	changed := !previous.HasMerchant || !reflect.DeepEqual(previous.Merchant.Value, doc)
	if err := s.CardVaultRefusal(doc.MerchantSettings, set.PSPs); err != nil {
		if changed || !errors.Is(err, ErrPlatformVaultUnreachable) {
			reject(merchantdocs.MerchantDoc, err.Error())
			return nil
		}
		for _, key := range platformUnreachable(s.platformVault, chargingPSPs(doc.MerchantSettings, set.PSPs)) {
			reject(merchantdocs.PSPDoc(key), fmt.Sprintf("psp %s: %s", key, ErrPlatformVaultUnreachable.Message))
		}
	}
	if !merchantconfig.PlatformCardVault(doc.MerchantSettings) {
		return nil
	}
	class := merchantClass(doc.Sandbox)
	if err := s.platformIdentityRefusal(ctx, q, id, class); errors.Is(err, ErrPlatformVaultUnavailable) {
		if changed {
			reject(merchantdocs.MerchantDoc, err.Error())
		}
		return nil
	} else if err != nil {
		return err
	}
	_, err := q.RegisterCustodian(ctx, gen.RegisterCustodianParams{
		MerchantID: id.UUID(), Key: config.PlatformCustodianKey, Kind: models.CustodianPlatform,
		Environment: class, AccountID: config.PlatformCustodianAccountID(s.platformVault, id.UUID()),
	})
	return err
}

// chargingPSPs are the live card PSPs new card charges go to.
func chargingPSPs(settings billing.MerchantSettings, psps map[string]merchantdocs.Doc[merchantdocs.PSP]) map[string]merchantdocs.PSP {
	cards := liveCardPSPs(psps)
	if len(settings.ChargePSPs) == 0 {
		return cards
	}
	out := map[string]merchantdocs.PSP{}
	for _, key := range settings.ChargePSPs {
		if p, ok := cards[key]; ok {
			out[key] = p
		}
	}
	return out
}
