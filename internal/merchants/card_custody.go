package merchants

import (
	"context"
	"errors"
	"fmt"
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
	// ErrPlatformVaultUnavailable refuses platform card custody where the
	// deployment has no platform vault, or the merchant's identity in it names
	// another tenant.
	ErrPlatformVaultUnavailable = apperr.New(http.StatusConflict, "platform_vault_unavailable",
		"this deployment has no platform vault for its posture; keep card_custody merchant").WithParam("settings.card_custody")
	// ErrPlatformVaultUnreachable refuses platform card custody beside a card
	// PSP that cannot charge the platform vault's cards.
	ErrPlatformVaultUnreachable = apperr.New(http.StatusConflict, "platform_vault_unreachable",
		"a card PSP checkout offers cannot charge cards from the platform vault")
)

// PlatformVaultAvailable reports whether platform card custody can be chosen.
func (s *Service) PlatformVaultAvailable() bool { return s != nil && s.platformVault != nil }

// CardCustodyRefusal is why the deployment cannot serve settings' card
// custody beside psps; nil when it can.
func (s *Service) CardCustodyRefusal(settings billing.MerchantSettings, psps map[string]merchantdocs.Doc[merchantdocs.PSP]) error {
	return CardCustodyRefusal(s.platformVault, s.providerEnvironment, settings, psps)
}

// CardCustodyRefusal is why a deployment with vault (nil: none) in
// environment cannot serve settings' card custody beside psps; nil when it
// can.
func CardCustodyRefusal(vault *config.PlatformVaultConfig, environment string, settings billing.MerchantSettings, psps map[string]merchantdocs.Doc[merchantdocs.PSP]) error {
	if merchantconfig.CardCustody(settings) != billing.CardCustodyPlatform {
		return nil
	}
	if vault == nil {
		return ErrPlatformVaultUnavailable
	}
	if keys := platformUnreachable(vault, environment, psps); len(keys) > 0 {
		return fmt.Errorf("psps %s: %w", strings.Join(keys, ", "), ErrPlatformVaultUnreachable)
	}
	return nil
}

// CheckCardCustody is CardCustodyRefusal, plus the merchant's identity in the
// platform vault, which must name this deployment's tenant.
func (s *Service) CheckCardCustody(ctx context.Context, id billing.MerchantID, settings billing.MerchantSettings, psps map[string]merchantdocs.Doc[merchantdocs.PSP]) error {
	if err := s.CardCustodyRefusal(settings, psps); err != nil || merchantconfig.CardCustody(settings) != billing.CardCustodyPlatform {
		return err
	}
	return s.database.RunInMerchantConn(merchant.WithID(ctx, id), func(ctx context.Context) error {
		return s.platformIdentityRefusal(ctx, s.database.Gen(ctx), id)
	})
}

// refusePSPForCardCustody refuses a PSP document that cannot serve the
// merchant's platform card custody.
func (s *Service) refusePSPForCardCustody(settings billing.MerchantSettings, key string, doc merchantdocs.PSP) error {
	if merchantconfig.CardCustody(settings) != billing.CardCustodyPlatform {
		return nil
	}
	if keys := platformUnreachable(s.platformVault, s.providerEnvironment, map[string]merchantdocs.Doc[merchantdocs.PSP]{key: {Value: doc}}); len(keys) > 0 {
		return fmt.Errorf("psp %s: %w", key, ErrPlatformVaultUnreachable)
	}
	return nil
}

// platformUnreachable are the live card PSPs, in key order, whose cards the
// platform vault cannot reach: its proxy cannot present a card to their rail,
// or their own server takes the card.
func platformUnreachable(vault *config.PlatformVaultConfig, environment string, psps map[string]merchantdocs.Doc[merchantdocs.PSP]) []string {
	var proxy custodians.Descriptor
	if vault != nil {
		proxy, _ = custodians.Get(vault.Kind)
	}
	var out []string
	for key, doc := range psps {
		p := doc.Value
		d, ok := rails.Lookup(models.Rail(p.Rail))
		if p.Archived || p.Environment != environment || !ok || !d.SupportsChargeSavedMethod {
			continue
		}
		entry, err := config.CardEntry(p.Rail, p.Settings, false)
		if !proxy.SupportsRail(d.Rail) || err != nil || entry == config.CardEntryServer {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}

// platformIdentityRefusal refuses when the merchant's platform key names an
// identity other than this deployment's vault partition.
func (s *Service) platformIdentityRefusal(ctx context.Context, q *gen.Queries, id billing.MerchantID) error {
	row, err := q.GetCustodianByKey(ctx, gen.GetCustodianByKeyParams{MerchantID: id.UUID(), Key: config.PlatformCustodianKey})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if row.Kind != models.CustodianPlatform || row.Environment != s.providerEnvironment || row.AccountID != config.PlatformCustodianAccountID(s.platformVault, id.UUID()) {
		return fmt.Errorf("the merchant's platform identity is %s; its cards move by custody migration: %w", row.AccountID, ErrPlatformVaultUnavailable)
	}
	return nil
}

// syncCardCustody serves platform card custody only where the deployment can:
// a merchant document newly choosing it is refused otherwise, else the PSP
// documents that cannot serve it are. A served choice records the merchant's
// identity in the platform vault.
func (s *Service) syncCardCustody(ctx context.Context, q *gen.Queries, id billing.MerchantID, previous merchantdocs.Set, set merchantdocs.Set, reject func(path, why string)) error {
	doc := set.Merchant.Value
	if !set.HasMerchant || merchantconfig.CardCustody(doc.Settings) != billing.CardCustodyPlatform {
		return nil
	}
	if _, err := merchantconfig.Normalize(doc.DisplayName, doc.Settings); err != nil {
		return nil // refused for its settings
	}
	changed := !previous.HasMerchant || !reflect.DeepEqual(previous.Merchant.Value, doc)
	if err := s.CardCustodyRefusal(doc.Settings, set.PSPs); err != nil {
		if changed || !errors.Is(err, ErrPlatformVaultUnreachable) {
			reject(merchantdocs.MerchantDoc, err.Error())
			return nil
		}
		for _, key := range platformUnreachable(s.platformVault, s.providerEnvironment, set.PSPs) {
			reject(merchantdocs.PSPDoc(key), fmt.Sprintf("psp %s: %s", key, ErrPlatformVaultUnreachable.Message))
		}
	}
	if err := s.platformIdentityRefusal(ctx, q, id); errors.Is(err, ErrPlatformVaultUnavailable) {
		if changed {
			reject(merchantdocs.MerchantDoc, err.Error())
		}
		return nil
	} else if err != nil {
		return err
	}
	_, err := q.RegisterCustodian(ctx, gen.RegisterCustodianParams{
		MerchantID: id.UUID(), Key: config.PlatformCustodianKey, Kind: models.CustodianPlatform,
		Environment: s.providerEnvironment, AccountID: config.PlatformCustodianAccountID(s.platformVault, id.UUID()),
	})
	return err
}
