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
	"github.com/open-rails/openrails/pkg/merchant"
)

// FinalizeInvoice builds and finalizes the monthly itemized invoice for
// (payer, currency) over [from, to) (issue #303/#474). Line items are rolled up
// from openrails.usage_events; money movements + totals from the money ledger in
// that currency; both snapshotted on the immutable finalized invoice. Idempotent:
// re-finalizing the same (period, currency) returns the existing invoice. currency
// "" defaults to USD; billing is external-currency-only (#474 invariant).
//
// The invoice is a STATEMENT. For prepaid it is informational (usage was drawn
// from balance); for arrears the owed_accrued total is what the #301 sweep
// settles via the existing charge path. No charge is initiated here.
func (s *MoneyService) FinalizeInvoice(ctx context.Context, payer identity.MerchantSubjectID, currency string, from, to time.Time) (*models.Invoice, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	if !to.After(from) {
		return nil, fmt.Errorf("invalid period: to must be after from")
	}
	// #474 invariant: invoices are external-currency-only (reject custom credits).
	cur := normalizeCurrency(currency)
	if err := RequireBillingCurrency(cur); err != nil {
		return nil, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	// Materialize the payable tenant_subjects row so the invoices FK (migration
	// 076) is satisfied even if no prior money op touched this subject (#317).
	if err := ensureMerchantSubject(ctx, s.db.Gen(ctx), tid.UUID(), payer.UUID()); err != nil {
		return nil, err
	}

	var inv *models.Invoice
	err = s.db.TenantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		tenantID := tid.UUID()
		payerID := payer.UUID()
		pfrom, pto := from.UTC(), to.UTC()

		// Idempotency: one invoice per (payer, period, currency).
		existing, gerr := q.GetInvoiceByPeriod(ctx, gen.GetInvoiceByPeriodParams{
			MerchantID: tenantID, MerchantSubjectID: payerID,
			PeriodFrom: pfrom, PeriodTo: pto, Currency: cur,
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
			MerchantID: tenantID, MerchantSubjectID: payerID,
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
			MerchantID: tenantID, MerchantSubjectID: payerID,
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
			MerchantID: tenantID, MerchantSubjectID: payerID, Currency: cur,
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
			MerchantID: tenantID, MerchantSubjectID: payerID, Currency: cur,
		})
		if balErr == nil {
			closing = bal.Balance
		} else if !errors.Is(balErr, pgx.ErrNoRows) {
			return balErr
		}

		now := s.now()
		inv = &models.Invoice{
			ID:              uuidutil.NewV7(),
			MerchantID:        tenantID,
			MerchantSubjectID: payerID,
			Currency:        cur, // amounts are minor units of this currency (#474)
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
			MerchantID:        inv.MerchantID,
			MerchantSubjectID: inv.MerchantSubjectID,
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
// RLS-scoped to the request tenant via Qx(ctx), mirroring GetTransactionsByMerchantSubject.
// Returns the page plus the total count for pagination.
func (s *MoneyService) ListInvoices(ctx context.Context, payer identity.MerchantSubjectID, limit, offset int) ([]models.Invoice, int, error) {
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
		tid, terr := merchant.Require(ctx)
		if terr != nil {
			return terr
		}
		tenantID := tid.UUID()
		n, e := q.CountInvoicesByPayer(ctx, gen.CountInvoicesByPayerParams{
			MerchantID: tenantID, MerchantSubjectID: payer.UUID(),
		})
		if e != nil {
			return e
		}
		total = int(n)
		limit32, _ := safecast.Convert[int32](limit)
		offset32, _ := safecast.Convert[int32](offset)
		rows, e := q.ListInvoicesByPayer(ctx, gen.ListInvoicesByPayerParams{
			MerchantID: tenantID, MerchantSubjectID: payer.UUID(),
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
func (s *MoneyService) GetInvoiceByID(ctx context.Context, payer identity.MerchantSubjectID, id uuid.UUID) (*models.Invoice, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	var inv *models.Invoice
	err := s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		tid, terr := merchant.Require(ctx)
		if terr != nil {
			return terr
		}
		row, e := s.db.Gen(ctx).GetInvoiceForPayer(ctx, gen.GetInvoiceForPayerParams{
			MerchantID:        tid.UUID(),
			MerchantSubjectID: payer.UUID(),
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

// FinalizeDueInvoices finalizes the [from, to) invoice for every (payer, currency)
// pair with money activity in the request tenant. Idempotent per pair. Returns the
// number of invoices finalized/returned. Invoices are denominated per currency
// (#474), so a payer with both USD and USDC balances gets one invoice each.
func (s *MoneyService) FinalizeDueInvoices(ctx context.Context, from, to time.Time) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("money service not initialized")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	pairs, err := s.db.Gen(ctx).ListMoneyAccountPairs(ctx, tid.UUID())
	if err != nil {
		return 0, err
	}
	count := 0
	for _, p := range pairs {
		// Custom-credit balances (#475 qualified codes) are not billed — skip them.
		if RequireBillingCurrency(normalizeCurrency(p.Currency)) != nil {
			continue
		}
		if _, err := s.FinalizeInvoice(ctx, identity.MerchantSubjectID(p.MerchantSubjectID), p.Currency, from, to); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}
