package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// NormalizeCatalogCredits projects declarations to current public names without
// writing registry rows. Planning uses it before comparing desired product specs.
func (s *Service) NormalizeCatalogCredits(ctx context.Context, in CreditsSpec) (CreditsSpec, error) {
	if len(in) == 0 {
		return in, nil
	}
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	out := make(CreditsSpec, len(in))
	for key, spec := range in {
		unit := strings.TrimSpace(spec.Unit)
		if _, builtin := moneyutil.CurrencyScale(unit); builtin {
			spec.Unit = money.NormalizeCurrency(unit)
			out[key] = spec
			continue
		}
		if s.rt.Merchants == nil {
			return nil, fmt.Errorf("merchant naming authority is not configured")
		}
		name := unit
		if strings.HasPrefix(unit, "credit:") {
			name, err = s.moneyService().CustomUnitName(ctx, unit)
			if err != nil {
				return nil, err
			}
		} else if slug, local, qualified := strings.Cut(unit, "/"); qualified {
			owner, e := s.rt.Merchants.GetBySlug(ctx, slug)
			if e != nil {
				return nil, e
			}
			if owner.ID != mid {
				return nil, fmt.Errorf("custom credit name belongs to another merchant")
			}
			name = local
		}
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" || strings.ContainsAny(name, "/ :\t\r\n") {
			return nil, fmt.Errorf("invalid custom credit unit %q", unit)
		}
		slug, e := s.rt.Merchants.CanonicalSlug(ctx, mid)
		if e != nil {
			return nil, e
		}
		spec.Unit = slug + "/" + name
		out[key] = spec
	}
	return out, nil
}

func (s *Service) storedCatalogCredits(ctx context.Context, in CreditsSpec) (models.CreditsSpec, error) {
	if len(in) == 0 {
		return toModelCreditsSpec(in), nil
	}
	normalized, err := s.NormalizeCatalogCredits(ctx, in)
	if err != nil {
		return nil, err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	out := toModelCreditsSpec(normalized)
	err = s.rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for key, spec := range out {
			unit := spec.Unit
			if _, local, ok := strings.Cut(unit, "/"); ok {
				unit = local
			}
			code, e := defineCustomCreditUnit(ctx, tx, mid.UUID(), key, unit)
			if e != nil {
				return e
			}
			spec.Unit = code
			out[key] = spec
		}
		return nil
	})
	return out, err
}

func (s *Service) catalogProduct(ctx context.Context, p *models.Product) (*CatalogProduct, error) {
	out := productToCatalogProduct(p)
	for key, spec := range out.CreditsSpec {
		unit, err := s.DisplayCurrency(ctx, spec.Unit)
		if err != nil {
			return nil, err
		}
		spec.Unit = unit
		out.CreditsSpec[key] = spec
	}
	return out, nil
}
