package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/catalog"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/catalogrules"
	"github.com/open-rails/openrails/internal/modules/money"
)

// Arrears seams: meters, their rate cards and exposure-freshness sweeps.

// Usage collection methods, as money's.
const (
	CollectionChargeAutomatically = money.CollectionChargeAutomatically
	CollectionSendInvoice         = money.CollectionSendInvoice
)

// EnsureUsageMeter idempotently declares a catalog meter.
func (s *Service) EnsureUsageMeter(ctx context.Context, spec catalogrules.Meter) error {
	_, err := catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (struct{}, error) {
		return struct{}{}, scoped.ensureUsageMeter(ctx, spec)
	})
	return err
}

func (s *Service) ensureUsageMeter(ctx context.Context, spec catalogrules.Meter) error {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	return money.NewMoneyService(s.catalogDatabase()).EnsureUsageMeter(ctx, spec)
}

// SetUsageMeter declares a meter and, in the same catalog edit, its rate
// card: card sets it, removeCard removes it, neither keeps it. With
// expectedRevision, only while the meter is at it (0: no meter yet).
func (s *Service) SetUsageMeter(ctx context.Context, spec catalogrules.Meter, card *UsageRateCardInput, removeCard bool, expectedRevision *int64) error {
	_, err := catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (struct{}, error) {
		if err := scoped.expectCatalogRevision(meterObject(spec.Key), expectedRevision); err != nil {
			return struct{}{}, err
		}
		if err := scoped.ensureUsageMeter(ctx, spec); err != nil {
			return struct{}{}, err
		}
		switch {
		case card != nil:
			return struct{}{}, scoped.setUsageRateCard(ctx, *card)
		case removeCard:
			if err := scoped.deleteDefaultUsageRateCard(ctx, spec.Key); err != nil && !errors.Is(err, money.ErrDefaultRateCardNotFound) {
				return struct{}{}, err
			}
		}
		return struct{}{}, nil
	})
	return err
}

// UsageRateCardInput declares a meter's in_arrears usage rate card under
// the product whose invoices carry it.
type UsageRateCardInput struct {
	ProductID *uuid.UUID
	MeterKey  string
	Filter    map[string][]string
	Price     catalog.RatePrice
	Allowance *catalog.Allowance
}

func (s *Service) setUsageRateCard(ctx context.Context, in UsageRateCardInput) error {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	return money.NewMoneyService(s.catalogDatabase()).SetUsageRateCard(ctx, money.UsageRateCardInput{
		ProductID: in.ProductID,
		MeterKey:  in.MeterKey,
		Filter:    in.Filter,
		Price:     in.Price,
		Allowance: in.Allowance,
	})
}

func (s *Service) deleteDefaultUsageRateCard(ctx context.Context, meterKey string) error {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	return money.NewMoneyService(s.catalogDatabase()).DeleteDefaultUsageRateCard(ctx, meterKey)
}

// SweepUsage rates a payer's reported usage over [from, to) into pending owed
// items (watermarked; safe to repeat) so arrears exposure stays fresh
// mid-period without finalizing an invoice.
func (s *Service) SweepUsage(ctx context.Context, payer identity.CustomerID, currency string, from, to time.Time) error {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	if payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	cur, err := requireCurrency(currency)
	if err != nil {
		return err
	}
	return s.moneyService().SweepUsage(ctx, payer, cur, from, to)
}
