package service

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/pricing"
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

// InvoiceProfileDTO is a payer's enterprise invoicing profile: net-N credit
// terms, collection method and the document fields snapshotted onto every
// invoice at finalize.
type InvoiceProfileDTO struct {
	NetTermsDays     int                 `json:"net_terms_days"`
	CollectionMethod string              `json:"collection_method"`
	PONumber         string              `json:"po_number,omitempty"`
	Tax              map[string]any      `json:"tax,omitempty"`
	BillingContacts  []InvoiceContactDTO `json:"billing_contacts,omitempty"`
	Memo             string              `json:"memo,omitempty"`
}

// SetCustomerInvoiceProfile upserts a payer's invoicing profile. Operator
// surface — a payer must not grant itself credit terms.
func (s *Service) SetCustomerInvoiceProfile(ctx context.Context, payer identity.CustomerID, p InvoiceProfileDTO) error {
	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	if payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	contacts := make([]models.InvoiceContact, 0, len(p.BillingContacts))
	for _, c := range p.BillingContacts {
		contacts = append(contacts, models.InvoiceContact{Name: c.Name, Email: c.Email})
	}
	return s.moneyService().SetCustomerInvoiceProfile(ctx, payer, money.CustomerInvoiceProfile{
		NetTermsDays:     p.NetTermsDays,
		CollectionMethod: p.CollectionMethod,
		PONumber:         p.PONumber,
		Tax:              p.Tax,
		BillingContacts:  contacts,
		Memo:             p.Memo,
	})
}

// EnsureCustomerInvoiceProfile inserts a payer's invoicing profile when none
// is stored. Existing operator configuration is left unchanged. The returned
// boolean is true only when this call committed a new profile.
func (s *Service) EnsureCustomerInvoiceProfile(ctx context.Context, payer identity.CustomerID, p InvoiceProfileDTO) (bool, error) {
	if s == nil || s.rt == nil {
		return false, fmt.Errorf("service not initialized")
	}
	if payer.IsZero() {
		return false, fmt.Errorf("payer required")
	}
	contacts := make([]models.InvoiceContact, 0, len(p.BillingContacts))
	for _, c := range p.BillingContacts {
		contacts = append(contacts, models.InvoiceContact{Name: c.Name, Email: c.Email})
	}
	return s.moneyService().EnsureCustomerInvoiceProfile(ctx, payer, money.CustomerInvoiceProfile{
		NetTermsDays:     p.NetTermsDays,
		CollectionMethod: p.CollectionMethod,
		PONumber:         p.PONumber,
		Tax:              p.Tax,
		BillingContacts:  contacts,
		Memo:             p.Memo,
	})
}

// GetCustomerInvoiceProfile returns a payer's invoicing profile (nil = none).
func (s *Service) GetCustomerInvoiceProfile(ctx context.Context, payer identity.CustomerID) (*InvoiceProfileDTO, error) {
	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	p, err := s.moneyService().GetCustomerInvoiceProfile(ctx, payer)
	if err != nil || p == nil {
		return nil, err
	}
	return &InvoiceProfileDTO{
		NetTermsDays:     p.NetTermsDays,
		CollectionMethod: p.CollectionMethod,
		PONumber:         p.PONumber,
		Tax:              p.Tax,
		BillingContacts:  contactsToDTO(p.BillingContacts),
		Memo:             p.Memo,
	}, nil
}

// EnsureUsageProduct idempotently ensures a catalog product for host-owned
// usage rate cards, returning its (deterministic) id.
func (s *Service) EnsureUsageProduct(ctx context.Context, key, displayName string) (uuid.UUID, error) {
	if s == nil || s.rt == nil {
		return uuid.Nil, fmt.Errorf("service not initialized")
	}
	products, err := s.requireProductService()
	if err != nil {
		return uuid.Nil, err
	}
	if existing, gerr := products.GetByKey(ctx, key); gerr == nil && existing != nil {
		return existing.ID, nil
	}
	p, err := s.CreateProduct(ctx, CreateProductRequest{Key: key, DisplayName: displayName})
	if err != nil {
		// Lost a create race: the row exists now.
		if existing, gerr := products.GetByKey(ctx, key); gerr == nil && existing != nil {
			return existing.ID, nil
		}
		return uuid.Nil, err
	}
	return p.ID, nil
}

// UsageMeterSpec declares a host-owned usage meter (upserted idempotently).
type UsageMeterSpec struct {
	Key           string `json:"key"`
	EventType     string `json:"event_type"`
	ValueProperty string `json:"value_property"`
	Aggregation   string `json:"aggregation"` // sum | count
	Unit          string `json:"unit,omitempty"`
}

// EnsureUsageMeter idempotently declares a host-owned catalog meter.
func (s *Service) EnsureUsageMeter(ctx context.Context, spec UsageMeterSpec) error {
	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	return s.moneyService().EnsureUsageMeter(ctx, money.UsageMeterSpec{
		Key:           spec.Key,
		EventType:     spec.EventType,
		ValueProperty: spec.ValueProperty,
		Aggregation:   spec.Aggregation,
		Unit:          spec.Unit,
	})
}

// UsageRateCardInput declares an in_arrears usage rate card. Payer nil = the
// merchant-default card (ProductID required); Payer set = a negotiated
// per-payer override that replaces the default for that meter.
type UsageRateCardInput struct {
	Payer     *identity.CustomerID
	ProductID *uuid.UUID
	MeterKey  string
	Price     pricing.RatePrice
	Allowance *pricing.Allowance
}

// SetUsageRateCard upserts an in_arrears usage rate card: the merchant
// default (ProductID set) or a negotiated per-payer override (Payer set).
func (s *Service) SetUsageRateCard(ctx context.Context, in UsageRateCardInput) error {
	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	return s.moneyService().SetUsageRateCard(ctx, money.UsageRateCardInput{
		Payer:     in.Payer,
		ProductID: in.ProductID,
		MeterKey:  in.MeterKey,
		Price:     in.Price,
		Allowance: in.Allowance,
	})
}

// DeletePayerRateCard removes a payer's negotiated override for a meter.
func (s *Service) DeletePayerRateCard(ctx context.Context, payer identity.CustomerID, meterKey string) error {
	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	return s.moneyService().DeletePayerRateCard(ctx, payer, meterKey)
}

// SweepUsage rates a payer's reported usage over [from, to) into pending owed
// items (watermarked; safe to repeat) so arrears exposure stays fresh
// mid-period without finalizing an invoice.
func (s *Service) SweepUsage(ctx context.Context, payer identity.CustomerID, currency string, from, to time.Time) error {
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

// MarkInvoicesPastDue flips the merchant's overdue open receivables to
// past_due (the host-visible dunning signal). Returns the number flipped.
func (s *Service) MarkInvoicesPastDue(ctx context.Context, now time.Time) (int, error) {
	if s == nil || s.rt == nil {
		return 0, fmt.Errorf("service not initialized")
	}
	return s.moneyService().MarkInvoicesPastDue(ctx, now)
}

// ChargeOutstanding collects the merchant's chargeable open/past-due
// receivables (collection_method=charge_automatically, saved method on file)
// via the runtime collection plane. Returns successful charge count.
func (s *Service) ChargeOutstanding(ctx context.Context, minThreshold int64) (int, error) {
	if s == nil || s.rt == nil {
		return 0, fmt.Errorf("service not initialized")
	}
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
		n, e = s.moneyService().ChargeOutstanding(ctx, rt.MoneyCharger, minThreshold)
		return e
	})
	return n, err
}

// RecordOutOfBandInvoicePayment applies a manual remittance (wire/check) to a
// send_invoice (or any open) receivable. reference dedups replays.
func (s *Service) RecordOutOfBandInvoicePayment(ctx context.Context, payer identity.CustomerID, invoiceID uuid.UUID, amount int64, reference string) (*InvoiceDTO, error) {
	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	inv, err := s.moneyService().RecordOutOfBandInvoicePayment(ctx, payer, invoiceID, amount, reference)
	if err != nil {
		return nil, err
	}
	dto := invoiceToDTO(inv)
	return &dto, nil
}
