package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/catalogrules"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money"
)

// Enterprise arrears facade (#798): the host-facing seams for negotiated rate
// cards, invoice profiles (net-N terms + document fields), exposure-freshness
// sweeps, past-due marking, collection and manual remittance. All input/output
// types here are public — hosts cannot import internal/modules/money.

// Invoice collection methods (#798, mirrors money constants).
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

// UsageRateCardInput declares an in_arrears usage rate card. Payer nil = the
// merchant-default card (ProductID required); Payer set = a negotiated
// per-payer price/allowance override inheriting the default's product, filter,
// and meter contract.
type UsageRateCardInput struct {
	Payer     *identity.CustomerID
	ProductID *uuid.UUID
	MeterKey  string
	Filter    map[string][]string
	Price     catalog.RatePrice
	Allowance *catalog.Allowance
	// ExpectedRevision is a payer override's revision (0: none yet).
	ExpectedRevision *int64
}

// SetUsageRateCard upserts an in_arrears usage rate card: the merchant
// default (ProductID set) or a negotiated per-payer override (Payer set).
func (s *Service) SetUsageRateCard(ctx context.Context, in UsageRateCardInput) error {
	_, err := catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (struct{}, error) {
		if err := scoped.expectRateCardRevision(ctx, in); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, scoped.setUsageRateCard(ctx, in)
	})
	return err
}

func (s *Service) expectRateCardRevision(ctx context.Context, in UsageRateCardInput) error {
	if in.ExpectedRevision == nil {
		return nil
	}
	if in.Payer == nil {
		return s.expectCatalogRevision(meterObject(in.MeterKey), in.ExpectedRevision)
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	var current int64
	row, err := s.catalogDatabase().Gen(ctx).GetPayerRateCard(ctx, gen.GetPayerRateCardParams{MerchantID: mid.UUID(), CustomerID: in.Payer.UUID(), MeterKey: in.MeterKey})
	switch {
	case err == nil:
		current = row.Revision
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	return checkRevision("rate override "+in.MeterKey, in.ExpectedRevision, current)
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
		Payer:     in.Payer,
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

// ListRateOverrides returns one page of customers' rate overrides, of one
// customer and one meter when given.
func (s *Service) ListRateOverrides(ctx context.Context, payer *identity.CustomerID, meterKey string, page billing.PageRequest) (billing.ListPage[billing.RateOverride], error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return billing.ListPage[billing.RateOverride]{}, pinErr
	}
	defer release()
	if s == nil || s.rt == nil {
		return billing.ListPage[billing.RateOverride]{}, fmt.Errorf("service not initialized")
	}
	return s.moneyService().ListRateOverrides(ctx, payer, meterKey, page)
}

// GetPayerRateCard reads a customer's rate override for one meter.
func (s *Service) GetPayerRateCard(ctx context.Context, payer identity.CustomerID, meterKey string) (*billing.RateOverride, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()
	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	return s.moneyService().GetPayerRateCard(ctx, payer, meterKey)
}

// DeletePayerRateCard removes a payer's negotiated override for a meter.
func (s *Service) DeletePayerRateCard(ctx context.Context, payer identity.CustomerID, meterKey string) error {
	_, err := catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (struct{}, error) {
		return struct{}{}, scoped.deletePayerRateCard(ctx, payer, meterKey)
	})
	return err
}

func (s *Service) deletePayerRateCard(ctx context.Context, payer identity.CustomerID, meterKey string) error {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	return money.NewMoneyService(s.catalogDatabase()).DeletePayerRateCard(ctx, payer, meterKey)
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

// PendingChargeDTO is one accrued-but-uninvoiced owed item (running spend).
// Source is the accrual identity (e.g. "metered:storage.repo_public_gb").
type PendingChargeDTO struct {
	SourceType string    `json:"source_type"`
	Source     string    `json:"source"`
	Amount     int64     `json:"amount"`
	InvoiceAt  time.Time `json:"invoice_at"`
}

// ListPendingCharges returns a payer's accrued-but-uninvoiced owed items —
// the current-period running spend after a SweepUsage.
func (s *Service) ListPendingCharges(ctx context.Context, payer identity.CustomerID, currency string) ([]PendingChargeDTO, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	cur, err := requireCurrency(currency)
	if err != nil {
		return nil, err
	}
	rows, err := s.moneyService().ListPendingCharges(ctx, payer, cur)
	if err != nil {
		return nil, err
	}
	out := make([]PendingChargeDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, PendingChargeDTO{
			SourceType: r.SourceType,
			Source:     r.Source,
			Amount:     r.Amount,
			InvoiceAt:  r.InvoiceAt,
		})
	}
	return out, nil
}

// GetOutstandingOwed returns the payer's current arrears exposure: open/
// past-due invoice balances plus accrued-but-uninvoiced pending items.
func (s *Service) GetOutstandingOwed(ctx context.Context, payer identity.CustomerID, currency string) (int64, error) {
	if s == nil || s.rt == nil {
		return 0, fmt.Errorf("service not initialized")
	}
	cur, err := requireCurrency(currency)
	if err != nil {
		return 0, err
	}
	var out int64
	err = s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var e error
		out, e = s.moneyService().GetOutstandingOwed(ctx, payer, cur)
		return e
	})
	return out, err
}

// NotifyOverdueInvoices tells each payer once about each invoice still owed
// past its due date. Returns the number of new notices.
func (s *Service) NotifyOverdueInvoices(ctx context.Context, now time.Time) (int, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return 0, pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return 0, fmt.Errorf("service not initialized")
	}
	return s.moneyService().NotifyOverdueInvoices(ctx, now)
}

// ChargeOutstanding collects the merchant's chargeable open/past-due
// receivables (collection_method=charge_automatically, saved method on file)
// through durable invoice_collection operations. Returns the settled count.
func (s *Service) ChargeOutstanding(ctx context.Context, minThreshold int64) (int, error) {
	rt, err := s.runtime()
	if err != nil {
		return 0, err
	}
	if rt.MoneyCharger == nil {
		return 0, fmt.Errorf("invoice collection charger not configured")
	}
	var n int
	err = s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var e error
		n, e = s.moneyService().ChargeOutstanding(ctx, rt.IntentRunner(), minThreshold)
		return e
	})
	return n, err
}
