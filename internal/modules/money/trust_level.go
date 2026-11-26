package money

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
)

// TrustLevel is the customer's effective trust tier in one currency: its
// explicit level, else the highest of the merchant's trust_tiers whose
// requirement its paid totals meet now, else merchantconfig.DefaultTrustLevel.
func (s *MoneyService) TrustLevel(ctx context.Context, customer identity.CustomerID, currency string) (billing.EffectiveTrustLevel, error) {
	levels, err := s.TrustLevelsIn(ctx, []uuid.UUID{customer.UUID()}, currency)
	return levels[customer.UUID()], err
}

// TrustLevelsIn are TrustLevel for many customers in one currency, in one
// read; every customer named is present.
func (s *MoneyService) TrustLevelsIn(ctx context.Context, customers []uuid.UUID, currency string) (map[uuid.UUID]billing.EffectiveTrustLevel, error) {
	cur := normalizeCurrency(currency)
	out := make(map[uuid.UUID]billing.EffectiveTrustLevel, len(customers))
	for _, id := range customers {
		out[id] = defaultTrust(cur)
	}
	// A merchant's own unit is never paid in nor given a level.
	if len(customers) == 0 || IsMerchantUnit(cur) || RequireBillingCurrency(cur) != nil {
		return out, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	trust, err := s.trustParam(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Gen(ctx).ListCustomersTrustIn(ctx, gen.ListCustomersTrustInParams{MerchantID: mid.UUID(), CustomerIds: customers, Currency: cur, Trust: trust, Now: s.now()})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.CustomerID] = effectiveTrust(cur, r.ExplicitTier, r.TrustLevel, r.NetPaidAmount, r.FirstPaidAt)
	}
	return out, nil
}

// CustomersTrustLevels are the customers' effective trust tiers in each
// currency they hold an explicit level or paid totals in, or the merchant's
// trust_tiers name, by currency.
func (s *MoneyService) CustomersTrustLevels(ctx context.Context, customers []uuid.UUID) (map[uuid.UUID][]billing.EffectiveTrustLevel, error) {
	out := make(map[uuid.UUID][]billing.EffectiveTrustLevel, len(customers))
	for _, id := range customers {
		out[id] = []billing.EffectiveTrustLevel{}
	}
	if len(customers) == 0 {
		return out, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	settings, err := s.merchantSettings(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Gen(ctx).ListCustomersTrust(ctx, gen.ListCustomersTrustParams{MerchantID: mid.UUID(), CustomerIds: customers, Trust: settings.TrustParam(), Now: s.now(),
		RowLimit: int32(min(len(customers)*len(catalog.Currencies()), math.MaxInt32))}) // #nosec G115 -- clamped to int32
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.CustomerID] = append(out[r.CustomerID], effectiveTrust(r.Currency, r.ExplicitTier, r.TrustLevel, r.NetPaidAmount, r.FirstPaidAt))
	}
	for id, levels := range out {
		for cur := range settings.TrustRules {
			if !slices.ContainsFunc(levels, func(l billing.EffectiveTrustLevel) bool { return string(l.Currency) == cur }) {
				levels = append(levels, defaultTrust(cur))
			}
		}
		slices.SortFunc(levels, func(a, b billing.EffectiveTrustLevel) int {
			return strings.Compare(string(a.Currency), string(b.Currency))
		})
		out[id] = levels
	}
	return out, nil
}

// trustParam is the merchant's trust tier rules as billing.trust_tier reads
// them.
func (s *MoneyService) trustParam(ctx context.Context) ([]byte, error) {
	settings, err := s.merchantSettings(ctx)
	return settings.TrustParam(), err
}

// merchantSettings are the merchant's validated settings; none bound is the
// defaults.
func (s *MoneyService) merchantSettings(ctx context.Context) (merchantconfig.Settings, error) {
	settings, err := merchantconfig.NewStore(s.db).Settings(ctx)
	if errors.Is(err, merchantconfig.ErrNoSettings) {
		return merchantconfig.Settings{}, nil
	}
	return settings, err
}

func defaultTrust(currency string) billing.EffectiveTrustLevel {
	return billing.EffectiveTrustLevel{Currency: catalog.Currency(currency), TrustLevel: merchantconfig.DefaultTrustLevel, Source: billing.TrustLevelDefault}
}

func effectiveTrust(currency string, explicit *string, level string, netPaid int64, firstPaidAt *time.Time) billing.EffectiveTrustLevel {
	out := billing.EffectiveTrustLevel{Currency: catalog.Currency(currency), TrustLevel: level, Source: billing.TrustLevelDefault, NetPaidAmount: netPaid}
	switch {
	case explicit != nil && *explicit != "":
		out.Source = billing.TrustLevelExplicit
	case level != merchantconfig.DefaultTrustLevel:
		out.Source = billing.TrustLevelRule
	}
	if firstPaidAt != nil {
		at := firstPaidAt.UTC()
		out.FirstPaidAt = &at
	}
	return out
}
