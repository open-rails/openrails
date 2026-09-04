package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/merchant"
)

// resolveCurrency translates an external unit name exactly once. Ledger and
// admission keys receive only ISO codes or the existing custom-unit UUID.
func (s *Service) resolveCurrency(ctx context.Context, raw string) (string, error) {
	code, err := requireCurrency(raw)
	if err != nil {
		return "", err
	}
	if !money.IsQualifiedUnit(code) {
		return code, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return "", err
	}
	var canonical string
	if strings.HasPrefix(code, "credit:") {
		// In-process callers can carry a resolved identity through an operation;
		// the registry lookup still proves it belongs to the selected merchant.
		canonical = code
	} else {
		slug, name, ok := strings.Cut(code, "/")
		if !ok || name == "" || strings.Contains(name, "/") {
			return "", fmt.Errorf("invalid custom credit name")
		}
		if s.rt.Merchants == nil {
			return "", fmt.Errorf("merchant naming authority is not configured")
		}
		owner, err := s.rt.Merchants.GetBySlug(ctx, slug)
		if err != nil {
			return "", err
		}
		if owner.ID != mid {
			return "", fmt.Errorf("custom credit name belongs to another merchant")
		}
		err = s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
			var err error
			canonical, err = s.moneyService().CustomUnitCode(ctx, name)
			return err
		})
		if err != nil {
			return "", err
		}
	}
	err = s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error { _, _, err := s.moneyService().ResolveUnit(ctx, canonical); return err })
	return canonical, err
}

// DisplayCurrency projects a public name from immutable ownership. It never
// interprets a stored prefix as a merchant lookup or fallback alias.
func (s *Service) DisplayCurrency(ctx context.Context, code string) (string, error) {
	if !money.IsQualifiedUnit(code) {
		return money.NormalizeCurrency(code), nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return "", err
	}
	var name string
	err = s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var err error
		name, err = s.moneyService().CustomUnitName(ctx, code)
		return err
	})
	if err != nil {
		return "", err
	}
	if s.rt.Merchants == nil {
		return "", fmt.Errorf("merchant naming authority is not configured")
	}
	slug, err := s.rt.Merchants.CanonicalSlug(ctx, mid)
	if err != nil {
		return "", err
	}
	return slug + "/" + name, nil
}
