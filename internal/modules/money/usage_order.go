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
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/modules/orders/usage"
	"github.com/open-rails/openrails/internal/retention"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// Owed usage is billed as a usage order: the period sweep closes each payer's
// previous period, the threshold pass the period so far once it reaches the
// payer's trigger, and an explicit close any window. Each bills the window's
// pending owed charges, once.

var (
	ErrUsageActionNotAllowed = errors.New("usage order action is not allowed in its current state")
	ErrPaymentExceedsDue     = errors.New("payment amount exceeds what the usage order has due")
)

// FinalizeUsage closes [from, to) for one payer and currency: the window's
// pending owed charges, usage priced when it was recorded included, are
// billed as one usage order, due after the payer's net terms. A second close
// of the same window answers the first order. Nil when the window billed
// nothing.
func (s *MoneyService) FinalizeUsage(ctx context.Context, payer identity.CustomerID, currency string, from, to time.Time) (*gen.BillingOrder, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
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
		return ensureCustomer(ctx, s.db.Gen(ctx), mid.UUID(), payer.UUID())
	}); err != nil {
		return nil, err
	}
	var out *gen.BillingOrder
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		merchantID, payerID, pfrom, pto := mid.UUID(), payer.UUID(), from.UTC(), to.UTC()
		// Concurrent closes and money writes serialize on the payer.
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: merchantID, ID: payerID}); err != nil {
			return fmt.Errorf("lock usage payer: %w", err)
		}
		existing, err := q.GetUsageOrderByPeriod(ctx, gen.GetUsageOrderByPeriodParams{MerchantID: merchantID, CustomerID: payerID, Currency: cur, PeriodStartsAt: pfrom, PeriodEndsAt: pto})
		if err == nil {
			out = &existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		sources, err := q.SumPendingOwedItemAmountBySourceInPeriod(ctx, gen.SumPendingOwedItemAmountBySourceInPeriodParams{
			MerchantID: merchantID, CustomerID: payerID, Currency: cur, PeriodStartsAt: pfrom, PeriodEndsAt: pto})
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
		// Owed a funded balance already repaid is not billed again: an order
		// claims at most the owed no other order claims.
		owed, err := s.moneyLedger(q, merchantID).OutstandingOwed(ctx, payerID, cur)
		if err != nil {
			return err
		}
		claimed, err := q.SumOwedOrderClaims(ctx, gen.SumOwedOrderClaimsParams{MerchantID: merchantID, CustomerID: payerID, Currency: cur})
		if err != nil {
			return err
		}
		due := min(receivable, max(owed-claimed, 0))
		// The payer's invoice profile: net-N terms, collection method and the
		// document fields the order carries.
		var profile *CustomerInvoiceProfile
		if row, err := q.GetInvoiceProfile(ctx, gen.GetInvoiceProfileParams{MerchantID: merchantID, CustomerID: payerID}); err == nil {
			if profile, err = invoiceProfileFromGen(row); err != nil {
				return err
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		now := s.now()
		u := usage.Order{CustomerID: payerID, Currency: cur, PeriodStart: pfrom, PeriodEnd: pto, DueAt: now, Paid: receivable - due,
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
		order, err := usage.Create(ctx, q, merchantID, u, now)
		if err != nil {
			return err
		}
		if _, err := q.AttachPendingOwedItemsToOrder(ctx, gen.AttachPendingOwedItemsToOrderParams{MerchantID: merchantID, CustomerID: payerID,
			OrderID: order.ID, Currency: cur, PeriodStartsAt: pfrom, PeriodEndsAt: pto, Now: now}); err != nil {
			return err
		}
		if due > 0 {
			data, err := json.Marshal(billing.NotificationData{OrderID: billing.OrderID(order.ID), AmountDue: &due, Currency: cur, DueAt: &u.DueAt,
				CollectionMethod: billing.InvoiceCollectionMethod(u.CollectionMethod)})
			if err != nil {
				return err
			}
			if err := q.CreateNotificationIfAbsent(ctx, gen.CreateNotificationIfAbsentParams{
				ID:         uuidutil.DeterministicID(uuidutil.DeterministicNamespace, "order_due", merchantID.String(), order.ID.String()),
				MerchantID: merchantID, CustomerID: payerID,
				EventType: string(models.NotificationOrderDue), Data: data, CreatedAt: now,
			}); err != nil {
				return fmt.Errorf("order due notification: %w", err)
			}
		}
		out = &order
		return nil
	})
	return out, err
}

// FinalizeDueUsageForBoundary closes the previous period of every payer
// active in it under the merchant's billing_period_boundary; anniversary
// periods are anchored on the payer's first recorded activity.
func (s *MoneyService) FinalizeDueUsageForBoundary(ctx context.Context, boundary string, now time.Time) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("money service not initialized")
	}
	if now.IsZero() {
		now = s.now()
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	// The previous period starts at most two periods back, so three months of
	// activity names every payer it could still have to bill.
	activeSince := retention.MonthStart(now).AddDate(0, -3, 0)
	payers, err := s.db.Gen(ctx).ListUsagePayers(ctx, gen.ListUsagePayersParams{MerchantID: mid.UUID(), ActiveSince: activeSince.UTC()})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, p := range payers {
		// Balances in an unregistered currency are never billed.
		if RequireBillingCurrency(normalizeCurrency(p.Currency)) != nil {
			continue
		}
		from, to, err := PreviousBillingPeriod(now, p.PeriodAnchor, boundary)
		if err != nil {
			return count, err
		}
		order, err := s.FinalizeUsage(ctx, identity.CustomerID(p.CustomerID), p.Currency, from, to)
		if err != nil {
			return count, err
		}
		if order != nil {
			count++
		}
	}
	return count, nil
}

// UsageThresholdOptions are the merchant's threshold pass settings.
type UsageThresholdOptions struct {
	CollectionThresholdAmount int64
	BillingPeriodBoundary     string
}

// FinalizeThresholdUsage bills the period so far of every arrears payer
// whose pending charges plus unpaid usage orders reached their trigger.
// Collection is left to ChargeOutstanding, so provider calls stay out of the
// billing transaction.
func (s *MoneyService) FinalizeThresholdUsage(ctx context.Context, cutoff time.Time, opt UsageThresholdOptions) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("money service not initialized")
	}
	if cutoff.IsZero() {
		cutoff = s.now()
	}
	cutoff = cutoff.UTC()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	settings, err := merchantconfig.NewStore(s.db).Settings(ctx)
	if err != nil {
		return 0, err
	}
	rows, err := s.db.Gen(ctx).ListUsageThresholdCandidates(ctx, gen.ListUsageThresholdCandidatesParams{
		MerchantID: mid.UUID(), Cutoff: cutoff, MinThreshold: opt.CollectionThresholdAmount, Policies: settings.PoliciesParam(),
	})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, r := range rows {
		from := r.PeriodStartsAt
		if opt.BillingPeriodBoundary != "" {
			start, err := CurrentBillingPeriodStart(cutoff, r.PeriodAnchor, opt.BillingPeriodBoundary)
			if err != nil {
				return count, err
			}
			from = start
		}
		if !cutoff.After(from) {
			continue
		}
		order, err := s.FinalizeUsage(ctx, identity.CustomerID(r.CustomerID), r.Currency, from, cutoff)
		if err != nil {
			return count, err
		}
		if order != nil {
			count++
		}
	}
	return count, nil
}

// lockUsageOrder locks a usage order under its payer, in the caller's
// transaction.
func lockUsageOrder(ctx context.Context, q *gen.Queries, merchantID, id uuid.UUID) (gen.BillingOrder, error) {
	row, err := q.GetOrder(ctx, gen.GetOrderParams{MerchantID: merchantID, ID: id})
	if err != nil {
		return row, err
	}
	if row.Origin != string(billing.OrderOriginUsage) {
		return row, ErrUsageActionNotAllowed
	}
	if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: merchantID, ID: row.CustomerID}); err != nil {
		return row, err
	}
	return q.LockUsageOrder(ctx, gen.LockUsageOrderParams{MerchantID: merchantID, CustomerID: row.CustomerID, ID: id})
}

// VoidUsageOrder voids an unpaid usage order and writes off what it still
// claimed, so the ledger stops counting it as owed.
func (s *MoneyService) VoidUsageOrder(ctx context.Context, id uuid.UUID) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		order, err := lockUsageOrder(ctx, q, mid.UUID(), id)
		if err != nil {
			return err
		}
		if order.Status == string(billing.OrderVoid) {
			return nil
		}
		due := usage.Due(order)
		voided, err := usage.VoidOrder(ctx, q, order, s.now())
		if err != nil {
			return err
		}
		if !voided {
			return ErrUsageActionNotAllowed
		}
		if due > 0 {
			_, err = s.moneyLedger(q, mid.UUID()).WriteOffOwed(ctx, order.CustomerID, normalizeCurrency(order.Currency), due,
				ledger.Coord{Operation: ledger.OpOrderVoid, Source: "invoice_void", SourceID: id.String()}, &id)
		}
		return err
	})
}

// MarkUsageOrderUncollectible writes an open usage order off; the debt
// stands, and collecting the order again reopens it.
func (s *MoneyService) MarkUsageOrderUncollectible(ctx context.Context, id uuid.UUID) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		order, err := lockUsageOrder(ctx, q, mid.UUID(), id)
		if err != nil {
			return err
		}
		if order.Status == string(billing.OrderUncollectible) {
			return nil
		}
		marked, err := usage.MarkUncollectible(ctx, q, order, s.now())
		if err == nil && !marked {
			err = ErrUsageActionNotAllowed
		}
		return err
	})
}

// UsageRemittance is money the merchant received for a usage order outside
// OpenRails. TransactionID is its identity: recording it again with the same
// terms answers the first payment.
type UsageRemittance struct {
	OrderID       uuid.UUID
	Amount        int64
	TransactionID string
	// PaidAt defaults to now; a replay that omits it does not compare it.
	PaidAt *time.Time
}

// RecordUsageRemittance records a remittance as a manual payment on its
// usage order and settles that much of what the order claims. created is
// false when the same remittance was already recorded.
func (s *MoneyService) RecordUsageRemittance(ctx context.Context, in UsageRemittance) (payment *models.Payment, created bool, err error) {
	if s == nil || s.db == nil {
		return nil, false, fmt.Errorf("money service not initialized")
	}
	in.TransactionID = strings.TrimSpace(in.TransactionID)
	if in.OrderID == uuid.Nil || in.Amount <= 0 || in.TransactionID == "" {
		return nil, false, fmt.Errorf("order, positive amount and transaction id are required")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, false, err
	}
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		order, e := lockUsageOrder(ctx, q, mid.UUID(), in.OrderID)
		if e != nil {
			return e
		}
		// A recorded remittance answers its replay before a paid status, a
		// reduced due amount or a later collection could refuse it.
		prior, e := q.GetPaymentByPSPTransactionID(ctx, gen.GetPaymentByPSPTransactionIDParams{MerchantID: mid.UUID(), Channel: string(models.ChannelManual), TransactionID: in.TransactionID})
		switch {
		case e == nil:
			if prior.OrderID == nil || *prior.OrderID != in.OrderID || prior.Amount != in.Amount || in.PaidAt != nil && !prior.PurchasedAt.Equal(in.PaidAt.UTC().Truncate(time.Microsecond)) {
				return billing.ErrIdempotencyKeyReused
			}
			payment, e = models.PaymentFromGen(prior)
			return e
		case !errors.Is(e, pgx.ErrNoRows):
			return e
		}
		if order.Status != string(billing.OrderOpen) && order.Status != string(billing.OrderUncollectible) {
			return ErrUsageActionNotAllowed
		}
		if in.Amount > usage.Due(order) {
			return ErrPaymentExceedsDue
		}
		now := s.now()
		paidAt := now
		if in.PaidAt != nil {
			paidAt = in.PaidAt.UTC().Truncate(time.Microsecond)
		}
		// The owed payment settles the arrears liability (DR
		// processor_clearing / CR arrears_liability) at the remittance's
		// unique coordinate.
		coord := ledger.Coord{Operation: ledger.OpManualOrderPayment, Source: "manual_invoice_payment", SourceID: in.TransactionID}
		tr, e := s.moneyLedger(q, mid.UUID()).PayOwed(ctx, order.CustomerID, normalizeCurrency(order.Currency), in.Amount, coord, &in.OrderID)
		if e != nil {
			return e
		}
		if tr.Amount != in.Amount || tr.OrderID == nil || *tr.OrderID != in.OrderID {
			return billing.ErrIdempotencyKeyReused
		}
		id := uuidutil.NewV7()
		if e := q.InsertOrderPayment(ctx, gen.InsertOrderPaymentParams{ID: id, MerchantID: mid.UUID(), CustomerID: order.CustomerID, OrderID: in.OrderID,
			LedgerTransferID: tr.ID, Channel: string(models.ChannelManual), TransactionID: in.TransactionID, Amount: in.Amount, Currency: order.Currency, PaidAt: paidAt, Now: now}); e != nil {
			if db.IsUniqueViolation(e) {
				return billing.ErrIdempotencyKeyReused
			}
			return e
		}
		if _, e := usage.Pay(ctx, q, mid.UUID(), in.OrderID, in.Amount, &id, nil, now); e != nil {
			return e
		}
		row, e := q.GetOrderPayment(ctx, gen.GetOrderPaymentParams{MerchantID: mid.UUID(), CustomerID: order.CustomerID, OrderID: in.OrderID, ID: id})
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
