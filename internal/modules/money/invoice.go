package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

// FinalizeInvoice builds the period invoice for (payer, currency) over [from,
// to). Line items are rolled up from openrails.usage_events; money movements and
// totals come from the money ledger; both are snapshotted on the invoice.
// Idempotent: re-finalizing the same (period, currency) returns the existing
// invoice. Arrears invoices with owed accrual become open receivables; prepaid
// / zero-due invoices are marked paid informational statements.
func (s *MoneyService) FinalizeInvoice(ctx context.Context, payer identity.CustomerID, currency string, from, to time.Time) (*models.Invoice, error) {
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
	// Materialize the payable customers row so the invoices FK (migration
	// 076) is satisfied even if no prior money op touched this subject (#317).
	if err := ensureCustomer(ctx, s.db.Gen(ctx), tid.UUID(), payer.UUID()); err != nil {
		return nil, err
	}

	// #615: rate reported usage (openrails.usage_events) into pending owed invoice
	// items via the catalog metered sidecars the payer reported usage for in this
	// period, BEFORE the finalize transaction rolls pending items onto the invoice.
	// Runs in its own transactions (AccrueOwed) so the committed owed accruals are
	// visible to the finalize tx below; idempotent on a deterministic period
	// source_id, so a re-finalize accrues nothing new.
	if err := s.sweepCatalogMeteredUsage(ctx, payer, cur, from.UTC(), to.UTC()); err != nil {
		return nil, err
	}

	var inv *models.Invoice
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		tenantID := tid.UUID()
		payerID := payer.UUID()
		pfrom, pto := from.UTC(), to.UTC()

		// Idempotency: one invoice per (payer, period, currency).
		existing, gerr := q.GetInvoiceByPeriod(ctx, gen.GetInvoiceByPeriodParams{
			MerchantID: tenantID, CustomerID: payerID,
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
			MerchantID: tenantID, CustomerID: payerID,
			Currency: cur,
			FromAt:   pfrom, ToAt: pto,
		})
		if terr != nil {
			return terr
		}
		items := map[string]*models.InvoiceLineItem{}
		for _, t := range totals {
			items[t.EventType] = &models.InvoiceLineItem{EventType: t.EventType, Amount: t.TotalAmount, Count: t.EventCount, Dimensions: map[string]int64{}}
		}

		dims, derr := q.AggregateUsageDimensions(ctx, gen.AggregateUsageDimensionsParams{
			MerchantID: tenantID, CustomerID: payerID,
			Currency: cur,
			FromAt:   pfrom, ToAt: pto,
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

		// --- money movements (#512 ledger, by transfer_type; amounts positive) ---
		movs, merr := q.SumLedgerMovementsByCustomerInPeriod(ctx, gen.SumLedgerMovementsByCustomerInPeriodParams{
			MerchantID: tenantID, CustomerID: payerID, Currency: cur,
			PeriodFrom: pfrom, PeriodTo: pto,
		})
		if merr != nil {
			return merr
		}
		// Normalize ledger transfer types (all positive amounts) into the statement's
		// signed movement map: money in is positive, money out negative — matching the
		// retired single-entry money_transactions sign convention.
		movements := map[string]int64{}
		for _, m := range movs {
			switch m.TransferType {
			case "deposit":
				movements["deposit"] += m.Total
			case "credit_spend", "spend", "capture":
				movements["withdrawal"] -= m.Total
			case "owed_accrual":
				movements[txOwedAccrual] += m.Total
			case "owed_payment":
				movements[txOwedPayment] -= m.Total
			case "credit_expire", "expire":
				movements["expiry"] -= m.Total
			default:
				movements[m.TransferType] += m.Total
			}
		}
		pendingReceivable, perr := q.SumPendingInvoiceItemAmountInPeriod(ctx, gen.SumPendingInvoiceItemAmountInPeriodParams{
			MerchantID: tenantID,
			CustomerID: payerID,
			Currency:   cur,
			PeriodFrom: pfrom,
			PeriodTo:   pto,
		})
		if perr != nil {
			return perr
		}

		// --- closing balance snapshot (derived, #491) ---
		bal, balErr := s.deriveBalance(ctx, q, tenantID, payerID, cur)
		if balErr != nil {
			return balErr
		}
		closing := bal.Balance

		now := s.now()
		invoiceID := uuidutil.NewV7()
		invoiceNumber := fmt.Sprintf("INV-%s", invoiceID.String())
		totalAmount := pendingReceivable
		amountPaid := int64(0)
		amountDue := totalAmount
		status := "open"
		issuedAt := &now
		dueAt := &now
		var paidAt *time.Time
		if amountDue == 0 {
			status = "paid"
			totalAmount = usageTotal
			amountPaid = usageTotal
			paidAt = &now
		}
		inv = &models.Invoice{
			ID:               invoiceID,
			MerchantID:       tenantID,
			CustomerID:       payerID,
			Currency:         cur, // amounts are minor units of this currency (#474)
			InvoiceNumber:    &invoiceNumber,
			PeriodFrom:       pfrom,
			PeriodTo:         pto,
			UsageTotal:       usageTotal,
			DepositsTotal:    movements["deposit"],
			OwedAccrued:      movements[txOwedAccrual],
			OwedPaid:         -movements[txOwedPayment], // owed_payment is stored negative in the map
			ClosingBalance:   closing,
			SubtotalAmount:   totalAmount,
			TotalAmount:      totalAmount,
			AmountPaid:       amountPaid,
			AmountDue:        amountDue,
			LineItems:        lineItems,
			MoneyMovements:   movements,
			Status:           status,
			CollectionMethod: "charge_automatically",
			IssuedAt:         issuedAt,
			DueAt:            dueAt,
			PaidAt:           paidAt,
			FinalizedAt:      &now,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		lineItemsJSON, jerr := json.Marshal(inv.LineItems)
		if jerr != nil {
			return fmt.Errorf("money: encode invoice line_items: %w", jerr)
		}
		movementsJSON, jerr := toJSONBC(inv.MoneyMovements)
		if jerr != nil {
			return jerr
		}
		if err := q.InsertInvoice(ctx, gen.InsertInvoiceParams{
			ID:               inv.ID,
			MerchantID:       inv.MerchantID,
			CustomerID:       inv.CustomerID,
			Currency:         inv.Currency,
			InvoiceNumber:    inv.InvoiceNumber,
			PeriodFrom:       inv.PeriodFrom,
			PeriodTo:         inv.PeriodTo,
			UsageTotal:       inv.UsageTotal,
			DepositsTotal:    inv.DepositsTotal,
			OwedAccrued:      inv.OwedAccrued,
			OwedPaid:         inv.OwedPaid,
			ClosingBalance:   inv.ClosingBalance,
			SubtotalAmount:   inv.SubtotalAmount,
			TotalAmount:      inv.TotalAmount,
			AmountPaid:       inv.AmountPaid,
			AmountDue:        inv.AmountDue,
			LineItems:        lineItemsJSON,
			MoneyMovements:   movementsJSON,
			Status:           inv.Status,
			CollectionMethod: inv.CollectionMethod,
			IssuedAt:         inv.IssuedAt,
			DueAt:            inv.DueAt,
			PaidAt:           inv.PaidAt,
			FinalizedAt:      inv.FinalizedAt,
			CreatedAt:        inv.CreatedAt,
			UpdatedAt:        inv.UpdatedAt,
		}); err != nil {
			return err
		}
		attached, err := q.AttachPendingInvoiceItemsToInvoice(ctx, gen.AttachPendingInvoiceItemsToInvoiceParams{
			MerchantID: inv.MerchantID,
			CustomerID: inv.CustomerID,
			InvoiceID:  &inv.ID,
			Now:        now,
			Currency:   inv.Currency,
			PeriodFrom: inv.PeriodFrom,
			PeriodTo:   inv.PeriodTo,
		})
		if err != nil {
			return err
		}
		if attached == 0 {
			if err := insertInvoiceItemsFromRollup(ctx, q, inv, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return inv, nil
}

func insertInvoiceItemsFromRollup(ctx context.Context, q *gen.Queries, inv *models.Invoice, now time.Time) error {
	for _, item := range inv.LineItems {
		metadata, err := json.Marshal(map[string]any{"dimensions": item.Dimensions})
		if err != nil {
			return fmt.Errorf("money: encode invoice item metadata: %w", err)
		}
		eventType := item.EventType
		sourceID := fmt.Sprintf("%s:%s", inv.ID.String(), item.EventType)
		quantity := item.Count
		if quantity <= 0 {
			quantity = 1
		}
		unitAmount := int64(0)
		if item.Count > 0 {
			unitAmount = item.Amount / item.Count
		}
		if err := q.InsertInvoiceItem(ctx, gen.InsertInvoiceItemParams{
			ID:         uuidutil.NewV7(),
			MerchantID: inv.MerchantID,
			CustomerID: inv.CustomerID,
			Currency:   inv.Currency,
			InvoiceID:  &inv.ID,
			SourceType: "usage_rollup",
			SourceID:   sourceID,
			EventType:  &eventType,
			PeriodFrom: inv.PeriodFrom,
			PeriodTo:   inv.PeriodTo,
			InvoiceAt:  now,
			Quantity:   quantity,
			UnitAmount: unitAmount,
			Amount:     item.Amount,
			Status:     "invoiced",
			Metadata:   metadata,
			CreatedAt:  now,
			UpdatedAt:  now,
		}); err != nil {
			return err
		}
	}
	return nil
}

// ListInvoices lists an merchant subject's finalized invoices, newest period first,
// paginated (issue #303). It filters customer_id directly (the payer) and is
// RLS-scoped to the request merchant via Qx(ctx), mirroring GetTransactionsByCustomer.
// Returns the page plus the total count for pagination.
func (s *MoneyService) ListInvoices(ctx context.Context, payer identity.CustomerID, limit, offset int) ([]models.Invoice, int, error) {
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
	err := s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		q := s.db.Gen(ctx)
		tid, terr := merchant.Require(ctx)
		if terr != nil {
			return terr
		}
		tenantID := tid.UUID()
		n, e := q.CountInvoicesByPayer(ctx, gen.CountInvoicesByPayerParams{
			MerchantID: tenantID, CustomerID: payer.UUID(),
		})
		if e != nil {
			return e
		}
		total = int(n)
		limit32, _ := safecast.Convert[int32](limit)
		offset32, _ := safecast.Convert[int32](offset)
		rows, e := q.ListInvoicesByPayer(ctx, gen.ListInvoicesByPayerParams{
			MerchantID: tenantID, CustomerID: payer.UUID(),
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
// for an merchant subject by id (issue #303). It filters merchant + payer + id and is
// RLS-scoped via Qx(ctx); an invoice belonging to another payer/merchant is
// unreachable (fail closed, pgx.ErrNoRows).
func (s *MoneyService) GetInvoiceByID(ctx context.Context, payer identity.CustomerID, id uuid.UUID) (*models.Invoice, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	var inv *models.Invoice
	err := s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		tid, terr := merchant.Require(ctx)
		if terr != nil {
			return terr
		}
		row, e := s.db.Gen(ctx).GetInvoiceForPayer(ctx, gen.GetInvoiceForPayerParams{
			MerchantID: tid.UUID(),
			CustomerID: payer.UUID(),
			ID:         id,
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

func (s *MoneyService) VoidInvoice(ctx context.Context, payer identity.CustomerID, id uuid.UUID) (*models.Invoice, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	var inv *models.Invoice
	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		tid, terr := merchant.Require(ctx)
		if terr != nil {
			return terr
		}
		if _, e := q.GetInvoiceForPayer(ctx, gen.GetInvoiceForPayerParams{
			MerchantID: tid.UUID(),
			CustomerID: payer.UUID(),
			ID:         id,
		}); e != nil {
			return e
		}
		row, e := q.VoidInvoiceForPayer(ctx, gen.VoidInvoiceForPayerParams{
			MerchantID: tid.UUID(),
			CustomerID: payer.UUID(),
			InvoiceID:  id,
			Now:        s.now(),
		})
		if e != nil {
			return e
		}
		var merr error
		inv, merr = invoiceFromGen(row)
		return merr
	})
	if err != nil {
		return nil, err
	}
	return inv, nil
}

func (s *MoneyService) MarkInvoiceUncollectible(ctx context.Context, payer identity.CustomerID, id uuid.UUID) (*models.Invoice, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	var inv *models.Invoice
	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		tid, terr := merchant.Require(ctx)
		if terr != nil {
			return terr
		}
		row, e := gen.New(tx).MarkInvoiceUncollectibleForPayer(ctx, gen.MarkInvoiceUncollectibleForPayerParams{
			MerchantID: tid.UUID(),
			CustomerID: payer.UUID(),
			InvoiceID:  id,
			Now:        s.now(),
		})
		if e != nil {
			return e
		}
		var merr error
		inv, merr = invoiceFromGen(row)
		return merr
	})
	if err != nil {
		return nil, err
	}
	return inv, nil
}

func (s *MoneyService) RecordOutOfBandInvoicePayment(ctx context.Context, payer identity.CustomerID, id uuid.UUID, amount int64, railPaymentID string) (*models.Invoice, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	if id == uuid.Nil {
		return nil, fmt.Errorf("invoice_id required")
	}
	if amount <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}
	railPaymentID = strings.TrimSpace(railPaymentID)
	if railPaymentID == "" {
		return nil, fmt.Errorf("rail_payment_id required")
	}
	var inv *models.Invoice
	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		tid, terr := merchant.Require(ctx)
		if terr != nil {
			return terr
		}
		invoiceRow, e := q.GetInvoiceForPayerForUpdate(ctx, gen.GetInvoiceForPayerForUpdateParams{
			MerchantID: tid.UUID(),
			CustomerID: payer.UUID(),
			ID:         id,
		})
		if e != nil {
			return e
		}
		if invoiceRow.Status != "open" && invoiceRow.Status != "past_due" {
			return fmt.Errorf("invoice is not payable")
		}
		if amount > invoiceRow.AmountDue {
			return fmt.Errorf("payment amount exceeds invoice amount_due")
		}
		now := s.now()
		sourceID := railPaymentID
		// Reference dedup: a prior owed-payment transfer with this reference means
		// the manual payment was already applied (the single-entry uniqueness this
		// replaced lived on money_transactions).
		if _, derr := q.GetLedgerTransferByCoords(ctx, gen.GetLedgerTransferByCoordsParams{
			MerchantID: tid.UUID(), CustomerID: payer.UUID(), Currency: normalizeCurrency(invoiceRow.Currency),
			TransferType: "owed_payment", Source: "manual_invoice_payment", SourceID: sourceID,
		}); derr == nil {
			return fmt.Errorf("manual payment reference %q already applied", sourceID)
		} else if !errors.Is(derr, pgx.ErrNoRows) {
			return derr
		}
		n, e := q.ApplyInvoicePaymentSnapshot(ctx, gen.ApplyInvoicePaymentSnapshotParams{
			MerchantID: tid.UUID(),
			CustomerID: payer.UUID(),
			InvoiceID:  id,
			Snapshot:   amount,
			Now:        now,
		})
		if e != nil {
			return e
		}
		if n == 0 {
			return fmt.Errorf("invoice payment was not applied")
		}
		// Settle the arrears liability via a #512 ledger owed-payment transfer
		// (DR processor_clearing / CR arrears_liability).
		ml := s.moneyLedger(q, tid.UUID())
		tr, e := ml.PayOwed(ctx, payer.UUID(), normalizeCurrency(invoiceRow.Currency), amount, "manual_invoice_payment", sourceID, &id)
		if e != nil {
			return e
		}
		rail := string(models.RailManual)
		if e := q.InsertInvoicePayment(ctx, gen.InsertInvoicePaymentParams{
			ID:                 uuidutil.NewV7(),
			MerchantID:         tid.UUID(),
			CustomerID:         payer.UUID(),
			InvoiceID:          id,
			MoneyTransactionID: &tr.ID,
			Currency:           invoiceRow.Currency,
			Amount:             amount,
			Status:             "settled",
			Rail:               &rail,
			RailPaymentID:      &railPaymentID,
			AttemptedAt:        now,
			SettledAt:          &now,
			CreatedAt:          now,
			UpdatedAt:          now,
		}); e != nil {
			return e
		}
		row, e := q.GetInvoiceForPayer(ctx, gen.GetInvoiceForPayerParams{
			MerchantID: tid.UUID(),
			CustomerID: payer.UUID(),
			ID:         id,
		})
		if e != nil {
			return e
		}
		var merr error
		inv, merr = invoiceFromGen(row)
		return merr
	})
	if err != nil {
		return nil, err
	}
	return inv, nil
}

// FinalizeDueInvoices finalizes the [from, to) invoice for every (payer, currency)
// pair with money activity in the request merchant. Idempotent per pair. Returns the
// number of invoices finalized/returned. Invoices are denominated per fiat
// account currency (#474), so a payer with both USD and EUR balances gets one
// invoice each.
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
		if _, err := s.FinalizeInvoice(ctx, identity.CustomerID(p.CustomerID), p.Currency, from, to); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func (s *MoneyService) FinalizeDueInvoicesForBoundary(ctx context.Context, boundary string, now time.Time) (int, error) {
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
	pairs, err := s.db.Gen(ctx).ListMoneyAccountPairs(ctx, tid.UUID())
	if err != nil {
		return 0, err
	}
	count := 0
	for _, p := range pairs {
		if RequireBillingCurrency(normalizeCurrency(p.Currency)) != nil {
			continue
		}
		from, to, err := PreviousInvoicePeriod(now, p.PeriodAnchor, boundary)
		if err != nil {
			return count, err
		}
		if _, err := s.FinalizeInvoice(ctx, identity.CustomerID(p.CustomerID), p.Currency, from, to); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// FinalizeThresholdInvoices finalizes open receivables for arrears customers
// whose pending invoice items plus open invoice balances have reached their
// configured credit line. Collection is intentionally left to ChargeOutstanding
// so provider calls stay out of the invoice-finalization transaction.
func (s *MoneyService) FinalizeThresholdInvoices(ctx context.Context, cutoff time.Time, opts ...InvoiceThresholdOptions) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("money service not initialized")
	}
	if cutoff.IsZero() {
		cutoff = s.now()
	}
	cutoff = cutoff.UTC()
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	var opt InvoiceThresholdOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	rows, err := s.db.Gen(ctx).ListInvoiceThresholdCandidates(ctx, gen.ListInvoiceThresholdCandidatesParams{
		MerchantID:   tid.UUID(),
		Cutoff:       cutoff,
		MinThreshold: opt.CollectionThresholdAmount,
	})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, r := range rows {
		from := r.PeriodFrom
		if opt.BillingPeriodBoundary != "" {
			start, err := CurrentInvoicePeriodStart(cutoff, r.PeriodAnchor, opt.BillingPeriodBoundary)
			if err != nil {
				return count, err
			}
			from = start
		}
		if !cutoff.After(from) {
			continue
		}
		if _, err := s.FinalizeInvoice(ctx, identity.CustomerID(r.CustomerID), r.Currency, from, cutoff); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}
