package payments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/query"
)

type PaymentService struct {
	repo  *repo.PaymentRepo
	clock clockwork.Clock
}

// EnsureSubjectID resolves-or-creates the payable tenant-subject id for a raw
// user/subject string (#364). See repo.EnsureTenantSubjectID for the identity
// scheme (UUID subjects are their own id; legacy subjects use a generated row).
func (s *PaymentService) EnsureSubjectID(ctx context.Context, userID string) (uuid.UUID, error) {
	return s.repo.EnsureSubjectID(ctx, userID)
}

const (
	PaymentStatusPendingValue   = "pending"
	PaymentStatusCompletedValue = "completed"
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

type GetPaymentsFilters = repo.PaymentFilters

func NewPaymentService(db *db.DB, clocks ...clockwork.Clock) *PaymentService {
	return &PaymentService{repo: repo.NewPaymentRepo(db), clock: timeutil.FirstClock(clocks...)}
}

func (s *PaymentService) SetClock(c clockwork.Clock) {
	s.clock = timeutil.FirstClock(c)
}

func (s *PaymentService) Clock() clockwork.Clock {
	return s.clock
}

func (s *PaymentService) Create(ctx context.Context, payment *models.Payment) error {
	return s.repo.Create(ctx, payment)
}

func (s *PaymentService) CreateIfNotExists(ctx context.Context, payment *models.Payment) (bool, error) {
	return s.repo.CreateIfNotExists(ctx, payment)
}

func (s *PaymentService) GetByID(ctx context.Context, id uuid.UUID) (*models.Payment, error) {
	return s.repo.GetByID(ctx, id)
}

// GetByIDWithDetails returns a payment with all related entities and any refund entries
func (s *PaymentService) GetByIDWithDetails(ctx context.Context, id uuid.UUID) (*models.Payment, []*models.Payment, error) {
	return s.repo.GetByIDWithDetails(ctx, id)
}

func (s *PaymentService) GetByUserID(ctx context.Context, userID string) ([]*models.Payment, error) {
	return s.repo.GetByUserID(ctx, userID)
}

func (s *PaymentService) GetByTransactionID(ctx context.Context, processor models.Processor, transactionID string) (*models.Payment, error) {
	return s.repo.GetByTransactionID(ctx, processor, transactionID)
}

func (s *PaymentService) GetByPriceID(ctx context.Context, priceID uuid.UUID) ([]*models.Payment, error) {
	return s.repo.GetByPriceID(ctx, priceID)
}

func (s *PaymentService) GetByProcessor(ctx context.Context, processor models.Processor) ([]*models.Payment, error) {
	return s.repo.GetByProcessor(ctx, processor)
}

func (s *PaymentService) Update(ctx context.Context, payment *models.Payment) error {
	return errors.New("payments are immutable; updates are not supported")
}

func (s *PaymentService) Delete(ctx context.Context, id uuid.UUID) error {
	return errors.New("payments cannot be deleted")
}

// Refund records a refund as a negative payment entry linked by transaction ID
// Note: Processors should handle the actual money movement; this persists the event.
// amount is in cents (smallest currency unit)
func (s *PaymentService) Refund(ctx context.Context, originalPaymentID uuid.UUID, refundTransactionID string, amount int64) (*models.Payment, error) {
	orig, err := s.GetByID(ctx, originalPaymentID)
	if err != nil {
		return nil, err
	}
	if err := s.ValidateRefund(ctx, orig, amount); err != nil {
		return nil, err
	}
	if strings.TrimSpace(refundTransactionID) == "" {
		return nil, errors.New("refund transaction id is required")
	}

	refund := &models.Payment{
		ID:              uuidutil.NewV7(),
		TenantSubjectID: orig.TenantSubjectID,
		PriceID:         orig.PriceID,
		SubscriptionID:  orig.SubscriptionID,
		RefundedPaymentID: func() *uuid.UUID {
			id := orig.ID
			return &id
		}(),
		Processor:     orig.Processor,
		TransactionID: refundTransactionID,
		Amount:        -amount,
		ListAmount:    orig.ListAmount,
		Currency:      orig.Currency,
		Status:        PaymentStatusCompletedValue,
		PurchasedAt:   s.now(),
		CreatedAt:     s.now(),
	}
	if err := s.Create(ctx, refund); err != nil {
		return nil, err
	}
	return refund, nil
}

func (s *PaymentService) ReserveRefund(ctx context.Context, originalPaymentID uuid.UUID, reservationTransactionID string, amount int64, metadata map[string]any) (*models.Payment, error) {
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
	refund := &models.Payment{
		ID:              uuidutil.NewV7(),
		TenantSubjectID: orig.TenantSubjectID,
		PriceID:         orig.PriceID,
		SubscriptionID:  orig.SubscriptionID,
		RefundedPaymentID: func() *uuid.UUID {
			id := orig.ID
			return &id
		}(),
		Processor:     orig.Processor,
		TransactionID: reservationTransactionID,
		Amount:        -amount,
		ListAmount:    orig.ListAmount,
		Currency:      orig.Currency,
		Status:        PaymentStatusPendingValue,
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
	if err := s.repo.CompleteRefundReservation(ctx, reservationID, strings.TrimSpace(refundTransactionID), metadata); err != nil {
		return nil, err
	}
	return s.GetByID(ctx, reservationID)
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
	return s.GetByTransactionID(ctx, payment.Processor, payment.TransactionID)
}

func (s *PaymentService) GetByMetadataValue(ctx context.Context, key, value string) (*models.Payment, error) {
	return s.repo.GetByMetadataValue(ctx, key, value)
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
	if !PaymentStatusCompleted(orig.Status) {
		return errors.New("only completed charge payments can be refunded")
	}
	if orig.Amount <= 0 || orig.RefundedPaymentID != nil {
		return errors.New("only successful charge payments can be refunded")
	}
	// Synthetic transaction references are placeholders, not gateway charges:
	// "sub:" rows reference a subscription, and "*_attempt:" rows are NMI
	// checkout attempt anchors (the real charge is a separate payment row).
	for _, prefix := range []string{"sub:", "nmi_sub_attempt:", "nmi_sale_attempt:"} {
		if strings.HasPrefix(strings.TrimSpace(orig.TransactionID), prefix) {
			return errors.New("synthetic transaction references are not refundable")
		}
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

func PaymentStatusCompleted(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", PaymentStatusCompletedValue:
		return true
	default:
		return false
	}
}

func (s *PaymentService) GetRefundTotalByPaymentID(ctx context.Context, paymentID uuid.UUID) (int64, error) {
	return s.repo.GetRefundTotalByPaymentID(ctx, paymentID)
}

func (s *PaymentService) LinkRefundedPayment(ctx context.Context, paymentID, originalPaymentID uuid.UUID) error {
	return s.repo.LinkRefundedPayment(ctx, paymentID, originalPaymentID)
}

func (s *PaymentService) GetPaginatedByUserID(ctx context.Context, userID string, page, pageSize int) ([]*models.Payment, int, error) {
	return s.repo.GetPaginatedByUserID(ctx, userID, page, pageSize)
}

func (s *PaymentService) GetPayments(ctx context.Context, queryOpts query.QueryOptions[GetPaymentsFilters]) ([]*models.Payment, int64, error) {
	repoOpts := query.QueryOptions[repo.PaymentFilters]{
		Filters:  queryOpts.Filters,
		Limit:    queryOpts.Limit,
		Offset:   queryOpts.Offset,
		Page:     queryOpts.Page,
		PageSize: queryOpts.PageSize,
		All:      queryOpts.All,
	}

	return s.repo.GetPayments(ctx, repoOpts)
}

func (s *PaymentService) GetLatestByUserAndProcessor(ctx context.Context, userID string, processor models.Processor) (*models.Payment, error) {
	return s.repo.GetLatestByUserAndProcessor(ctx, userID, processor)
}

func (s *PaymentService) GetLatestBySubscriptionID(ctx context.Context, subscriptionID uuid.UUID) (*models.Payment, error) {
	return s.repo.GetLatestBySubscriptionID(ctx, subscriptionID)
}

func (s *PaymentService) GetLatestChargeBySubscriptionID(ctx context.Context, subscriptionID uuid.UUID) (*models.Payment, error) {
	return s.repo.GetLatestChargeBySubscriptionID(ctx, subscriptionID)
}

func (s *PaymentService) CountByUserAndProcessor(ctx context.Context, userID string, processor models.Processor) (successful int, failed int, err error) {
	return s.repo.CountByUserAndProcessor(ctx, userID, processor)
}

func (s *PaymentService) MarkFailed(ctx context.Context, id uuid.UUID) error {
	return s.repo.MarkFailed(ctx, id)
}
