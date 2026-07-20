package money

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/open-rails/openrails/pkg/pricing"
)

// Enterprise arrears seams (#798): host-driven sweep, past-due marking,
// pending-charge visibility and host-owned usage meters / rate cards
// (including negotiated per-payer overrides).

// SweepUsage rates a payer's reported usage over [from, to) through the
// catalog rate cards into pending owed invoice items — the same watermarked
// sweep FinalizeInvoice runs, without finalizing. Hosts call it to keep
// arrears exposure (GetOutstandingOwed) fresh mid-period.
func (s *MoneyService) SweepUsage(ctx context.Context, payer identity.CustomerID, currency string, from, to time.Time) error {
	return s.sweepCatalogRateCardUsage(ctx, payer, currency, from, to)
}

// MarkInvoicesPastDue transitions the merchant's overdue open receivables
// (due_at < now, amount_due > 0) to past_due. Returns the number flipped.
func (s *MoneyService) MarkInvoicesPastDue(ctx context.Context, now time.Time) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("money service not initialized")
	}
	if now.IsZero() {
		now = s.now()
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var e error
		n, e = s.db.Gen(ctx).MarkInvoicesPastDue(ctx, gen.MarkInvoicesPastDueParams{
			MerchantID: tid.UUID(), Now: now.UTC(),
		})
		return e
	})
	return int(n), err
}

// PendingCharge is one accrued-but-uninvoiced owed item (running spend).
// Source is the accrual identity (e.g. "metered:storage.repo_public_gb").
type PendingCharge struct {
	SourceType string    `json:"source_type"`
	Source     string    `json:"source"`
	Amount     int64     `json:"amount"`
	InvoiceAt  time.Time `json:"invoice_at"`
}

// ListPendingCharges returns a payer's pending (uninvoiced) owed items.
func (s *MoneyService) ListPendingCharges(ctx context.Context, payer identity.CustomerID, currency string) ([]PendingCharge, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	cur := normalizeCurrency(currency)
	if err := RequireBillingCurrency(cur); err != nil {
		return nil, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var out []PendingCharge
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		rows, e := s.db.Gen(ctx).ListPendingInvoiceItemsByPayer(ctx, gen.ListPendingInvoiceItemsByPayerParams{
			MerchantID: tid.UUID(), CustomerID: payer.UUID(), Currency: cur,
		})
		if e != nil {
			return e
		}
		out = make([]PendingCharge, 0, len(rows))
		for _, r := range rows {
			out = append(out, PendingCharge{
				SourceType: r.SourceType,
				Source:     r.Source,
				Amount:     r.Amount,
				InvoiceAt:  r.InvoiceAt,
			})
		}
		return nil
	})
	return out, err
}

// UsageMeterSpec declares a host-owned usage meter (upserted idempotently).
type UsageMeterSpec struct {
	Key           string
	EventType     string
	ValueProperty string
	Aggregation   string // sum | count (rating supports these)
	Unit          string
}

// EnsureUsageMeter idempotently upserts a catalog meter. Host-owned catalogs
// (no manifest push) use this to declare their metered dimensions.
func (s *MoneyService) EnsureUsageMeter(ctx context.Context, spec UsageMeterSpec) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	key := strings.TrimSpace(spec.Key)
	if key == "" {
		return fmt.Errorf("meter key required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		_, e := s.db.Qx(ctx).Exec(ctx, `
INSERT INTO openrails.catalog_meters (merchant_id, key, event_type, value_property, aggregation, unit)
VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), NULLIF($5, ''), NULLIF($6, ''))
ON CONFLICT (merchant_id, key) DO UPDATE
SET event_type = EXCLUDED.event_type,
    value_property = EXCLUDED.value_property,
    aggregation = EXCLUDED.aggregation,
    unit = EXCLUDED.unit,
    updated_at = now()`,
			tid.UUID(), key, strings.TrimSpace(spec.EventType), strings.TrimSpace(spec.ValueProperty),
			strings.TrimSpace(spec.Aggregation), strings.TrimSpace(spec.Unit))
		return e
	})
}

// UsageRateCardInput declares a usage rate card. Payer nil/zero = the
// merchant-default card (ProductID required); Payer set = a negotiated
// per-payer override (#798) that replaces the default for that meter.
type UsageRateCardInput struct {
	Payer     *identity.CustomerID
	ProductID *uuid.UUID
	MeterKey  string
	Price     pricing.RatePrice
	Allowance *pricing.Allowance
}

// SetUsageRateCard upserts an in_arrears usage rate card. Operator surface —
// negotiated pricing is never self-serve.
func (s *MoneyService) SetUsageRateCard(ctx context.Context, in UsageRateCardInput) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	meterKey := strings.TrimSpace(in.MeterKey)
	if meterKey == "" {
		return fmt.Errorf("meter_key required")
	}
	payerScoped := in.Payer != nil && !in.Payer.IsZero()
	if !payerScoped && (in.ProductID == nil || *in.ProductID == uuid.Nil) {
		return fmt.Errorf("product_id required for a merchant-default rate card")
	}
	priceJSON, err := json.Marshal(in.Price)
	if err != nil {
		return fmt.Errorf("encode rate card price: %w", err)
	}
	var allowanceJSON []byte
	if in.Allowance != nil {
		if allowanceJSON, err = json.Marshal(in.Allowance); err != nil {
			return fmt.Errorf("encode rate card allowance: %w", err)
		}
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if payerScoped {
			if err := ensureCustomer(ctx, gen.New(tx), tid.UUID(), in.Payer.UUID()); err != nil {
				return err
			}
			_, e := tx.Exec(ctx, `
INSERT INTO openrails.catalog_rate_cards
    (merchant_id, product_id, customer_id, ordinal, meter_key, payment_term, allowance, price)
VALUES ($1, $2, $3, 1, $4, 'in_arrears', $5::jsonb, $6::jsonb)
ON CONFLICT (merchant_id, customer_id, meter_key) WHERE meter_key IS NOT NULL AND customer_id IS NOT NULL
DO UPDATE SET product_id = EXCLUDED.product_id,
    allowance = EXCLUDED.allowance,
    price = EXCLUDED.price,
    updated_at = now()`,
				tid.UUID(), in.ProductID, in.Payer.UUID(), meterKey, allowanceJSONOrNull(allowanceJSON), string(priceJSON))
			return e
		}
		_, e := tx.Exec(ctx, `
INSERT INTO openrails.catalog_rate_cards
    (merchant_id, product_id, ordinal, meter_key, payment_term, allowance, price)
VALUES ($1, $2,
    COALESCE((SELECT MAX(ordinal) + 1 FROM openrails.catalog_rate_cards
              WHERE merchant_id = $1 AND product_id = $2 AND customer_id IS NULL), 1),
    $3, 'in_arrears', $4::jsonb, $5::jsonb)
ON CONFLICT (merchant_id, meter_key) WHERE meter_key IS NOT NULL AND customer_id IS NULL
DO UPDATE SET product_id = EXCLUDED.product_id,
    allowance = EXCLUDED.allowance,
    price = EXCLUDED.price,
    updated_at = now()`,
			tid.UUID(), in.ProductID, meterKey, allowanceJSONOrNull(allowanceJSON), string(priceJSON))
		return e
	})
}

// DeletePayerRateCard removes a payer's negotiated override for a meter,
// restoring the merchant default.
func (s *MoneyService) DeletePayerRateCard(ctx context.Context, payer identity.CustomerID, meterKey string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	meterKey = strings.TrimSpace(meterKey)
	if meterKey == "" {
		return fmt.Errorf("meter_key required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		_, e := s.db.Pool().Exec(ctx, `
DELETE FROM openrails.catalog_rate_cards
WHERE merchant_id = $1 AND customer_id = $2 AND meter_key = $3`,
			tid.UUID(), payer.UUID(), meterKey)
		return e
	})
}

func allowanceJSONOrNull(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}
