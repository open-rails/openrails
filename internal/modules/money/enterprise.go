package money

import (
	"context"
	"encoding/json"
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
	"github.com/open-rails/openrails/internal/pagination"
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

// NotifyOverdueInvoices queues one overdue notice per open invoice still owed
// past its due date. Returns the number of new notices.
func (s *MoneyService) NotifyOverdueInvoices(ctx context.Context, now time.Time) (int, error) {
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
		n, e = s.db.Gen(ctx).NotifyOverdueInvoices(ctx, gen.NotifyOverdueInvoicesParams{
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

// EnsureUsageMeter idempotently upserts a catalog meter.
func (s *MoneyService) EnsureUsageMeter(ctx context.Context, meter catalogrules.Meter) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	if err := catalogrules.ValidateMeter("usage meter", &meter); err != nil {
		return err
	}
	if !catalogrules.BillingSupported(meter.Aggregation) {
		return fmt.Errorf("usage meter: aggregation %q is not supported for billing", meter.Aggregation)
	}
	groupByJSON, err := json.Marshal(meter.GroupBy)
	if err != nil {
		return fmt.Errorf("encode usage meter group_by: %w", err)
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		queries := gen.New(tx)
		if err := queries.LockUsageMeterKey(ctx, tid.UUID().String()+":"+meter.Key); err != nil {
			return fmt.Errorf("lock usage meter: %w", err)
		}

		var existing catalogrules.Meter
		row, err := queries.GetUsageMeterForUpdate(ctx, gen.GetUsageMeterForUpdateParams{
			MerchantID: tid.UUID(),
			MeterKey:   meter.Key,
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if err := queries.InsertUsageMeter(ctx, gen.InsertUsageMeterParams{
				MerchantID:    tid.UUID(),
				MeterKey:      meter.Key,
				EventType:     meter.EventType,
				ValueProperty: meter.ValueProperty,
				Aggregation:   string(meter.Aggregation),
				Unit:          meter.Unit,
				GroupBy:       groupByJSON,
			}); err != nil {
				return fmt.Errorf("insert usage meter: %w", err)
			}
			return nil
		case err != nil:
			return fmt.Errorf("load usage meter: %w", err)
		}

		existing = catalogrules.Meter{
			Key:           row.Key,
			EventType:     row.EventType,
			ValueProperty: row.ValueProperty,
			Aggregation:   catalog.Aggregation(row.Aggregation),
			Unit:          row.Unit,
		}
		if err := json.Unmarshal(row.GroupBy, &existing.GroupBy); err != nil {
			return fmt.Errorf("decode usage meter group_by: %w", err)
		}
		if usageMeterSemanticsEqual(existing, meter) {
			return nil
		}
		hasActivity, err := usageMeterHasActivity(ctx, tx, tid.UUID(), existing, meter)
		if err != nil {
			return err
		}
		if hasActivity {
			return ErrMeterInUse
		}
		if err := validateUsageMeterRateCardContracts(ctx, queries, tid.UUID(), meter); err != nil {
			return err
		}
		if err := queries.UpdateUsageMeter(ctx, gen.UpdateUsageMeterParams{
			MerchantID:    tid.UUID(),
			MeterKey:      meter.Key,
			EventType:     meter.EventType,
			ValueProperty: meter.ValueProperty,
			Aggregation:   string(meter.Aggregation),
			Unit:          meter.Unit,
			GroupBy:       groupByJSON,
		}); err != nil {
			return fmt.Errorf("update usage meter: %w", err)
		}
		return nil
	})
}

// UsageRateCardInput declares a usage rate card. Payer nil/zero = the
// merchant-default card (ProductID required); Payer set = a negotiated
// per-payer price/allowance override (#798) that inherits the default's
// product, filter, and meter contract.
type UsageRateCardInput struct {
	Payer     *identity.CustomerID
	ProductID *uuid.UUID
	MeterKey  string
	Filter    map[string][]string
	Price     catalog.RatePrice
	Allowance *catalog.Allowance
}

// SetUsageRateCard upserts an in_arrears usage rate card. Operator surface —
// negotiated pricing is never self-serve.
func (s *MoneyService) SetUsageRateCard(ctx context.Context, in UsageRateCardInput) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	meterKey := catalogrules.NormalizeKey(in.MeterKey)
	if meterKey == "" {
		return invalidUsageRateCard(fmt.Errorf("meter_key required"))
	}
	payerScoped := in.Payer != nil && !in.Payer.IsZero()
	if !payerScoped && (in.ProductID == nil || *in.ProductID == uuid.Nil) {
		return invalidUsageRateCard(fmt.Errorf("product_id required for a merchant-default rate card"))
	}
	if payerScoped && in.ProductID != nil {
		return invalidUsageRateCard(fmt.Errorf("product_id is inherited by a payer rate card"))
	}
	if payerScoped && len(in.Filter) > 0 {
		return invalidUsageRateCard(fmt.Errorf("filter is inherited by a payer rate card"))
	}
	if err := catalogrules.ValidateUsagePrice("usage rate card", &in.Price); err != nil {
		return invalidUsageRateCard(err)
	}
	if in.Price.Currency == "" {
		return invalidUsageRateCard(fmt.Errorf("usage rate card: currency is required"))
	}
	if err := RequireBillingCurrency(in.Price.Currency); err != nil {
		return invalidUsageRateCard(err)
	}
	if err := catalogrules.ValidateAllowance("usage rate card", in.Allowance); err != nil {
		return invalidUsageRateCard(err)
	}
	if err := catalogrules.ValidateFilter("usage rate card", &in.Filter); err != nil {
		return invalidUsageRateCard(err)
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
		queries := gen.New(tx)
		meter, err := loadUsageMeterForRateCard(ctx, tx, tid.UUID(), meterKey)
		if err != nil {
			return err
		}
		if !catalogrules.BillingSupported(meter.Aggregation) {
			return invalidUsageRateCard(fmt.Errorf("usage meter: aggregation %q is not supported for billing", meter.Aggregation))
		}
		if err := catalogrules.ValidateDimensions("usage rate card", meter.GroupBy, in.Filter, &in.Price); err != nil {
			return invalidUsageRateCard(err)
		}
		if err := ensureAllowanceSource(
			ctx,
			tx,
			tid.UUID(),
			meterKey,
			in.Payer,
			in.Price.Currency,
			in.Allowance,
		); err != nil {
			return err
		}
		if err := validateRateCardAsAllowanceSource(ctx, queries, tid.UUID(), meter, in.Price); err != nil {
			return err
		}
		if payerScoped {
			if err := ensureCustomer(ctx, queries, tid.UUID(), in.Payer.UUID()); err != nil {
				return err
			}
			defaultCurrency, err := loadDefaultRateCardCurrency(ctx, tx, tid.UUID(), meterKey)
			if err != nil {
				return err
			}
			if in.Price.Currency != defaultCurrency {
				return ErrRateCardCurrencyMismatch
			}
			if err := queries.UpsertPayerUsageRateCard(ctx, gen.UpsertPayerUsageRateCardParams{
				MerchantID: tid.UUID(),
				CustomerID: in.Payer.UUID(),
				MeterKey:   meterKey,
				Allowance:  allowanceJSON,
				Price:      priceJSON,
			}); err != nil {
				return fmt.Errorf("upsert payer usage rate card: %w", err)
			}
			return nil
		}
		if err := ensureActiveProduct(ctx, tx, tid.UUID(), *in.ProductID); err != nil {
			return err
		}
		if err := queries.LockUsageRateCardProduct(
			ctx,
			tid.UUID().String()+":"+in.ProductID.String(),
		); err != nil {
			return fmt.Errorf("lock rate card product: %w", err)
		}
		hasCurrencyConflict, err := queries.UsageRateCardCurrencyConflict(
			ctx,
			gen.UsageRateCardCurrencyConflictParams{
				MerchantID: tid.UUID(),
				MeterKey:   meterKey,
				Currency:   in.Price.Currency,
			},
		)
		if err != nil {
			return fmt.Errorf("check payer rate card currencies: %w", err)
		}
		if hasCurrencyConflict {
			return ErrRateCardCurrencyMismatch
		}
		filterJSON, err := json.Marshal(in.Filter)
		if err != nil {
			return fmt.Errorf("encode rate card filter: %w", err)
		}
		if err := queries.UpsertDefaultUsageRateCard(ctx, gen.UpsertDefaultUsageRateCardParams{
			MerchantID: tid.UUID(),
			ProductID:  *in.ProductID,
			MeterKey:   meterKey,
			Filter:     filterJSON,
			Allowance:  allowanceJSON,
			Price:      priceJSON,
		}); err != nil {
			return fmt.Errorf("upsert default usage rate card: %w", err)
		}
		return nil
	})
}

// DeleteDefaultUsageRateCard removes a meter's merchant-default card after all
// negotiated payer overrides have been removed.
func (s *MoneyService) DeleteDefaultUsageRateCard(ctx context.Context, meterKey string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	meterKey = catalogrules.NormalizeKey(meterKey)
	if meterKey == "" {
		return fmt.Errorf("meter_key required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := loadUsageMeterForRateCard(ctx, tx, tid.UUID(), meterKey); err != nil {
			return err
		}
		queries := gen.New(tx)
		state, err := queries.GetDefaultUsageRateCardDeleteState(
			ctx,
			gen.GetDefaultUsageRateCardDeleteStateParams{
				MerchantID: tid.UUID(),
				MeterKey:   meterKey,
			},
		)
		if err != nil {
			return fmt.Errorf("load default usage rate card: %w", err)
		}
		if !state.DefaultExists {
			return ErrDefaultRateCardNotFound
		}
		if state.OverrideCount > 0 {
			return ErrRateCardHasOverrides
		}
		dependencyCurrencies, err := queries.GetUsageRateCardAllowanceDependencyCurrencies(
			ctx,
			gen.GetUsageRateCardAllowanceDependencyCurrenciesParams{
				MerchantID: tid.UUID(),
				MeterKey:   meterKey,
			},
		)
		if err != nil {
			return fmt.Errorf("load allowance dependencies: %w", err)
		}
		if len(dependencyCurrencies) > 0 {
			return ErrAllowanceSourceInUse
		}
		if err := queries.DeleteDefaultUsageRateCard(ctx, gen.DeleteDefaultUsageRateCardParams{
			MerchantID: tid.UUID(),
			MeterKey:   meterKey,
		}); err != nil {
			return fmt.Errorf("delete default usage rate card: %w", err)
		}
		return nil
	})
}

// ListRateOverrides returns one keyset page of customers' rate overrides,
// by customer then meter: of one customer and one meter when given.
func (s *MoneyService) ListRateOverrides(ctx context.Context, payer *identity.CustomerID, meterKey string, page billing.PageRequest) (billing.ListPage[billing.RateOverride], error) {
	var out billing.ListPage[billing.RateOverride]
	if s == nil || s.db == nil {
		return out, fmt.Errorf("money service not initialized")
	}
	limit, err := pagination.Limit(page)
	if err != nil {
		return out, err
	}
	var position struct {
		Customer uuid.UUID `json:"c"`
		Meter    string    `json:"m"`
	}
	params := gen.ListRateOverridesParams{FetchLimit: pagination.Fetch(limit)}
	if present, err := pagination.Decode(page.Cursor, &position); err != nil {
		return out, err
	} else if present {
		if position.Customer == uuid.Nil {
			return out, pagination.ErrInvalidCursor
		}
		params.AfterCustomer, params.AfterKey = &position.Customer, &position.Meter
	}
	if payer != nil {
		id := payer.UUID()
		params.CustomerID = &id
	}
	if meterKey = catalogrules.NormalizeKey(meterKey); meterKey != "" {
		params.MeterKey = &meterKey
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return out, err
	}
	params.MerchantID = tid.UUID()
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		rows, err := s.db.Gen(ctx).ListRateOverrides(ctx, params)
		if err != nil {
			return err
		}
		items := make([]billing.RateOverride, 0, len(rows))
		for _, row := range rows {
			item, err := rateOverride(row.CustomerID, row.MeterKey, row.Price, row.Allowance, row.Revision, row.CreatedAt, row.UpdatedAt)
			if err != nil {
				return err
			}
			items = append(items, item)
		}
		out = pagination.Cut(items, limit, func(o billing.RateOverride) any {
			return struct {
				Customer uuid.UUID `json:"c"`
				Meter    string    `json:"m"`
			}{o.CustomerID.UUID(), o.MeterKey}
		})
		return nil
	})
	return out, err
}

// GetPayerRateCard reads a customer's rate override for one meter.
func (s *MoneyService) GetPayerRateCard(ctx context.Context, payer identity.CustomerID, meterKey string) (*billing.RateOverride, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var out billing.RateOverride
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		row, err := s.db.Gen(ctx).GetPayerRateCard(ctx, gen.GetPayerRateCardParams{MerchantID: tid.UUID(), CustomerID: payer.UUID(), MeterKey: meterKey})
		if err != nil {
			return err
		}
		out, err = rateOverride(row.CustomerID, row.MeterKey, row.Price, row.Allowance, row.Revision, row.CreatedAt, row.UpdatedAt)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
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
	meterKey = catalogrules.NormalizeKey(meterKey)
	if meterKey == "" {
		return fmt.Errorf("meter_key required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		return s.db.Gen(ctx).DeletePayerRateCard(ctx, gen.DeletePayerRateCardParams{MerchantID: tid.UUID(), CustomerID: payer.UUID(), MeterKey: meterKey})
	})
}
