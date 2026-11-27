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
	if strings.TrimSpace(code) == "" {
		return "", apperr.Invalidf("currency required").WithParam(param)
	}
	normalized, err := requireCurrency(code)
	if errors.Is(err, ErrCurrencyUnsupported) {
		return "", s.currencyRefusal(ctx, param, moneyutil.NormalizeCurrency(code))
	}
	return normalized, err
}

// balanceCurrency is code normalized when it is a registry currency or one of
// the own units of the merchant in ctx: what a balance, lot or spend may be
// in. Any other code is currency_unsupported, naming param.
func (s *Service) balanceCurrency(ctx context.Context, param, code string) (string, error) {
	normalized := moneyutil.NormalizeCurrency(code)
	if normalized == "" {
		return "", apperr.Invalidf("currency required").WithParam(param)
	}
	if _, ok := moneyutil.LookupCurrency(normalized); ok {
		return normalized, nil
	}
	registry, err := merchantconfig.NewStore(s.rt.DB).Registry(ctx)
	if err != nil {
		return "", err
	}
	if _, ok := registry.Unit(normalized); ok {
		return normalized, nil
	}
	return "", fmt.Errorf("%w: %q", ErrCurrencyUnsupported.WithParam(param), normalized)
}

// withUnits gives each balance in one of the merchant's own units that unit,
// to show its amounts at.
func (s *Service) withUnits(ctx context.Context, balances []billing.Balance) error {
	var registry *currency.Registry
	for i := range balances {
		code := string(balances[i].Currency)
		if !currency.IsUnitCode(code) {
			continue
		}
		if registry == nil {
			r, err := merchantconfig.NewStore(s.rt.DB).Registry(ctx)
			if err != nil {
				return err
			}
			registry = &r
		}
		if u, ok := registry.Unit(code); ok {
			balances[i].Unit = &billing.MerchantCurrency{Code: catalogwire.Currency(u.Code), Name: u.Name, Symbol: u.Symbol, Scale: u.Decimals}
		}
	}
	return nil
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

// ownUnit refuses (currency_unsupported, naming param) a code spelled as a
// merchant's unit that the merchant in ctx does not declare.
func (s *Service) ownUnit(ctx context.Context, param, code string) error {
	if !currency.IsUnitCode(code) {
		return nil
	}
	registry, err := merchantconfig.NewStore(s.rt.DB).Registry(ctx)
	if err != nil {
		return err
	}
	if _, ok := registry.Unit(code); ok {
		return nil
	}
	return fmt.Errorf("%w: %q", ErrCurrencyUnsupported.WithParam(param), moneyutil.NormalizeCurrency(code))
}

// checkCatalogUnits refuses (currency_unsupported) a catalog document whose
// prices, credit grants or rate cards name a unit the merchant does not
// declare. Parsing it already refused any other unknown currency.
func (s *Service) checkCatalogUnits(ctx context.Context, app catalogwire.Application) error {
	for _, key := range slices.Sorted(maps.Keys(app.Products)) {
		product := app.Products[key]
		if grant := product.CreditGrant; grant.Set && !grant.Null {
			if err := s.ownUnit(ctx, "products."+key+".credit_grant.currency", string(grant.Value.Currency)); err != nil {
				return err
			}
		}
		for _, priceKey := range slices.Sorted(maps.Keys(product.Prices)) {
			if price := product.Prices[priceKey]; price.Currency.Set {
				if err := s.ownUnit(ctx, "products."+key+".prices."+priceKey+".currency", string(price.Currency.Value)); err != nil {
					return err
				}
			}
		}
		for i, card := range product.RateCards.Value {
			if err := s.ownUnit(ctx, fmt.Sprintf("products.%s.rate_cards[%d].price.currency", key, i), string(card.Price.Currency)); err != nil {
				return err
			}
		}
	}
	return nil
}
