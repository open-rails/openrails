package payments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money/books"
	"github.com/open-rails/openrails/internal/modules/money/credits"
	"github.com/open-rails/openrails/internal/modules/purchasedcredits"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

type PaymentService struct {
	db    *db.DB
	clock clockwork.Clock
}

const (
	PaymentStatusPendingValue   = "pending"
	PaymentStatusSucceededValue = "succeeded"
	PaymentStatusFailedValue    = "failed"
	PaymentStatusRefundedValue  = "refunded"
)

// SetCard stamps p with the brand, in lower case, and last four of the card
// it charged.
func SetCard(p *models.Payment, c models.Card) {
	c.Brand = strings.ToLower(c.Brand)
	p.CardBrand, p.CardLast4, _, _ = c.Columns()
}

// SetMethodCard stamps p with the saved method it charged and its card; a
// method that is gone leaves p as it is.
func SetMethodCard(ctx context.Context, q *gen.Queries, merchantID, methodID uuid.UUID, p *models.Payment) error {
	pm, err := q.GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: merchantID, ID: methodID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	SetCard(p, models.CardFromColumns(pm.CardBrand, pm.CardLast4, nil, nil))
	if pm.CustomerID == p.CustomerID {
		p.PaymentMethodID = &pm.ID
	}
	return nil
}

// now returns the current time from the service's clock, or time.Now() if no clock is set.
func (s *PaymentService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

func NewPaymentService(db *db.DB, clocks ...clockwork.Clock) *PaymentService {
	return &PaymentService{db: db, clock: timeutil.FirstClock(clocks...)}
}

func (s *PaymentService) SetClock(c clockwork.Clock) {
	s.clock = timeutil.FirstClock(c)
}

func (s *PaymentService) Clock() clockwork.Clock {
	return s.clock
}

func (s *PaymentService) Create(ctx context.Context, payment *models.Payment) error {
	if payment != nil && payment.RefundedPaymentID != nil && payment.ReversalKind != nil && *payment.ReversalKind == ReversalDisputeReversal && payment.Amount > 0 && PaymentStatusSucceeded(payment.Status) {
		return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			d := s.db.NewWithPgxTx(tx)
			scoped := NewPaymentService(d, s.clock)
			mid, err := merchant.Require(ctx)
			if err != nil {
				return err
			}
			if _, err := d.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: payment.CustomerID}); err != nil {
				return err
			}
			if err := scoped.insert(ctx, payment); err != nil {
				return err
			}
			if err := scoped.syncPurchasedCreditRefund(ctx, *payment.RefundedPaymentID); err != nil {
				return err
			}
			return books.PostReversal(ctx, d, payment.ID, s.now())
		})
	}
	return s.insert(ctx, payment)
}

// Reversal kinds of the mirror rows Refund writes: a chargeback is recorded
// like a refund, and the kind tells them apart for metrics.
const (
	ReversalRefund          = "refund"
	ReversalChargeback      = "chargeback"
	ReversalDisputeReversal = "dispute_reversal"
)

// Refund records a reversal as a negative payment entry linked by transaction
// ID. reversalKind says WHAT the reversal is (refund vs chargeback); the rails
// handle the actual money movement, this persists the event. amount is micros.
func (s *PaymentService) Refund(ctx context.Context, originalPaymentID uuid.UUID, refundTransactionID string, amount int64, reversalKind string) (*models.Payment, error) {
	return s.recordRefund(ctx, originalPaymentID, refundTransactionID, amount, reversalKind, "", time.Time{}, nil, true)
}

// RefundWithReason records a provider's refund and the reason it gives.
func (s *PaymentService) RefundWithReason(ctx context.Context, originalPaymentID uuid.UUID, refundTransactionID string, amount int64, reason billing.RefundReason) (*models.Payment, error) {
	return s.recordRefund(ctx, originalPaymentID, refundTransactionID, amount, ReversalRefund, reason, time.Time{}, nil, true)
}

// RefundObservedWithReason records a provider refund with its reason at the
// provider's occurrence time.
func (s *PaymentService) RefundObservedWithReason(ctx context.Context, originalPaymentID uuid.UUID, refundTransactionID string, amount int64, reason billing.RefundReason, occurredAt time.Time) (*models.Payment, error) {
	if occurredAt.IsZero() {
		return s.RefundWithReason(ctx, originalPaymentID, refundTransactionID, amount, reason)
	}
	return s.recordRefund(ctx, originalPaymentID, refundTransactionID, amount, ReversalRefund, reason, occurredAt.UTC().Truncate(time.Microsecond), nil, true)
}

// RecordObservedRefund retains the provider's occurrence time and provenance.
// The general ledger dates the reversal's legs at that time, when it settled;
// access and other local effects use the service clock. A replay returns the
// first accepted facts without rewriting their metadata.
func (s *PaymentService) RecordObservedRefund(ctx context.Context, originalPaymentID uuid.UUID, refundTransactionID string, amount int64, reversalKind string, occurredAt time.Time, metadata map[string]any) (*models.Payment, error) {
	if occurredAt.IsZero() {
		return nil, errors.New("observed refund requires its provider occurrence time")
	}
	return s.recordRefund(ctx, originalPaymentID, refundTransactionID, amount, reversalKind, "", occurredAt.UTC().Truncate(time.Microsecond), metadata, true)
}

// RecordImportedRefund is RecordObservedRefund for a refund an import brings
// in from the merchant's history: the customer was told at the time, so it
// records no refund receipt.
func (s *PaymentService) RecordImportedRefund(ctx context.Context, originalPaymentID uuid.UUID, refundTransactionID string, amount int64, occurredAt time.Time) (*models.Payment, error) {
	if occurredAt.IsZero() {
		return nil, errors.New("imported refund requires its provider occurrence time")
	}
	return s.recordRefund(ctx, originalPaymentID, refundTransactionID, amount, ReversalRefund, "", occurredAt.UTC().Truncate(time.Microsecond), nil, false)
}

// recordRefund records the reversal; receipt records a new refund's
// refund_completed fact with it.
func (s *PaymentService) recordRefund(ctx context.Context, originalPaymentID uuid.UUID, refundTransactionID string, amount int64, reversalKind string, reason billing.RefundReason, occurredAt time.Time, metadata map[string]any, receipt bool) (*models.Payment, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var refund *models.Payment
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Serialize provider facts against the original charge. Different webhook
		// event IDs can describe the same refund; validation and replay lookup must
		// share the lock so neither duplicate insertion nor double counting races.
		transactionDB := s.db.NewWithPgxTx(tx)
		original, err := NewPaymentService(transactionDB, s.clock).GetByID(ctx, originalPaymentID)
		if err != nil {
			return err
		}

		if _, err := transactionDB.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: original.CustomerID}); err != nil {
			return err
		}
		if _, err := transactionDB.Gen(ctx).LockPaymentSubscriptions(ctx, gen.LockPaymentSubscriptionsParams{MerchantID: mid.UUID(), PaymentID: originalPaymentID}); err != nil {
			return err
		}

		if _, err := transactionDB.Gen(ctx).LockPaymentForRefund(ctx, gen.LockPaymentForRefundParams{MerchantID: mid.UUID(), PaymentID: originalPaymentID}); err != nil {
			return err
		}
		scoped := NewPaymentService(transactionDB, s.clock)
		var created bool
		refund, created, err = scoped.refundLocked(ctx, originalPaymentID, refundTransactionID, amount, reversalKind, reason, occurredAt, metadata)
		if err != nil {
			return err
		}
		if created && receipt {
			if err := scoped.queueRefundReceipt(ctx, refund, true); err != nil {
				return err
			}
		}
		if err := scoped.syncPurchasedCreditRefund(ctx, originalPaymentID); err != nil {
			return err
		}
		if reversalKind == ReversalRefund && refund.Metadata["admin_refund_idempotency_key"] == nil {
			if err := scoped.ApplyRefundAccessPolicy(ctx, originalPaymentID); err != nil {
				return err
			}
		}
		// Booked after the access policy: access it ended makes the refund
		// take unrecognised revenue first.
		return books.PostReversal(ctx, transactionDB, refund.ID, s.now())
	})
	return refund, err
}

// refundReasonOf is a reversal's reason: a refund's, unknown when none is
// named; nil on a chargeback or a won dispute.
func refundReasonOf(kind string, reason billing.RefundReason) *string {
	if kind != ReversalRefund {
		return nil
	}
	if !reason.Valid() {
		reason = billing.RefundUnknown
	}
	r := string(reason)
	return &r
}

// ErrRefundReservationPending defers a provider refund fact while an OpenRails
// refund of the same payment is unresolved.
var ErrRefundReservationPending = errors.New("an OpenRails refund of this payment is still in flight; retry")

// refundLocked records the reversal unless it is recorded already; created
// reports a new one.
func (s *PaymentService) refundLocked(ctx context.Context, originalPaymentID uuid.UUID, refundTransactionID string, amount int64, reversalKind string, reason billing.RefundReason, occurredAt time.Time, metadata map[string]any) (_ *models.Payment, created bool, _ error) {
	if reversalKind != ReversalRefund && reversalKind != ReversalChargeback && reversalKind != ReversalDisputeReversal {
		return nil, false, fmt.Errorf("invalid reversal kind %q", reversalKind)
	}
	orig, err := s.GetByID(ctx, originalPaymentID)
	if err != nil {
		return nil, false, err
	}
	if orig.PspID != nil {
		ctx = db.WithPSPID(ctx, *orig.PspID)
	}
	existing, err := s.GetByPSPTransactionID(ctx, orig.Rail, refundTransactionID)
	if err == nil {
		if existing.RefundedPaymentID == nil || *existing.RefundedPaymentID != orig.ID || existing.Amount != -amount || existing.ReversalKind == nil || *existing.ReversalKind != reversalKind || !PaymentStatusSucceeded(existing.Status) {
			return nil, false, errors.New("refund transaction id is already bound to different payment facts")
		}
		return existing, false, nil
	}
	if !db.IsNotFound(err) {
		return nil, false, err
	}
	// While an OpenRails refund of this payment is in flight, a provider
	// refund fact may be that same refund; recording it now would count it
	// twice. The caller retries once the reservation resolves.
	if reversalKind == ReversalRefund {
		refunds, err := s.ListRefunds(ctx, orig.ID)
		if err != nil {
			return nil, false, err
		}
		for _, r := range refunds {
			if r.Amount < 0 && strings.EqualFold(strings.TrimSpace(r.Status), PaymentStatusPendingValue) {
				return nil, false, ErrRefundReservationPending
			}
		}
	}
	if err := s.ValidateRefund(ctx, orig, amount); err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(refundTransactionID) == "" {
		return nil, false, errors.New("refund transaction id is required")
	}
	now := s.now()
	if occurredAt.IsZero() {
		occurredAt = now
	}

	refund := &models.Payment{
		ID:             uuidutil.NewV7(),
		CustomerID:     orig.CustomerID,
		SubscriptionID: orig.SubscriptionID,
		OrderID:        orig.OrderID,
		RefundedPaymentID: func() *uuid.UUID {
			id := orig.ID
			return &id
		}(),
		Rail: orig.Rail,
		// A reversal is executed by the account that took the charge, so it
		// inherits the original's PSP rather than re-resolving.
		PspID:         orig.PspID,
		TransactionID: refundTransactionID,
		Amount:        -amount,
		ListAmount:    orig.ListAmount,
		Currency:      orig.Currency,
		CardBrand:     orig.CardBrand,
		CardLast4:     orig.CardLast4,
		Status:        PaymentStatusSucceededValue,
		ReversalKind:  &reversalKind,
		RefundReason:  refundReasonOf(reversalKind, reason),
		// The reversal settled at the rail; the feed excludes it on
		// amount/refunded_payment_id, not on this marker.
		MoneyMovement: models.MoneyMovementRail,
		PurchasedAt:   occurredAt,
		CreatedAt:     now,
		Metadata:      metadata,
	}
	if err := s.Create(ctx, refund); err != nil {
		return nil, false, err
	}
	return refund, true, nil
}

func (s *PaymentService) ReserveRefund(ctx context.Context, originalPaymentID uuid.UUID, reservationTransactionID string, amount int64, reason billing.RefundReason, metadata map[string]any) (*models.Payment, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var reservation *models.Payment
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		scoped := NewPaymentService(d, s.clock)
		original, err := scoped.GetByID(ctx, originalPaymentID)
		if err != nil {
			return err
		}
		if _, err := d.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: original.CustomerID}); err != nil {
			return err
		}
		if _, err := d.Gen(ctx).LockPaymentForRefund(ctx, gen.LockPaymentForRefundParams{MerchantID: mid.UUID(), PaymentID: originalPaymentID}); err != nil {
			return err
		}
		if err := purchasedcredits.New(d, s.clock).ValidateRefund(ctx, originalPaymentID, amount); err != nil {
			return err
		}
		if reservation, err = scoped.reserveRefundLocked(ctx, originalPaymentID, reservationTransactionID, amount, reason, metadata); err != nil {
			return err
		}
		return credits.New(d.Gen(ctx), mid.UUID(), s.now).SyncReservation(ctx, originalPaymentID)
	})
	return reservation, err
}

func (s *PaymentService) reserveRefundLocked(ctx context.Context, originalPaymentID uuid.UUID, reservationTransactionID string, amount int64, reason billing.RefundReason, metadata map[string]any) (*models.Payment, error) {
	orig, err := s.GetByID(ctx, originalPaymentID)
	if err != nil {
		return nil, err
	}
	if err := s.ValidateRefund(ctx, orig, amount); err != nil {
		return nil, err
	}
	reservationTransactionID = strings.TrimSpace(reservationTransactionID)
	if reservationTransactionID == "" {
		return nil, errors.New("refund reservation transaction id is required")
	}

	now := s.now()
	kind := ReversalRefund
	refund := &models.Payment{
		ID:             uuidutil.NewV7(),
		CustomerID:     orig.CustomerID,
		SubscriptionID: orig.SubscriptionID,
		OrderID:        orig.OrderID,
		RefundedPaymentID: func() *uuid.UUID {
			id := orig.ID
			return &id
		}(),
		Rail: orig.Rail,
		// Same account as the charge it reverses.
		PspID:         orig.PspID,
		TransactionID: reservationTransactionID,
		Amount:        -amount,
		ListAmount:    orig.ListAmount,
		Currency:      orig.Currency,
		CardBrand:     orig.CardBrand,
		CardLast4:     orig.CardLast4,
		Status:        PaymentStatusPendingValue,
		ReversalKind:  &kind,
		RefundReason:  refundReasonOf(kind, reason),
		Metadata:      metadata,
		PurchasedAt:   now,
		CreatedAt:     now,
	}
	if err := s.Create(ctx, refund); err != nil {
		return nil, err
	}
	return refund, nil
}

func (s *PaymentService) CompleteRefundReservation(ctx context.Context, reservationID uuid.UUID, refundTransactionID string, metadata map[string]any) (*models.Payment, error) {
	if strings.TrimSpace(refundTransactionID) == "" {
		return nil, errors.New("refund transaction id is required")
	}
	var result *models.Payment
	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		scoped := NewPaymentService(d, s.clock)
		reservation, err := scoped.GetByID(ctx, reservationID)
		if err != nil {
			return err
		}
		mid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		if _, err := d.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: reservation.CustomerID}); err != nil {
			return err
		}
		if err := scoped.completeRefundReservation(ctx, reservationID, strings.TrimSpace(refundTransactionID), metadata); err != nil {
			return err
		}
		if reservation.RefundedPaymentID != nil {
			if err := scoped.syncPurchasedCreditRefund(ctx, *reservation.RefundedPaymentID); err != nil {
				return err
			}
		}
		if err := books.PostReversal(ctx, d, reservationID, s.now()); err != nil {
			return err
		}
		if result, err = scoped.GetByID(ctx, reservationID); err != nil {
			return err
		}
		return scoped.queueRefundReceipt(ctx, result, false)
	})
	return result, err
}

// syncPurchasedCreditRefund uses each settled reversal's identity. A dispute
// recovery may restore only the credit withdrawn by that dispute; aggregating
// cash reversals would accidentally undo unrelated voluntary refunds.
func (s *PaymentService) syncPurchasedCreditRefund(ctx context.Context, paymentID uuid.UUID) error {
	rows, err := s.ListRefunds(ctx, paymentID)
	if err != nil {
		return err
	}
	purchased := purchasedcredits.New(s.db, s.clock)
	// ListRefunds is newest first. Apply negative events oldest first, then
	// their positive recoveries, so every recovery finds its exact effect.
	// A reversal that took no credit is applied again on every pass: passes
	// repeat until none posts, so credit a recovery reopens is settled now,
	// not by whichever later event syncs next.
	for {
		posted := false
		for i := len(rows) - 1; i >= 0; i-- {
			row := rows[i]
			if PaymentStatusSucceeded(row.Status) && row.Amount < 0 {
				p, err := purchased.ApplyReversal(ctx, paymentID, row.ID, nil)
				if err != nil {
					return err
				}
				posted = posted || p
			}
		}
		for i := len(rows) - 1; i >= 0; i-- {
			row := rows[i]
			if !PaymentStatusSucceeded(row.Status) || row.Amount <= 0 {
				continue
			}
			var reversed *uuid.UUID
			if raw, ok := row.Metadata["reverses_payment_id"].(string); ok {
				id, err := uuid.Parse(raw)
				if err != nil || id == uuid.Nil {
					return errors.New("dispute recovery has invalid reversal identity")
				}
				reversed = &id
			}
			p, err := purchased.ApplyReversal(ctx, paymentID, row.ID, reversed)
			if err != nil {
				return err
			}
			posted = posted || p
		}
		if !posted {
			mid, err := merchant.Require(ctx)
			if err != nil {
				return err
			}
			// What the payment's pending refunds reserve of its lot follows
			// every settled reversal.
			return credits.New(s.db.Gen(ctx), mid.UUID(), s.now).SyncReservation(ctx, paymentID)
		}
	}
}

func (s *PaymentService) CompleteProviderAttempt(ctx context.Context, attemptID uuid.UUID, providerTransactionID string, metadata map[string]any) (*models.Payment, error) {
	if strings.TrimSpace(providerTransactionID) == "" {
		return nil, errors.New("provider transaction id is required")
	}
	if err := s.completeProviderAttempt(ctx, attemptID, providerTransactionID, metadata); err != nil {
		return nil, err
	}
	return s.GetByID(ctx, attemptID)
}

func (s *PaymentService) CompleteProviderAttemptInPlace(ctx context.Context, attemptID uuid.UUID, metadata map[string]any) (*models.Payment, error) {
	if err := s.completeProviderAttemptInPlace(ctx, attemptID, metadata); err != nil {
		return nil, err
	}
	return s.GetByID(ctx, attemptID)
}

func (s *PaymentService) ValidateRefund(ctx context.Context, orig *models.Payment, amount int64) error {
	if orig == nil {
		return errors.New("original payment is required")
	}
	if amount <= 0 {
		return errors.New("refund amount must be > 0")
	}
	if !PaymentStatusSucceeded(orig.Status) {
		return errors.New("only succeeded charge payments can be refunded")
	}
	if orig.Amount <= 0 || orig.RefundedPaymentID != nil {
		return errors.New("only successful charge payments can be refunded")
	}
	// Only money that arrived can be sent back. Bookkeeping rows (attempt
	// anchors, declines, placeholders) carry a locally minted transaction
	// reference the rail would not recognise.
	if orig.MoneyMovement != models.MoneyMovementRail {
		return errors.New("payment records no money movement at the rail and is not refundable")
	}
	// A usage order's payment settled a ledger debt; refunding it would
	// reopen the debt, which the ledger does not do.
	if orig.LedgerTransferID != nil {
		return errors.New("a usage order's payment is not refundable")
	}

	refundedTotal, err := s.GetRefundTotalByPaymentID(ctx, orig.ID)
	if err != nil {
		return fmt.Errorf("failed to calculate refunded total: %w", err)
	}
	if amount > orig.Amount {
		return fmt.Errorf("%w: refund amount cannot exceed original payment amount", ErrRefundExceedsPayment)
	}
	if refundedTotal > 0 {
		if amount+refundedTotal > orig.Amount {
			return fmt.Errorf("%w: refund total would exceed original payment (refunded %d of %d)", ErrRefundExceedsPayment, refundedTotal, orig.Amount)
		}
	}
	return nil
}

// ErrRefundExceedsPayment refuses a refund that would return more than its
// charge took.
var ErrRefundExceedsPayment = errors.New("refund exceeds the payment")

func PaymentStatusSucceeded(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", PaymentStatusSucceededValue:
		return true
	default:
		return false
	}
}

func (s *PaymentService) LinkRefundedPayment(ctx context.Context, paymentID, originalPaymentID uuid.UUID) error {
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		scoped := NewPaymentService(d, s.clock)
		original, err := scoped.GetByID(ctx, originalPaymentID)
		if err != nil {
			return err
		}
		mid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		if _, err := d.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: original.CustomerID}); err != nil {
			return err
		}
		if err := scoped.linkRefundedPayment(ctx, paymentID, originalPaymentID); err != nil {
			return err
		}
		if err := scoped.syncPurchasedCreditRefund(ctx, originalPaymentID); err != nil {
			return err
		}
		return books.PostReversal(ctx, d, paymentID, s.now())
	})
}

func (s *PaymentService) MarkFailed(ctx context.Context, id uuid.UUID) error {
	payment, err := s.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if payment.Amount >= 0 || payment.RefundedPaymentID == nil {
		return s.markFailed(ctx, id)
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		scoped := NewPaymentService(d, s.clock)
		mid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		if _, err := d.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: payment.CustomerID}); err != nil {
			return err
		}
		current, err := scoped.GetByID(ctx, id)
		if err != nil {
			return err
		}
		if current.Status != PaymentStatusPendingValue && current.Status != PaymentStatusFailedValue {
			return errors.New("a succeeded refund cannot be released")
		}
		if err := scoped.markFailed(ctx, id); err != nil {
			return err
		}
		if _, err := d.Gen(ctx).GetPurchasedCreditGrant(ctx, gen.GetPurchasedCreditGrantParams{MerchantID: mid.UUID(), PaymentID: *payment.RefundedPaymentID}); err != nil {
			if db.IsNotFound(err) {
				return nil
			}
			return err
		}
		// The released refund reserves nothing. It must not expose a lot which
		// expired while the provider result was unresolved: retire it in the
		// same transaction.
		lots := credits.New(d.Gen(ctx), mid.UUID(), s.now)
		if err := lots.SyncReservation(ctx, *payment.RefundedPaymentID); err != nil {
			return err
		}
		_, err = lots.ExpireLapsed(ctx, payment.CustomerID, payment.Currency)
		return err
	})
}

// ApplyRefundAccessPolicy applies the same payment-scoped rule to imported,
// reconciled and notified refunds. Explicit OpenRails refunds retain their
// caller's access choice and do not use this provider policy.
func (s *PaymentService) ApplyRefundAccessPolicy(ctx context.Context, paymentID uuid.UUID) error {
	original, err := s.GetByID(ctx, paymentID)
	if err != nil {
		return err
	}
	total, err := s.GetRefundTotalByPaymentID(ctx, paymentID)
	if err != nil {
		return err
	}
	revoke, err := merchantconfig.ProviderRefundRevokes(ctx, s.db, total, original.Amount)
	if err != nil || !revoke {
		return err
	}
	return entitlements.NewEntitlementService(s.db, s.clock).RevokeRefundedPaymentAccess(ctx, paymentID, "")
}
