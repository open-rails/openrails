package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/open-rails/openrails/billing"
	catalogwire "github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/currency"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// ErrCurrencyUnsupported refuses a currency outside the supported registry.
var ErrCurrencyUnsupported = apperr.New(http.StatusBadRequest, billing.CodeCurrencyUnsupported, "unknown currency")

// requireCurrency validates and normalizes a supported currency.
func requireCurrency(currency string) (string, error) {
	currency = strings.TrimSpace(currency)
	if currency == "" {
		return "", apperr.Invalidf("currency required")
	}
	normalized := moneyutil.NormalizeCurrency(currency)
	if err := moneyutil.ValidateCurrency(normalized); err != nil {
		return "", fmt.Errorf("%w: %q", ErrCurrencyUnsupported.WithParam("currency"), normalized)
	}
	return normalized, nil
}

// registryCurrency is requireCurrency for an operation of the merchant in
// ctx that takes money, naming param when it refuses (currencyRefusal).
func (s *Service) registryCurrency(ctx context.Context, param, code string) (string, error) {
	normalized, err := requireCurrency(code)
	if errors.Is(err, ErrCurrencyUnsupported) {
		return "", s.currencyRefusal(ctx, param, moneyutil.NormalizeCurrency(code))
	}
	return normalized, err
}

// currencyRefusal refuses code, which OpenRails' registry does not hold:
// merchant_unit_unsupported when it is one of the merchant's own units, else
// currency_unsupported.
func (s *Service) currencyRefusal(ctx context.Context, param, code string) error {
	if err := s.unitRefusal(ctx, param, code); err != nil {
		return err
	}
	return fmt.Errorf("%w: %q", ErrCurrencyUnsupported.WithParam(param), code)
}

// unitRefusal is merchant_unit_unsupported when code is one of the own units
// of the merchant in ctx, else nil.
func (s *Service) unitRefusal(ctx context.Context, param, code string) error {
	if _, known := moneyutil.LookupCurrency(code); known || s == nil || s.rt == nil || s.rt.DB == nil {
		return nil
	}
	settings, err := merchantconfig.NewStore(s.rt.DB).Settings(ctx)
	if err != nil {
		return nil
	}
	if _, err := settings.Currencies.Money(code); errors.Is(err, currency.ErrMerchantUnit) {
		return merchantconfig.UnitRefusal(param, err)
	}
	return nil
}

// refuseCatalogUnits refuses (merchant_unit_unsupported) a catalog document
// whose credit grants or rate cards name one of the merchant's own units.
// Parsing the document already refused a price or limit in one: it reads
// OpenRails' registry alone.
func (s *Service) refuseCatalogUnits(ctx context.Context, app catalogwire.Application) error {
	settings, err := merchantconfig.NewStore(s.rt.DB).Settings(ctx)
	if err != nil || len(settings.Currencies.Units()) == 0 {
		return nil
	}
	refuse := func(param string, code catalogwire.Currency) error {
		if _, err := settings.Currencies.Money(string(code)); errors.Is(err, currency.ErrMerchantUnit) {
			return merchantconfig.UnitRefusal(param, err)
		}
		return nil
	}
	for _, key := range slices.Sorted(maps.Keys(app.Products)) {
		product := app.Products[key]
		if grant := product.CreditGrant; grant.Set && !grant.Null {
			if err := refuse("products."+key+".credit_grant.currency", grant.Value.Currency); err != nil {
				return err
			}
		}
		for i, card := range product.RateCards.Value {
			if err := refuse(fmt.Sprintf("products.%s.rate_cards[%d].price.currency", key, i), card.Price.Currency); err != nil {
				return err
			}
		}
	}
	return nil
}
