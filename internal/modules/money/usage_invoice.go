package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/destructive"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/billingfact"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/invoices/usage"
	"github.com/open-rails/openrails/internal/modules/money/books"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// Owed usage is billed as a usage invoice: the period sweep closes each customer's
// previous period, the threshold pass the period so far once it reaches the
// customer's trigger, and an explicit close any window. Each bills the window's
// pending owed charges, once.

var (
	ErrUsageActionNotAllowed = errors.New("usage invoice action is not allowed in its current state")
	ErrPaymentExceedsDue     = errors.New("payment amount exceeds what the usage invoice has due")
)

// FinalizeUsage closes [from, to) for one customer and currency at now, the
// caller's clock: the window's pending owed charges, usage priced when it was
// recorded included, are billed as one usage invoice, due after the customer's net
// terms. A second close of the same window answers the first invoice. Nil when
// the window billed nothing.
func (s *MoneyService) FinalizeUsage(ctx context.Context, customer identity.CustomerID, currency string, from, to, now time.Time) (*gen.BillingInvoice, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if customer.IsZero() {
		return nil, fmt.Errorf("customer required")
	}
	if !to.After(from) {
		return nil, fmt.Errorf("invalid period: to must be after from")
	}
	cur := normalizeCurrency(currency)
	if err := RequireBillingCurrency(cur); err != nil {
		return nil, err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		return ensureCustomer(ctx, s.db.Gen(ctx), mid.UUID(), customer.UUID())
	}); err != nil {
		return nil, err
	}
	var out *gen.BillingInvoice
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.finalizeUsageTx(ctx, s.db.NewWithPgxTx(tx), customer, cur, from, to, instant(now))
		return err
	})
	return out, err
}

// finalizeUsageTx bills the window [from, to) at now, the caller's clock.
func (s *MoneyService) finalizeUsageTx(ctx context.Context, d *db.DB, customer identity.CustomerID, cur string, from, to, now time.Time) (*gen.BillingInvoice, error) {
	q := d.Gen(ctx)
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var out *gen.BillingInvoice
	err = func() error {
		merchantID, customerID, pfrom, pto := mid.UUID(), customer.UUID(), from.UTC(), to.UTC()
		// Concurrent closes and money writes serialize on the customer.
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: merchantID, ID: customerID}); err != nil {
			return fmt.Errorf("lock usage customer: %w", err)
		}
		// Usage is owed as it accrued, and running charges through the
		// window's end as far as it has come, before the window is billed.
		if _, err := s.WithDatabase(d).at(now).postEverything(ctx, q, merchantID, customerID, cur, earlier(pto, now), usagePostingBilling); err != nil {
			return err
		}
		// The invoice bills the window's owed items; its period runs from where
		// their charges began (a pricing period's start) to the window's end.
		start, err := q.EarliestPendingOwedCharge(ctx, gen.EarliestPendingOwedChargeParams{MerchantID: merchantID, CustomerID: customerID, Currency: cur,
			WindowStartsAt: pfrom, WindowEndsAt: pto})
		if err != nil {
			return err
		}
		existing, err := q.GetUsageInvoiceOfWindow(ctx, gen.GetUsageInvoiceOfWindowParams{MerchantID: merchantID, CustomerID: customerID, Currency: cur,
			PeriodEndsAt: pto, WindowStartsAt: pfrom})
		if err == nil {
			out = &existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		sources, err := q.SumPendingOwedItemAmountBySourceInPeriod(ctx, gen.SumPendingOwedItemAmountBySourceInPeriodParams{
			MerchantID: merchantID, CustomerID: customerID, Currency: cur, PeriodStartsAt: pfrom, PeriodEndsAt: pto})
		if err != nil {
			return err
		}
		lines := make([]usage.Line, 0, len(sources))
		var receivable int64
		for _, r := range sources {
			if r.Amount > 0 {
				lines = append(lines, usage.Line{Source: r.Source, Amount: r.Amount})
				receivable += r.Amount
			}
		}
		if receivable == 0 {
			return nil
		}
		// Owed a funded balance already repaid is not billed again: an invoice
		// claims at most the owed no other invoice claims.
		owed, err := s.moneyLedger(q, merchantID).OutstandingOwed(ctx, customerID, cur)
		if err != nil {
			return err
		}
		claimed, err := q.SumOwedInvoiceClaims(ctx, gen.SumOwedInvoiceClaimsParams{MerchantID: merchantID, CustomerID: customerID, Currency: cur})
		if err != nil {
			return err
		}
		due := min(receivable, max(owed-claimed, 0))
		// The customer's invoice profile: net-N terms, collection method and the
		// document fields the invoice carries.
		var profile *CustomerInvoiceProfile
		if row, err := q.GetInvoiceProfile(ctx, gen.GetInvoiceProfileParams{MerchantID: merchantID, CustomerID: customerID}); err == nil {
			if profile, err = invoiceProfileFromGen(row); err != nil {
				return err
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		u := usage.Invoice{CustomerID: customerID, Currency: cur, PeriodStart: earlier(start.UTC(), pfrom), PeriodEnd: pto, DueAt: now, Paid: receivable - due,
			Lines: lines, CollectionMethod: CollectionChargeAutomatically}
		if profile != nil {
			if profile.NetTermsDays > 0 {
				u.DueAt = now.Add(time.Duration(profile.NetTermsDays) * 24 * time.Hour)
			}
			if m, err := normalizeCollectionMethod(profile.CollectionMethod); err == nil {
				u.CollectionMethod = m
			}
			u.PONumber, u.Memo, u.Tax, u.BillingContacts = nilIfEmpty(profile.PONumber), nilIfEmpty(profile.Memo), profile.Tax, profile.BillingContacts
		}
		invoice, err := usage.Create(ctx, d, merchantID, u, now)
		if err != nil {
			return err
		}
		if _, err := q.AttachPendingOwedItemsToInvoice(ctx, gen.AttachPendingOwedItemsToInvoiceParams{MerchantID: merchantID, CustomerID: customerID,
			InvoiceID: invoice.ID, Currency: cur, PeriodStartsAt: pfrom, PeriodEndsAt: pto, Now: now}); err != nil {
			return err
		}
		if due > 0 {
			data, err := json.Marshal(billing.NotificationData{InvoiceID: billing.InvoiceID(invoice.ID), AmountDue: &due, Currency: catalog.Currency(cur), DueAt: &u.DueAt,
				CollectionMethod: billing.InvoiceCollectionMethod(u.CollectionMethod)})
			if err != nil {
				return err
			}
			if _, err := billingfact.Record(ctx, q, billingfact.Fact{
				ID:         uuidutil.DerivedV7(uuidutil.TimeOf(invoice.ID), uuidutil.DeterministicNamespace, uuidutil.Name("order_due", merchantID.String(), invoice.ID.String())),
				MerchantID: merchantID, CustomerID: customerID,
				EventType: string(models.NotificationInvoiceDue), Data: data, CreatedAt: now,
			}); err != nil {
				return fmt.Errorf("invoice due notification: %w", err)
			}
		}
		out = &invoice
		return nil
	}()
	return out, err
}

// lockUsageInvoice locks a usage invoice under its customer, in the caller's
// transaction.
func lockUsageInvoice(ctx context.Context, q *gen.Queries, merchantID, id uuid.UUID) (gen.BillingInvoice, error) {
	row, err := q.GetInvoice(ctx, gen.GetInvoiceParams{MerchantID: merchantID, ID: id})
	if err != nil {
		return row, err
	}
	if row.Origin != string(billing.InvoiceOriginUsage) {
		return row, ErrUsageActionNotAllowed
	}
	if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: merchantID, ID: row.CustomerID}); err != nil {
		return row, err
	}
	row, err = q.LockUsageInvoice(ctx, gen.LockUsageInvoiceParams{MerchantID: merchantID, CustomerID: row.CustomerID, ID: id})
	if err == nil && row.CollectionOwner != string(billing.InvoiceCollectionEngine) {
		return row, ErrUsageActionNotAllowed
	}
	return row, err
}

// VoidUsageInvoice voids an unpaid usage invoice and writes off what it still
// claimed, so the ledger stops counting it as owed.
func (s *MoneyService) VoidUsageInvoice(ctx context.Context, id uuid.UUID) error {
	return s.destructiveUsageInvoice(ctx, id, destructive.WaiveInvoice)
}

// MarkUsageInvoiceUncollectible stops collection while retaining the debt. A
// later reopened invoice has a new admitted revision, not the old allowance.
func (s *MoneyService) MarkUsageInvoiceUncollectible(ctx context.Context, id uuid.UUID) error {
	return s.destructiveUsageInvoice(ctx, id, destructive.MarkUncollectible)
}

func (s *MoneyService) destructiveUsageInvoice(ctx context.Context, id uuid.UUID, kind destructive.Kind) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	observed, err := s.db.Gen(ctx).GetInvoice(ctx, gen.GetInvoiceParams{MerchantID: mid.UUID(), ID: id})
	if err != nil {
		return err
	}
	if observed.Origin != string(billing.InvoiceOriginUsage) {
		return ErrUsageActionNotAllowed
	}
	final := string(billing.InvoiceVoid)
	if kind == destructive.MarkUncollectible {
		final = string(billing.InvoiceUncollectible)
	}
	if observed.Status == final {
		return nil
	}
	if observed.Status != string(billing.InvoiceOpen) && !(kind == destructive.WaiveInvoice && observed.Status == string(billing.InvoiceUncollectible)) {
		return ErrUsageActionNotAllowed
	}
	if kind == destructive.MarkUncollectible && usage.Due(observed) <= 0 {
		return ErrUsageActionNotAllowed
	}
	command, err := usageInvoiceDestructiveCommand(ctx, kind, observed)
	if err != nil {
		return err
	}
	return destructive.RunLocal(ctx, s.db, s.destructiveAdmissions, command, func(ctx context.Context, _ pgx.Tx, txDB *db.DB) (bool, error) {
		q := txDB.Gen(ctx)
		invoice, err := lockUsageInvoice(ctx, q, mid.UUID(), id)
		if err != nil {
			return false, err
		}
		if invoice.Status == final {
			return false, nil
		}
		if invoice.Total != observed.Total || usage.Due(invoice) != usage.Due(observed) || invoice.Status != observed.Status {
			return false, ErrUsageActionNotAllowed
		}
		if (invoice.AttemptID == nil) != (observed.AttemptID == nil) || invoice.AttemptID != nil && *invoice.AttemptID != *observed.AttemptID {
			return false, ErrUsageActionNotAllowed
		}
		if kind == destructive.MarkUncollectible {
			changed, err := usage.MarkUncollectible(ctx, txDB, invoice, s.now())
			if err == nil && !changed {
				err = ErrUsageActionNotAllowed
			}
			return changed, err
		}
		due := usage.Due(invoice)
		changed, err := usage.VoidInvoice(ctx, txDB, invoice, s.now())
		if err != nil {
			return false, err
		}
		if !changed {
			return false, ErrUsageActionNotAllowed
		}
		if due > 0 {
			if _, err = s.moneyLedger(q, mid.UUID()).WriteOffOwed(ctx, invoice.CustomerID, normalizeCurrency(invoice.Currency), due,
				ledger.Coord{Operation: ledger.OpInvoiceVoid, Source: "invoice_void", SourceID: id.String()}, &id); err != nil {
				return false, err
			}
			err = books.New(q, mid.UUID()).WaiveUsage(ctx, invoice, due, s.now())
		}
		return true, err
	})
}

// UsageRemittance is money the merchant received for a usage invoice outside
// OpenRails. TransactionID is its identity: recording it again with the same
// terms answers the first payment.
type UsageRemittance struct {
	InvoiceID     uuid.UUID
	Amount        int64
	TransactionID string
	// PaidAt defaults to now; a replay that omits it does not compare it.
	PaidAt *time.Time
}

// RecordUsageRemittance records a remittance as a manual payment on its
// usage invoice and settles that much of what the invoice claims. created is
// false when the same remittance was already recorded.
func (s *MoneyService) RecordUsageRemittance(ctx context.Context, in UsageRemittance) (payment *models.Payment, created bool, err error) {
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
		invoice, e := lockUsageInvoice(ctx, q, mid.UUID(), in.InvoiceID)
		if e != nil {
			return e
		}
		// A recorded remittance answers its replay before a paid status, a
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
		if invoice.Status != string(billing.InvoiceOpen) && invoice.Status != string(billing.InvoiceUncollectible) {
			return ErrUsageActionNotAllowed
		}
		if in.Amount > usage.Due(invoice) {
			return ErrPaymentExceedsDue
		}
		now := s.now()
		paidAt := now
		if in.PaidAt != nil {
			paidAt = in.PaidAt.UTC().Truncate(time.Microsecond)
		}
		// The money arrives (DR external_receipts / CR unapplied_cash),
		// then the owed payment settles the arrears liability (DR
		// units_sold / CR customer_owed) at the remittance's unique
		// coordinate.
		id := uuidutil.NewV7()
		if e := books.New(q, mid.UUID()).Receive(ctx, books.Receipt{Payment: id, Customer: invoice.CustomerID, Invoice: &in.InvoiceID, Amount: in.Amount, Currency: invoice.Currency, At: paidAt}); e != nil {
			return e
		}
		if e := books.Started(ctx, q, mid.UUID(), invoice.Currency, now); e != nil {
			return e
		}
		coord := ledger.Coord{Operation: ledger.OpManualInvoicePayment, Source: "manual_invoice_payment", SourceID: in.TransactionID}
		tr, e := s.moneyLedger(q, mid.UUID()).PayOwed(ctx, invoice.CustomerID, normalizeCurrency(invoice.Currency), in.Amount, coord, &in.InvoiceID)
		if e != nil {
			return e
		}
		if tr.Amount != in.Amount || tr.InvoiceID == nil || *tr.InvoiceID != in.InvoiceID {
			return billing.ErrIdempotencyKeyReused
		}
		if e := books.New(q, mid.UUID()).CollectUsage(ctx, books.UsageCollection{Payment: id, Customer: invoice.CustomerID, Invoice: &in.InvoiceID,
			Currency: invoice.Currency, Amount: in.Amount, At: paidAt}); e != nil {
			return e
		}
		if e := q.InsertInvoicePayment(ctx, gen.InsertInvoicePaymentParams{ID: id, MerchantID: mid.UUID(), CustomerID: invoice.CustomerID, InvoiceID: in.InvoiceID,
			LedgerTransferID: tr.ID, Channel: string(models.ChannelManual), TransactionID: in.TransactionID, Amount: in.Amount, Currency: invoice.Currency, PaidAt: paidAt, Now: now}); e != nil {
			if db.IsUniqueViolation(e) {
				return billing.ErrIdempotencyKeyReused
			}
			return e
		}
		if _, e := usage.Pay(ctx, s.db.NewWithPgxTx(tx), mid.UUID(), in.InvoiceID, in.Amount, &id, nil, now); e != nil {
			return e
		}
		row, e := q.GetInvoicePayment(ctx, gen.GetInvoicePaymentParams{MerchantID: mid.UUID(), CustomerID: invoice.CustomerID, InvoiceID: in.InvoiceID, ID: id})
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

// A collection is the only transition that reopens an uncollectible invoice.
// Its immutable attempt ID distinguishes that new episode even with a frozen
// clock; incidental updates must never stand in for operation identity.
func usageInvoiceDestructiveCommand(ctx context.Context, kind destructive.Kind, invoice gen.BillingInvoice) (destructive.Command, error) {
	revision := "initial"
	if invoice.AttemptID != nil {
		revision = invoice.AttemptID.String()
	}
	return destructive.CommandFromContext(ctx, kind, invoice.ID.String(), string(kind)+":"+invoice.ID.String()+":"+revision, struct {
		Revision, Status string
		Total, Due       int64
	}{revision, invoice.Status, invoice.Total, usage.Due(invoice)})
}
