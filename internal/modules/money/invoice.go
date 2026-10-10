package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/modules/money/statement"
	"github.com/open-rails/openrails/internal/retention"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// FinalizeInvoice builds the period invoice for (payer, currency) over [from,
// to). Line items are rolled up from billing.usage_events; money movements and
// totals come from the money ledger; both are snapshotted on the invoice.
// Idempotent: re-finalizing the same (period, currency) returns the existing
// invoice. Arrears invoices with owed accrual become open receivables; prepaid
// / zero-due invoices are marked paid informational statements, unless
// threshold invoices bill part of the period: the statement stays open until
// they are paid.
func (s *MoneyService) FinalizeInvoice(ctx context.Context, payer identity.CustomerID, currency string, from, to time.Time) (*models.Invoice, error) {
	return s.finalizeInvoice(ctx, payer, currency, from, to, basisAny)
}

// invoiceBasis is what a period must hold, under the payer lock, for a
// finalization to write its invoice. The scheduled passes choose their payers
// before taking that lock, so the choice is rechecked there.
type invoiceBasis int

const (
	// basisAny writes the statement unconditionally: an explicit finalization.
	basisAny invoiceBasis = iota
	// basisActivity needs usage, a money movement or an uninvoiced item in the
	// period: the period sweep never states a period the payer had no part in.
	basisActivity
	// basisPending needs an uninvoiced item in the period: a threshold pass
	// whose items another pass has invoiced since leaves nothing to bill.
	basisPending
)

// finalizeInvoice is FinalizeInvoice under a basis; a nil invoice and nil
// error mean the period held nothing the basis requires.
func (s *MoneyService) finalizeInvoice(ctx context.Context, payer identity.CustomerID, currency string, from, to time.Time, basis invoiceBasis) (*models.Invoice, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	if !to.After(from) {
		return nil, fmt.Errorf("invalid period: to must be after from")
	}
	// Invoices require a registered currency.
	cur := normalizeCurrency(currency)
	if err := RequireBillingCurrency(cur); err != nil {
		return nil, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	// Materialize the customers row the invoice references, on a
	// merchant-pinned connection.
	if err := s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		return ensureCustomer(ctx, s.db.Gen(ctx), tid.UUID(), payer.UUID())
	}); err != nil {
		return nil, err
	}

	// Rate reported usage into pending owed invoice items via the catalog rate
	// cards before the finalize transaction rolls them onto the invoice. The
	// accruals commit in their own transactions; the rating watermark makes a
	// re-finalize accrue nothing new.
	if err := s.sweepCatalogRateCardUsage(ctx, payer, cur, from.UTC(), to.UTC()); err != nil {
		return nil, err
	}

	var inv *models.Invoice
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		tenantID := tid.UUID()
		payerID := payer.UUID()
		pfrom, pto := from.UTC(), to.UTC()

		// Serialize the period lookup with concurrent finalizers and money
		// writes. A uniqueness failure after both readers see no invoice would
		// abort one replica's merchant pass instead of returning the first bill.
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: tenantID, ID: payerID}); err != nil {
			return fmt.Errorf("lock invoice payer for finalization: %w", err)
		}

		// Idempotency: one invoice per (payer, period, currency).
		existing, gerr := q.GetInvoiceByPeriod(ctx, gen.GetInvoiceByPeriodParams{
			MerchantID: tenantID, CustomerID: payerID,
			PeriodStartsAt: pfrom, PeriodEndsAt: pto, Currency: cur,
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

		// Itemize the rated charge per accrual source (e.g. "metered:<meter>"):
		// the usage rollups above carry quantities but zero amounts.
		ratedRows, rerr := q.SumPendingInvoiceItemAmountBySourceInPeriod(ctx, gen.SumPendingInvoiceItemAmountBySourceInPeriodParams{
			MerchantID: tenantID, CustomerID: payerID, Currency: cur,
			PeriodStartsAt: pfrom, PeriodEndsAt: pto,
		})
		if rerr != nil {
			return rerr
		}
		for _, rr := range ratedRows {
			if rr.Amount > 0 {
				lineItems = append(lineItems, models.InvoiceLineItem{EventType: rr.Source, Amount: rr.Amount, Count: rr.ItemCount})
			}
		}

		// --- money movements (ledger, by transfer_type; amounts positive) ---
		movs, merr := q.SumLedgerMovementsByCustomerInPeriod(ctx, gen.SumLedgerMovementsByCustomerInPeriodParams{
			MerchantID: tenantID, CustomerID: payerID, Currency: cur,
			PeriodStartsAt: pfrom, PeriodEndsAt: pto,
		})
		if merr != nil {
			return merr
		}
		// Normalize ledger transfer types (positive amounts) into the statement's
		// signed movement map: money in positive, money out negative.
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
			case "owed_repayment":
				movements[txOwedRepayment] -= m.Total
			case "credit_expire", "expire":
				movements["expiry"] -= m.Total
			default:
				movements[m.TransferType] += m.Total
			}
		}
		var pendingItems int64
		for _, rr := range ratedRows {
			pendingItems += rr.ItemCount
		}
		switch {
		case basis == basisPending && pendingItems == 0,
			basis == basisActivity && pendingItems == 0 && len(totals) == 0 && len(movs) == 0:
			return nil
		}
		// A statement also states its period's charges that threshold invoices
		// already bill, and is paid only once they are. A threshold invoice
		// states only what it bills.
		var covered int64
		if basis != basisPending {
			period := gen.LockInvoicesBillingPeriodParams{MerchantID: tenantID, CustomerID: payerID, Currency: cur, PeriodStartsAt: pfrom, PeriodEndsAt: pto}
			if _, err := q.LockInvoicesBillingPeriod(ctx, period); err != nil {
				return err
			}
			sum, err := q.SumBilledInvoiceItemAmountInPeriod(ctx, gen.SumBilledInvoiceItemAmountInPeriodParams(period))
			if err != nil {
				return err
			}
			covered = sum
		}
		pendingReceivable, perr := q.SumPendingInvoiceItemAmountInPeriod(ctx, gen.SumPendingInvoiceItemAmountInPeriodParams{
			MerchantID:     tenantID,
			CustomerID:     payerID,
			Currency:       cur,
			PeriodStartsAt: pfrom,
			PeriodEndsAt:   pto,
		})
		if perr != nil {
			return perr
		}

		receivable := pendingReceivable
		// Owed that funding already repaid is not billed again: an invoice
		// claims at most the owed no other invoice claims.
		owed, oerr := s.moneyLedger(q, tenantID).OutstandingOwed(ctx, payerID, cur)
		if oerr != nil {
			return oerr
		}
		claimed, cerr := q.SumOwedInvoiceClaims(ctx, gen.SumOwedInvoiceClaimsParams{MerchantID: tenantID, CustomerID: payerID, Currency: cur})
		if cerr != nil {
			return cerr
		}
		due := min(receivable, max(owed-claimed, 0))

		// --- closing balance snapshot (derived) ---
		bal, balErr := s.deriveBalance(ctx, q, tenantID, payerID, cur)
		if balErr != nil {
			return balErr
		}
		closing := bal.Balance

		// Payer invoice profile: net-N terms, collection method, document
		// snapshot. Without one: due at finalize, charge_automatically, no
		// document fields.
		var profile *CustomerInvoiceProfile
		if row, perr := q.GetInvoiceProfile(ctx, gen.GetInvoiceProfileParams{
			MerchantID: tenantID, CustomerID: payerID,
		}); perr == nil {
			if profile, perr = invoiceProfileFromGen(row); perr != nil {
				return perr
			}
		} else if !errors.Is(perr, pgx.ErrNoRows) {
			return perr
		}

		now := s.now()
		invoiceID := uuidutil.NewV7()
		invoiceNumber := fmt.Sprintf("INV-%s", invoiceID.String())
		totalAmount := receivable + covered
		amountPaid := receivable - due
		amountDue := due
		status := "open"
		issuedAt := &now
		dueAt := &now
		collectionMethod := CollectionChargeAutomatically
		if profile != nil {
			if profile.NetTermsDays > 0 {
				d := now.Add(time.Duration(profile.NetTermsDays) * 24 * time.Hour)
				dueAt = &d
			}
			if m, merr := normalizeCollectionMethod(profile.CollectionMethod); merr == nil {
				collectionMethod = m
			}
		}
		if totalAmount == 0 {
			// Nothing billed: prepaid usage was paid as it was spent.
			totalAmount, amountPaid = usageTotal, usageTotal
		}
		var paidAt *time.Time
		if amountDue == 0 && amountPaid == totalAmount {
			status, paidAt = "paid", &now
		}
		inv = &models.Invoice{
			ID:               invoiceID,
			MerchantID:       tenantID,
			CustomerID:       payerID,
			Currency:         cur, // amounts are native units of this currency
			InvoiceNumber:    &invoiceNumber,
			PeriodStartsAt:   pfrom,
			PeriodEndsAt:     pto,
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
			CollectionMethod: collectionMethod,
			IssuedAt:         issuedAt,
			DueAt:            dueAt,
			PaidAt:           paidAt,
			FinalizedAt:      &now,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		if profile != nil {
			inv.PONumber = nilIfEmpty(profile.PONumber)
			inv.Tax = profile.Tax
			inv.BillingContacts = profile.BillingContacts
			inv.Memo = nilIfEmpty(profile.Memo)
		}
		lineItemsJSON, jerr := json.Marshal(inv.LineItems)
		if jerr != nil {
			return fmt.Errorf("money: encode invoice line_items: %w", jerr)
		}
		movementsJSON, jerr := toJSONBC(inv.MoneyMovements)
		if jerr != nil {
			return jerr
		}
		taxJSON, jerr := toJSONBC(inv.Tax)
		if jerr != nil {
			return jerr
		}
		contactsJSON, jerr := json.Marshal(inv.BillingContacts)
		if jerr != nil {
			return fmt.Errorf("money: encode invoice billing_contacts: %w", jerr)
		}
		if inv.BillingContacts == nil {
			contactsJSON = []byte("[]")
		}
		if err := q.InsertInvoice(ctx, gen.InsertInvoiceParams{
			ID:               inv.ID,
			MerchantID:       inv.MerchantID,
			CustomerID:       inv.CustomerID,
			Currency:         inv.Currency,
			InvoiceNumber:    inv.InvoiceNumber,
			PeriodStartsAt:   inv.PeriodStartsAt,
			PeriodEndsAt:     inv.PeriodEndsAt,
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
			PoNumber:         inv.PONumber,
			Tax:              taxJSON,
			BillingContacts:  contactsJSON,
			Memo:             inv.Memo,
			CreatedAt:        inv.CreatedAt,
			UpdatedAt:        inv.UpdatedAt,
		}); err != nil {
			return err
		}
		// Consume the pending workspace: attached rows keep only their
		// invoice_id/status tombstone so they can't bill twice. The statement
		// itemization is line_items above.
		if _, err := q.AttachPendingInvoiceItemsToInvoice(ctx, gen.AttachPendingInvoiceItemsToInvoiceParams{
			MerchantID:     inv.MerchantID,
			CustomerID:     inv.CustomerID,
			InvoiceID:      &inv.ID,
			Now:            now,
			Currency:       inv.Currency,
			PeriodStartsAt: inv.PeriodStartsAt,
			PeriodEndsAt:   inv.PeriodEndsAt,
		}); err != nil {
			return err
		}
		if inv.AmountDue > 0 {
			number := inv.ID.String()
			if inv.InvoiceNumber != nil && *inv.InvoiceNumber != "" {
				number = *inv.InvoiceNumber
			}
			amountDue := inv.AmountDue
			data, err := json.Marshal(billing.NotificationData{InvoiceID: billing.InvoiceID(inv.ID), InvoiceNumber: number, AmountDue: &amountDue, Currency: inv.Currency, DueAt: inv.DueAt})
			if err != nil {
				return err
			}
			if err := q.CreateNotificationIfAbsent(ctx, gen.CreateNotificationIfAbsentParams{
				ID:         uuidutil.DeterministicID(uuidutil.DeterministicNamespace, "invoice_issued", inv.MerchantID.String(), inv.ID.String()),
				MerchantID: inv.MerchantID, CustomerID: inv.CustomerID,
				EventType: string(models.NotificationInvoiceIssued), Data: data, CreatedAt: now,
			}); err != nil {
				return fmt.Errorf("invoice issued notification: %w", err)
			}
		}
		if covered == 0 {
			return nil
		}
		// What the billing invoices already received counts at once.
		if _, err := q.SettleStatementCoverage(ctx, gen.SettleStatementCoverageParams{MerchantID: tenantID, StatementIds: []uuid.UUID{inv.ID}, Now: now}); err != nil {
			return err
		}
		row, err := q.GetInvoiceForPayer(ctx, gen.GetInvoiceForPayerParams{MerchantID: tenantID, CustomerID: payerID, ID: inv.ID})
		if err != nil {
			return err
		}
		inv, err = invoiceFromGen(row)
		return err
	})
	if err != nil {
		return nil, err
	}
	return inv, nil
}

// GetInvoiceByID returns one finalized invoice with its line items, filtered
// by merchant + payer + id: another payer's invoice is unreachable
// (pgx.ErrNoRows).
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
		// Write off the debt, not just the invoice row: an unreversed accrual
		// would keep capping the payer. Idempotent on the invoice coordinate.
		before, e := q.GetInvoiceForPayer(ctx, gen.GetInvoiceForPayerParams{
			MerchantID: tid.UUID(), CustomerID: payer.UUID(), ID: id,
		})
		if e != nil {
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
		if before.AmountDue > 0 {
			if _, we := s.moneyLedger(q, tid.UUID()).WriteOffOwed(
				ctx, payer.UUID(), normalizeCurrency(before.Currency), before.AmountDue,
				ledger.Coord{Operation: ledger.OpInvoiceVoid, Source: "invoice_void", SourceID: id.String()},
				&id,
			); we != nil {
				return we
			}
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

// InvoiceRemittance is money the merchant received for an invoice outside
// OpenRails. TransactionID is its identity: recording it again with the same
// terms answers the first payment.
type InvoiceRemittance struct {
	InvoiceID     uuid.UUID
	Amount        int64
	TransactionID string
	// PaidAt defaults to now; a replay that omits it does not compare it.
	PaidAt *time.Time
}

// RecordInvoiceRemittance records a remittance as a manual payment on its
// invoice and settles that much of what the invoice claims. created is false
// when the same remittance was already recorded.
func (s *MoneyService) RecordInvoiceRemittance(ctx context.Context, in InvoiceRemittance) (payment *models.Payment, created bool, err error) {
	if s == nil || s.db == nil {
		return nil, false, fmt.Errorf("money service not initialized")
	}
	in.TransactionID = strings.TrimSpace(in.TransactionID)
	if in.InvoiceID == uuid.Nil || in.Amount <= 0 || in.TransactionID == "" {
		return nil, false, fmt.Errorf("invoice, positive amount and transaction id are required")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, false, err
	}
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		invoiceRow, e := q.GetMerchantInvoice(ctx, gen.GetMerchantInvoiceParams{MerchantID: mid.UUID(), ID: in.InvoiceID})
		if e != nil {
			return e
		}
		if _, e := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: invoiceRow.CustomerID}); e != nil {
			return e
		}
		if invoiceRow, e = q.GetInvoiceForPayerForUpdate(ctx, gen.GetInvoiceForPayerForUpdateParams{MerchantID: mid.UUID(), CustomerID: invoiceRow.CustomerID, ID: in.InvoiceID}); e != nil {
			return e
		}
		// A recorded remittance answers its replay before paid status, a
		// reduced due amount or a later collection could refuse it.
		prior, e := q.GetPaymentByPSPTransactionID(ctx, gen.GetPaymentByPSPTransactionIDParams{MerchantID: mid.UUID(), Channel: string(models.ChannelManual), TransactionID: in.TransactionID})
		switch {
		case e == nil:
			if prior.InvoiceID == nil || *prior.InvoiceID != in.InvoiceID || prior.Amount != in.Amount || in.PaidAt != nil && !prior.PurchasedAt.Equal(in.PaidAt.UTC().Truncate(time.Microsecond)) {
				return billing.ErrIdempotencyKeyReused
			}
			payment, e = models.PaymentFromGen(prior)
			return e
		case !errors.Is(e, pgx.ErrNoRows):
			return e
		}
		if invoiceRow.Status != "open" || invoiceRow.CollectionIntentID != nil {
			return ErrInvoiceActionNotAllowed
		}
		if in.Amount > invoiceRow.AmountDue {
			return ErrPaymentExceedsDue
		}
		now := s.now()
		paidAt := now
		if in.PaidAt != nil {
			paidAt = in.PaidAt.UTC().Truncate(time.Microsecond)
		}
		n, e := q.ApplyInvoicePaymentSnapshot(ctx, gen.ApplyInvoicePaymentSnapshotParams{MerchantID: mid.UUID(), CustomerID: invoiceRow.CustomerID, InvoiceID: in.InvoiceID, Snapshot: in.Amount, Now: now})
		if e != nil {
			return e
		}
		if n == 0 {
			return fmt.Errorf("invoice payment was not applied")
		}
		if e := statement.Follow(ctx, q, mid.UUID(), invoiceRow.CustomerID, invoiceRow.Currency, in.InvoiceID, now); e != nil {
			return e
		}
		// The owed-payment transfer settles the arrears liability (DR
		// processor_clearing / CR arrears_liability) at the remittance's
		// unique coordinate.
		coord := ledger.Coord{Operation: ledger.OpManualInvoicePay, Source: "manual_invoice_payment", SourceID: in.TransactionID}
		tr, e := s.moneyLedger(q, mid.UUID()).PayOwed(ctx, invoiceRow.CustomerID, normalizeCurrency(invoiceRow.Currency), in.Amount, coord, &in.InvoiceID)
		if e != nil {
			return e
		}
		if tr.Amount != in.Amount || tr.InvoiceID == nil || *tr.InvoiceID != in.InvoiceID {
			return billing.ErrIdempotencyKeyReused
		}
		id := uuidutil.NewV7()
		if e := q.InsertInvoicePayment(ctx, gen.InsertInvoicePaymentParams{ID: id, MerchantID: mid.UUID(), CustomerID: invoiceRow.CustomerID, InvoiceID: in.InvoiceID,
			LedgerTransferID: tr.ID, Channel: string(models.ChannelManual), TransactionID: in.TransactionID, Amount: in.Amount, Currency: invoiceRow.Currency, PaidAt: paidAt, Now: now}); e != nil {
			if db.IsUniqueViolation(e) {
				return billing.ErrIdempotencyKeyReused
			}
			return e
		}
		row, e := q.GetInvoicePayment(ctx, gen.GetInvoicePaymentParams{MerchantID: mid.UUID(), CustomerID: invoiceRow.CustomerID, InvoiceID: in.InvoiceID, ID: id})
		if e != nil {
			return e
		}
		created = true
		payment, e = models.PaymentFromGen(row)
		return e
	})
	if err != nil {
		return nil, false, err
	}
	return payment, created, nil
}

// FinalizeDueInvoicesForBoundary finalizes the previous period of every
// invoice payer active in it under the merchant's billing_period_boundary;
// anniversary periods are anchored on the payer's first recorded activity.
func (s *MoneyService) FinalizeDueInvoicesForBoundary(ctx context.Context, boundary string, now time.Time) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("money service not initialized")
	}
	if now.IsZero() {
		now = s.now()
	}
	// The previous period starts at most two periods back, so three months of
	// activity names every payer it could still have to invoice.
	activeSince := retention.MonthStart(now).AddDate(0, -3, 0)
	return s.finalizeInvoicePayers(ctx, activeSince, func(p gen.ListInvoicePayersRow) (time.Time, time.Time, error) {
		return PreviousInvoicePeriod(now, p.PeriodAnchor, boundary)
	})
}

// finalizeInvoicePayers finalizes every (payer, currency) ListInvoicePayers
// enumerates (ledger movement or catalog-priced usage since activeSince) whose
// period holds activity. A usage-only payer has no ledger row until
// FinalizeInvoice rates it, exactly once through the rating watermark.
func (s *MoneyService) finalizeInvoicePayers(ctx context.Context, activeSince time.Time, period func(gen.ListInvoicePayersRow) (time.Time, time.Time, error)) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("money service not initialized")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	payers, err := s.db.Gen(ctx).ListInvoicePayers(ctx, gen.ListInvoicePayersParams{MerchantID: tid.UUID(), ActiveSince: activeSince.UTC()})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, p := range payers {
		// Balances in an unregistered currency are never invoiced.
		if RequireBillingCurrency(normalizeCurrency(p.Currency)) != nil {
			continue
		}
		from, to, err := period(p)
		if err != nil {
			return count, err
		}
		inv, err := s.finalizeInvoice(ctx, identity.CustomerID(p.CustomerID), p.Currency, from, to, basisActivity)
		if err != nil {
			return count, err
		}
		if inv != nil {
			count++
		}
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
		from := r.PeriodStartsAt
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
		inv, err := s.finalizeInvoice(ctx, identity.CustomerID(r.CustomerID), r.Currency, from, cutoff, basisPending)
		if err != nil {
			return count, err
		}
		if inv != nil {
			count++
		}
	}
	return count, nil
}
