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
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/purchasedcredits"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

type PaymentService struct {
	repo  *PaymentRepo
	clock clockwork.Clock
}

const (
	PaymentStatusPendingValue   = "pending"
	PaymentStatusSucceededValue = "succeeded"
	PaymentStatusFailedValue    = "failed"
	PaymentStatusRefundedValue  = "refunded"
)

// now returns the current time from the service's clock, or time.Now() if no clock is set.
func (s *PaymentService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

func NewPaymentService(db *db.DB, clocks ...clockwork.Clock) *PaymentService {
	return &PaymentService{repo: NewPaymentRepo(db), clock: timeutil.FirstClock(clocks...)}
}

func (s *PaymentService) SetClock(c clockwork.Clock) {
	s.clock = timeutil.FirstClock(c)
}

func (s *PaymentService) Clock() clockwork.Clock {
	return s.clock
}

func (s *PaymentService) Create(ctx context.Context, payment *models.Payment) error {
	if payment != nil && payment.RefundedPaymentID != nil && payment.ReversalKind != nil && *payment.ReversalKind == ReversalDisputeReversal && payment.Amount > 0 && PaymentStatusSucceeded(payment.Status) {
		return s.repo.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			d := s.repo.db.NewWithPgxTx(tx)
			scoped := NewPaymentService(d, s.clock)
			mid, err := merchant.Require(ctx)
			if err != nil {
				return err
			}
			if _, err := d.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: payment.CustomerID}); err != nil {
				return err
			}
			if err := scoped.repo.Create(ctx, payment); err != nil {
				return err
			}
			return scoped.syncPurchasedCreditRefund(ctx, *payment.RefundedPaymentID)
		})
	}
	return s.repo.Create(ctx, payment)
}

func (s *PaymentService) CreateIfNotExists(ctx context.Context, payment *models.Payment) (bool, error) {
	return s.repo.CreateIfNotExists(ctx, payment)
}

func (s *PaymentService) GetByID(ctx context.Context, id uuid.UUID) (*models.Payment, error) {
	return s.repo.GetByID(ctx, id)
}

// AttachRelations loads each payment's price, product and subscription.
func (s *PaymentService) AttachRelations(ctx context.Context, payments ...*models.Payment) error {
	return s.repo.AttachRelations(ctx, payments...)
}

func (s *PaymentService) GetByIDWithDetails(ctx context.Context, id uuid.UUID) (*models.Payment, []*models.Payment, error) {
	return s.repo.GetByIDWithDetails(ctx, id)
}

func (s *PaymentService) GetByUserID(ctx context.Context, userID string) ([]*models.Payment, error) {
	return s.repo.GetByUserID(ctx, userID)
}

func (s *PaymentService) GetByPSPTransactionID(ctx context.Context, rail models.Rail, transactionID string) (*models.Payment, error) {
	return s.repo.GetByPSPTransactionID(ctx, rail, transactionID)
}

func (s *PaymentService) GetManualByTransactionID(ctx context.Context, transactionID string) (*models.Payment, error) {
	return s.repo.GetManualByTransactionID(ctx, transactionID)
}

func (s *PaymentService) Update(ctx context.Context, payment *models.Payment) error {
	return errors.New("payments are immutable; updates are not supported")
}

func (s *PaymentService) Delete(ctx context.Context, id uuid.UUID) error {
	return errors.New("payments cannot be deleted")
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
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var refund *models.Payment
	err = s.repo.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Serialize provider facts against the original charge. Different webhook
		// event IDs can describe the same refund; validation and replay lookup must
		// share the lock so neither duplicate insertion nor double counting races.
		transactionDB := s.repo.db.NewWithPgxTx(tx)
		original, err := NewPaymentService(transactionDB, s.clock).GetByID(ctx, originalPaymentID)
		if err != nil {
			return err
		}
		if _, err := transactionDB.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: original.CustomerID}); err != nil {
			return err
		}
		if _, err := transactionDB.Gen(ctx).LockPaymentForRefund(ctx, gen.LockPaymentForRefundParams{MerchantID: mid.UUID(), PaymentID: originalPaymentID}); err != nil {
			return err
		}
		scoped := NewPaymentService(transactionDB, s.clock)
		refund, err = scoped.refundLocked(ctx, originalPaymentID, refundTransactionID, amount, reversalKind)
		if err != nil {
			return err
		}
		return scoped.syncPurchasedCreditRefund(ctx, originalPaymentID)
	})
	return refund, err
}

// ErrRefundReservationPending defers a provider refund fact while an OpenRails
// refund of the same payment is unresolved.
var ErrRefundReservationPending = errors.New("an OpenRails refund of this payment is still in flight; retry")

func (s *PaymentService) refundLocked(ctx context.Context, originalPaymentID uuid.UUID, refundTransactionID string, amount int64, reversalKind string) (*models.Payment, error) {
	if reversalKind != ReversalRefund && reversalKind != ReversalChargeback && reversalKind != ReversalDisputeReversal {
		return nil, fmt.Errorf("invalid reversal kind %q", reversalKind)
	}
	orig, err := s.GetByID(ctx, originalPaymentID)
	if err != nil {
		return nil, err
	}
	if orig.PspID != nil {
		ctx = db.WithPSPID(ctx, *orig.PspID)
	}
	existing, err := s.GetByPSPTransactionID(ctx, orig.Rail, refundTransactionID)
	if err == nil {
		if existing.RefundedPaymentID == nil || *existing.RefundedPaymentID != orig.ID || existing.Amount != -amount || existing.ReversalKind == nil || *existing.ReversalKind != reversalKind || !PaymentStatusSucceeded(existing.Status) {
			return nil, errors.New("refund transaction id is already bound to different payment facts")
		}
		return existing, nil
	}
	if !db.IsNotFound(err) {
		return nil, err
	}
	// While an OpenRails refund of this payment is in flight, a provider
	// refund fact may be that same refund; recording it now would count it
	// twice. The caller retries once the reservation resolves.
	if reversalKind == ReversalRefund {
		refunds, err := s.repo.ListRefunds(ctx, orig.ID)
		if err != nil {
			return nil, err
		}
		for _, r := range refunds {
			if r.Amount < 0 && strings.EqualFold(strings.TrimSpace(r.Status), PaymentStatusPendingValue) {
				return nil, ErrRefundReservationPending
			}
		}
	}
	if err := s.ValidateRefund(ctx, orig, amount); err != nil {
		return nil, err
	}
	if strings.TrimSpace(refundTransactionID) == "" {
		return nil, errors.New("refund transaction id is required")
	}

	refund := &models.Payment{
		ID:             uuidutil.NewV7(),
		CustomerID:     orig.CustomerID,
		PriceID:        orig.PriceID,
		SubscriptionID: orig.SubscriptionID,
		OrderID:        orig.OrderID,
		InvoiceID:      orig.InvoiceID,
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
		Status:        PaymentStatusSucceededValue,
		ReversalKind:  &reversalKind,
		// The reversal settled at the rail; the feed excludes it on
		// amount/refunded_payment_id, not on this marker.
		MoneyMovement: models.MoneyMovementRail,
		PurchasedAt:   s.now(),
		CreatedAt:     s.now(),
	}
	if err := s.Create(ctx, refund); err != nil {
		return nil, err
	}
	return refund, nil
}

func (s *PaymentService) ReserveRefund(ctx context.Context, originalPaymentID uuid.UUID, reservationTransactionID string, amount int64, metadata map[string]any) (*models.Payment, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var reservation *models.Payment
	err = s.repo.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.repo.db.NewWithPgxTx(tx)
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
		reservation, err = scoped.reserveRefundLocked(ctx, originalPaymentID, reservationTransactionID, amount, metadata)
		return err
	})
	return reservation, err
}

func (s *PaymentService) reserveRefundLocked(ctx context.Context, originalPaymentID uuid.UUID, reservationTransactionID string, amount int64, metadata map[string]any) (*models.Payment, error) {
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
		PriceID:        orig.PriceID,
		SubscriptionID: orig.SubscriptionID,
		OrderID:        orig.OrderID,
		InvoiceID:      orig.InvoiceID,
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
		Status:        PaymentStatusPendingValue,
		ReversalKind:  &kind,
		Metadata:      metadata,
		PurchasedAt:   now,
		CreatedAt:     now,
	}
	if err := s.Create(ctx, refund); err != nil {
		return nil, err
	}
	return refund, nil
}

func (s *PaymentService) GetRefundByAdminIdempotencyKey(ctx context.Context, originalPaymentID uuid.UUID, key string) (*models.Payment, error) {
	return s.repo.GetRefundByAdminIdempotencyKey(ctx, originalPaymentID, key)
}

func (s *PaymentService) CompleteRefundReservation(ctx context.Context, reservationID uuid.UUID, refundTransactionID string, metadata map[string]any) (*models.Payment, error) {
	if strings.TrimSpace(refundTransactionID) == "" {
		return nil, errors.New("refund transaction id is required")
	}
	var result *models.Payment
	err := s.repo.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.repo.db.NewWithPgxTx(tx)
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
		if err := scoped.repo.CompleteRefundReservation(ctx, reservationID, strings.TrimSpace(refundTransactionID), metadata); err != nil {
			return err
		}
		if reservation.RefundedPaymentID != nil {
			if err := scoped.syncPurchasedCreditRefund(ctx, *reservation.RefundedPaymentID); err != nil {
				return err
			}
		}
		result, err = scoped.GetByID(ctx, reservationID)
		return err
	})
	return result, err
}

// syncPurchasedCreditRefund uses each settled reversal's identity. A dispute
// recovery may restore only the credit withdrawn by that dispute; aggregating
// cash reversals would accidentally undo unrelated voluntary refunds.
func (s *PaymentService) syncPurchasedCreditRefund(ctx context.Context, paymentID uuid.UUID) error {
	rows, err := s.repo.ListRefunds(ctx, paymentID)
	if err != nil {
		return err
	}
	credits := purchasedcredits.New(s.repo.db, s.clock)
	// ListRefunds is newest first. Apply negative events oldest first, then
	// their positive recoveries, so every recovery finds its exact effect.
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		if PaymentStatusSucceeded(row.Status) && row.Amount < 0 {
			if err := credits.ApplyReversal(ctx, paymentID, row.ID, nil); err != nil {
				return err
			}
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
		if err := credits.ApplyReversal(ctx, paymentID, row.ID, reversed); err != nil {
			return err
		}
	}
	return nil
}

func (s *PaymentService) ReserveProviderAttempt(ctx context.Context, payment *models.Payment) (*models.Payment, error) {
	if payment == nil {
		return nil, errors.New("payment attempt is required")
	}
	if strings.TrimSpace(payment.TransactionID) == "" {
		return nil, errors.New("payment attempt transaction id is required")
	}
	if payment.Amount <= 0 {
		return nil, errors.New("payment attempt amount must be > 0")
	}
	now := s.now()
	if payment.ID == uuid.Nil {
		payment.ID = uuidutil.NewV7()
	}
	if payment.Status == "" {
		payment.Status = PaymentStatusPendingValue
	}
	if payment.PurchasedAt.IsZero() {
		payment.PurchasedAt = now
	}
	if payment.CreatedAt.IsZero() {
		payment.CreatedAt = now
	}
	created, err := s.CreateIfNotExists(ctx, payment)
	if err != nil {
		return nil, err
	}
	if created {
		return payment, nil
	}
	return s.GetByPSPTransactionID(ctx, payment.Rail, payment.TransactionID)
}

func (s *PaymentService) GetByNMISubscriptionOrder(ctx context.Context, orderID string) (*models.Payment, error) {
	return s.repo.GetByNMISubscriptionOrder(ctx, orderID)
}

func (s *PaymentService) GetByStripeInvoice(ctx context.Context, invoiceID string) (*models.Payment, error) {
	return s.repo.GetByStripeInvoice(ctx, invoiceID)
}

func (s *PaymentService) CompleteProviderAttempt(ctx context.Context, attemptID uuid.UUID, providerTransactionID string, metadata map[string]any) (*models.Payment, error) {
	if strings.TrimSpace(providerTransactionID) == "" {
		return nil, errors.New("provider transaction id is required")
	}
	if err := s.repo.CompleteProviderAttempt(ctx, attemptID, providerTransactionID, metadata); err != nil {
		return nil, err
	}
	return s.GetByID(ctx, attemptID)
}

func (s *PaymentService) CompleteProviderAttemptInPlace(ctx context.Context, attemptID uuid.UUID, metadata map[string]any) (*models.Payment, error) {
	if err := s.repo.CompleteProviderAttemptInPlace(ctx, attemptID, metadata); err != nil {
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
	// An invoice's payment settled a ledger debt; refunding it would reopen
	// the debt, which the ledger does not do.
	if orig.InvoiceID != nil {
		return errors.New("an invoice's payment is not refundable")
	}

	refundedTotal, err := s.repo.GetRefundTotalByPaymentID(ctx, orig.ID)
	if err != nil {
		return fmt.Errorf("failed to calculate refunded total: %w", err)
	}
	if amount > orig.Amount {
		return errors.New("refund amount cannot exceed original payment amount")
	}
	if refundedTotal > 0 {
		if amount+refundedTotal > orig.Amount {
			return fmt.Errorf("refund total would exceed original payment (refunded %d of %d)", refundedTotal, orig.Amount)
		}
	}
	return nil
}

func PaymentStatusSucceeded(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", PaymentStatusSucceededValue:
		return true
	default:
		return false
	}
}

func (s *PaymentService) GetRefundTotalByPaymentID(ctx context.Context, paymentID uuid.UUID) (int64, error) {
	return s.repo.GetRefundTotalByPaymentID(ctx, paymentID)
}

func (s *PaymentService) LinkRefundedPayment(ctx context.Context, paymentID, originalPaymentID uuid.UUID) error {
	return s.repo.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.repo.db.NewWithPgxTx(tx)
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
		if err := scoped.repo.LinkRefundedPayment(ctx, paymentID, originalPaymentID); err != nil {
			return err
		}
		return scoped.syncPurchasedCreditRefund(ctx, originalPaymentID)
	})
}

// ListPage is one page of payments, newest first.
func (s *PaymentService) ListPage(ctx context.Context, p billing.PaymentListParams) (billing.ListPage[*models.Payment], error) {
	return s.repo.ListPage(ctx, p)
}

func (s *PaymentService) GetLatestChargeBySubscriptionID(ctx context.Context, subscriptionID uuid.UUID) (*models.Payment, error) {
	return s.repo.GetLatestChargeBySubscriptionID(ctx, subscriptionID)
}

func (s *PaymentService) MarkFailed(ctx context.Context, id uuid.UUID) error {
	payment, err := s.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if payment.Amount >= 0 || payment.RefundedPaymentID == nil {
		return s.repo.MarkFailed(ctx, id)
	}
	return s.repo.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.repo.db.NewWithPgxTx(tx)
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
		if err := scoped.repo.MarkFailed(ctx, id); err != nil {
			return err
		}
		if _, err := d.Gen(ctx).GetPurchasedCreditGrant(ctx, gen.GetPurchasedCreditGrantParams{MerchantID: mid.UUID(), PaymentID: *payment.RefundedPaymentID}); err != nil {
			if db.IsNotFound(err) {
				return nil
			}
			return err
		}
		// Releasing a pending refund must not expose a lot which expired while
		// the provider result was unresolved. Retire it in the same transaction.
		ledger := grants.New(d.Gen(ctx), mid.UUID())
		ledger.SetClock(s.now)
		_, err = ledger.ExpireLapsed(ctx, payment.CustomerID, payment.Currency)
		return err
	})
}

// RefundTotals reports the succeeded refunds against each listed charge.
func (s *PaymentService) RefundTotals(ctx context.Context, paymentIDs []uuid.UUID) (map[uuid.UUID]int64, error) {
	return s.repo.RefundTotals(ctx, paymentIDs)
}
