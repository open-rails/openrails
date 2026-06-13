package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/tenant"
)

// FinalizeInvoice builds and finalizes the monthly itemized invoice for
// (payer, credit_type) over [from, to) (issue #303). Line items are rolled up
// from openrails.usage_events; money movements + totals from the credit ledger;
// both snapshotted on the immutable finalized invoice. Idempotent: re-finalizing
// the same period returns the existing invoice.
//
// The invoice is a STATEMENT. For prepaid it is informational (usage was drawn
// from balance); for arrears the owed_accrued total is what the #301 sweep
// settles via the existing charge path. No charge is initiated here.
func (s *MoneyService) FinalizeInvoice(ctx context.Context, payer identity.TenantSubjectID, from, to time.Time) (*models.Invoice, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	if !to.After(from) {
		return nil, fmt.Errorf("invalid period: to must be after from")
	}
	tid, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	// Materialize the payable tenant_subjects row so the invoices FK (migration
	// 076) is satisfied even if no prior money op touched this subject (#317).
	if err := ensureTenantSubject(ctx, s.db.Gen(ctx), tid.UUID(), payer.UUID()); err != nil {
		return nil, err
	}

	var inv *models.Invoice
	err = s.db.TenantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		tenantID := tid.UUID()
		payerID := payer.UUID()
		pfrom, pto := from.UTC(), to.UTC()

		// Idempotency: one invoice per (payer, period).
		existing, gerr := q.GetInvoiceByPeriod(ctx, gen.GetInvoiceByPeriodParams{
			TenantID: tenantID, TenantSubjectID: payerID,
			PeriodFrom: pfrom, PeriodTo: pto,
		})
		if gerr == nil {
			inv, gerr = invoiceFromGen(existing)
			return gerr
		}
		if !errors.Is(gerr, pgx.ErrNoRows) {
			return gerr
		}

		// --- usage line items (per event_type) ---
		totals, terr := q.AggregateUsageTotals(ctx, gen.AggregateUsageTotalsParams{
			TenantID: tenantID, TenantSubjectID: payerID,
			FromAt: pfrom, ToAt: pto,
		})
		if terr != nil {
			return terr
		}
		items := map[string]*models.InvoiceLineItem{}
		for _, t := range totals {
			items[t.EventType] = &models.InvoiceLineItem{EventType: t.EventType, Amount: t.TotalAmount, Count: t.EventCount, Dimensions: map[string]int64{}}
		}

		dims, derr := q.AggregateUsageDimensions(ctx, gen.AggregateUsageDimensionsParams{
			TenantID: tenantID, TenantSubjectID: payerID,
			FromAt: pfrom, ToAt: pto,
		})
		if derr != nil {
			return derr
		}
		for _, d := range dims {
			if it, ok := items[d.EventType]; ok {
				it.Dimensions[d.Key] = d.Total
			}
		}

		lineItems := make([]models.InvoiceLineItem, 0, len(items))
		var usageTotal int64
		for _, it := range items {
			usageTotal += it.Amount
			lineItems = append(lineItems, *it)
		}

		// --- money movements (ledger, by transaction_type) ---
		movs, merr := q.SumMoneyMovementsInPeriodByPayer(ctx, gen.SumMoneyMovementsInPeriodByPayerParams{
			TenantID: tenantID, TenantSubjectID: payerID, Currency: DefaultCurrency,
			PeriodFrom: pfrom, PeriodTo: pto,
		})
		if merr != nil {
			return merr
		}
		movements := map[string]int64{}
		for _, m := range movs {
			movements[m.TransactionType] = m.Total
		}

		// --- closing balance snapshot ---
		var closing int64
		bal, balErr := q.GetMoneyBalance(ctx, gen.GetMoneyBalanceParams{
			TenantID: tenantID, TenantSubjectID: payerID, Currency: DefaultCurrency,
		})
		if balErr == nil {
			closing = bal.Balance
		} else if !errors.Is(balErr, pgx.ErrNoRows) {
			return balErr
		}

		now := s.now()
		inv = &models.Invoice{
			ID:              uuidutil.NewV7(),
			TenantID:        tenantID,
			TenantSubjectID: payerID,
			Currency:        "usd", // µ$ = 1e-6 USD
			PeriodFrom:      pfrom,
			PeriodTo:        pto,
			UsageTotal:      usageTotal,
			DepositsTotal:   movements["deposit"],
			OwedAccrued:     movements[txOwedAccrual],
			OwedPaid:        -movements[txOwedPayment], // owed_payment amounts are negative
			ClosingBalance:  closing,
			LineItems:       lineItems,
			MoneyMovements:  movements,
			Status:          "finalized",
			FinalizedAt:     &now,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		lineItemsJSON, jerr := json.Marshal(inv.LineItems)
		if jerr != nil {
			return fmt.Errorf("money: encode invoice line_items: %w", jerr)
		}
		movementsJSON, jerr := toJSONBC(inv.MoneyMovements)
		if jerr != nil {
			return jerr
		}
		return q.InsertInvoice(ctx, gen.InsertInvoiceParams{
			ID:              inv.ID,
			TenantID:        inv.TenantID,
			TenantSubjectID: inv.TenantSubjectID,
			Currency:        inv.Currency,
			PeriodFrom:      inv.PeriodFrom,
			PeriodTo:        inv.PeriodTo,
			UsageTotal:      inv.UsageTotal,
			DepositsTotal:   inv.DepositsTotal,
			OwedAccrued:     inv.OwedAccrued,
			OwedPaid:        inv.OwedPaid,
			ClosingBalance:  inv.ClosingBalance,
			LineItems:       lineItemsJSON,
			MoneyMovements:  movementsJSON,
			Status:          inv.Status,
			FinalizedAt:     inv.FinalizedAt,
			CreatedAt:       inv.CreatedAt,
			UpdatedAt:       inv.UpdatedAt,
		})
	})
	if err != nil {
		return nil, err
	}
	return inv, nil
}

// ListInvoices lists an tenant subject's finalized invoices, newest period first,
// paginated (issue #303). It filters tenant_subject_id directly (the payer) and is
// RLS-scoped to the request tenant via Qx(ctx), mirroring GetTransactionsByTenantSubject.
// Returns the page plus the total count for pagination.
func (s *MoneyService) ListInvoices(ctx context.Context, payer identity.TenantSubjectID, limit, offset int) ([]models.Invoice, int, error) {
	if s == nil || s.db == nil {
		return nil, 0, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return nil, 0, fmt.Errorf("payer required")
	}
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var items []models.Invoice
	var total int
	err := s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		q := s.db.Gen(ctx)
		tid, terr := tenant.Require(ctx)
		if terr != nil {
			return terr
		}
		tenantID := tid.UUID()
		n, e := q.CountInvoicesByPayer(ctx, gen.CountInvoicesByPayerParams{
			TenantID: tenantID, TenantSubjectID: payer.UUID(),
		})
		if e != nil {
			return e
		}
		total = int(n)
		limit32, _ := safecast.Convert[int32](limit)
		offset32, _ := safecast.Convert[int32](offset)
		rows, e := q.ListInvoicesByPayer(ctx, gen.ListInvoicesByPayerParams{
			TenantID: tenantID, TenantSubjectID: payer.UUID(),
			Column3: limit32, Column4: offset32,
		})
		if e != nil {
			return e
		}
		items = make([]models.Invoice, 0, len(rows))
		for _, r := range rows {
			m, merr := invoiceFromGen(r)
			if merr != nil {
				return merr
			}
			items = append(items, *m)
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// GetInvoiceByID returns one finalized invoice (with its snapshotted line items)
// for an tenant subject by id (issue #303). It filters tenant + payer + id and is
// RLS-scoped via Qx(ctx); an invoice belonging to another payer/tenant is
// unreachable (fail closed, pgx.ErrNoRows).
func (s *MoneyService) GetInvoiceByID(ctx context.Context, payer identity.TenantSubjectID, id uuid.UUID) (*models.Invoice, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	var inv *models.Invoice
	err := s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		tid, terr := tenant.Require(ctx)
		if terr != nil {
			return terr
		}
		row, e := s.db.Gen(ctx).GetInvoiceForPayer(ctx, gen.GetInvoiceForPayerParams{
			TenantID:        tid.UUID(),
			TenantSubjectID: payer.UUID(),
			ID:              id,
		})
		if e != nil {
			return e
		}
		inv, e = invoiceFromGen(row)
		return e
	})
	if err != nil {
		return nil, err
	}
	return inv, nil
}

// FinalizeDueInvoices finalizes the [from, to) invoice for every known payer
// (every account with a balance row) in the request tenant. Idempotent per
// account. Returns the number of invoices finalized/returned.
//
// TODO(#472 schema cleanup): there is no ListMoneyAccountPairs gen query yet (the
// credit-era ListCreditAccountPairs enumerated payers by credit_type and is gone
// from the money domain). The per-payer enumerator query must be added to
// internal/db/queries/money_accounts.sql before this can finalize all accounts;
// until then it is a no-op. FinalizeInvoice(payer, …) works per-payer.
func (s *MoneyService) FinalizeDueInvoices(ctx context.Context, from, to time.Time) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("money service not initialized")
	}
	if _, err := tenant.Require(ctx); err != nil {
		return 0, err
	}
	return 0, nil
}
