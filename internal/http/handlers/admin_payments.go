package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/processors"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/query"
	log "github.com/sirupsen/logrus"
	"github.com/uptrace/bun"
)

type paymentPath struct {
	PaymentID string `uri:"id" binding:"required"`
}

type refundRequest struct {
	Amount int64  `json:"amount" binding:"required,gt=0"`
	Reason string `json:"reason,omitempty"`
}

const adminRefundIdempotencyHeader = "X-Idempotency-Key"

var adminRefundLocks sync.Map

func lockAdminRefund(paymentID string) func() {
	value, _ := adminRefundLocks.LoadOrStore(paymentID, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func adminRefundLockKey(paymentID string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("admin_refund:" + paymentID))
	return int64(h.Sum64())
}

type adminOffChannelPaymentPath struct {
	UserID string `uri:"user_id" binding:"required"`
}

type adminOffChannelPaymentRequest struct {
	PriceID          string         `json:"price_id" binding:"required"`
	TransactionID    string         `json:"transaction_id" binding:"required"`
	Amount           *int64         `json:"amount,omitempty"`
	Currency         string         `json:"currency,omitempty"`
	PurchasedAt      string         `json:"purchased_at,omitempty"`
	DiscountCode     *string        `json:"discount_code,omitempty"`
	DiscountReason   *string        `json:"discount_reason,omitempty"`
	DiscountMetadata map[string]any `json:"discount_metadata,omitempty"`
}

func AdminRefundPayment(r *httprequest.Request) {
	var path paymentPath
	if err := r.ShouldBindURI(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	paymentID, err := api.ParsePaymentID(path.PaymentID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid payment ID")
		return
	}
	var req refundRequest
	if !r.BindJSON(&req) {
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header(adminRefundIdempotencyHeader))
	if idempotencyKey == "" {
		r.ErrorJSON(http.StatusBadRequest, adminRefundIdempotencyHeader+" is required")
		return
	}
	unlock := lockAdminRefund(paymentID.String())
	defer unlock()

	refund, refundTransactionID, err := executeAdminRefund(r.Request.Context(), r, paymentID, req, idempotencyKey)
	if err != nil {
		status, message := adminRefundErrorResponse(err)
		if refundTransactionID != "" && status == http.StatusInternalServerError {
			message = "refund issued but failed to record"
		}
		log.WithError(err).WithFields(log.Fields{
			"payment_id":            paymentID,
			"status":                status,
			"refund_transaction_id": refundTransactionID,
		}).Warn("admin refund request failed")
		r.ErrorJSON(status, message)
		return
	}
	r.JSON(http.StatusCreated, PaymentToAPI(refund, nil))
}

func executeAdminRefund(ctx context.Context, r *httprequest.Request, paymentID uuid.UUID, req refundRequest, idempotencyKey string) (*models.Payment, string, error) {
	if r.State.DB == nil {
		prepared, err := prepareAdminRefund(ctx, r, r.State.PaymentService, paymentID, req, idempotencyKey)
		if err != nil {
			return nil, "", err
		}
		return issuePreparedAdminRefund(ctx, r, r.State.PaymentService, prepared, req, idempotencyKey)
	}
	var prepared *adminRefundPrepared
	err := r.State.DB.Q(r.Request.Context()).RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(?)", adminRefundLockKey(paymentID.String())); err != nil {
			return fmt.Errorf("lock refund: %w", err)
		}
		paymentService := payments.NewPaymentService(db.NewWithTx(tx), r.Clock)
		result, err := prepareAdminRefund(ctx, r, paymentService, paymentID, req, idempotencyKey)
		if err != nil {
			return err
		}
		prepared = result
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return issuePreparedAdminRefund(ctx, r, r.State.PaymentService, prepared, req, idempotencyKey)
}

type adminRefundStatusError struct {
	Status  int
	Message string
}

func (e *adminRefundStatusError) Error() string { return e.Message }

func adminRefundHTTPError(status int, message string) error {
	return &adminRefundStatusError{Status: status, Message: message}
}

func adminRefundErrorResponse(err error) (int, string) {
	var statusErr *adminRefundStatusError
	if errors.As(err, &statusErr) {
		return statusErr.Status, statusErr.Message
	}
	return http.StatusInternalServerError, "refund request failed"
}

type adminRefundPrepared struct {
	existing             *models.Payment
	payment              *models.Payment
	reservation          *models.Payment
	stripeRefundTargetID string
	nmiClient            *nmi.NMIClient
}

func prepareAdminRefund(ctx context.Context, r *httprequest.Request, paymentService *payments.PaymentService, paymentID uuid.UUID, req refundRequest, idempotencyKey string) (*adminRefundPrepared, error) {
	payment, err := paymentService.GetByID(ctx, paymentID)
	if err != nil {
		return nil, adminRefundHTTPError(http.StatusNotFound, "payment not found")
	}
	if existing, err := paymentService.GetRefundByAdminIdempotencyKey(ctx, paymentID, idempotencyKey); err == nil {
		if !adminRefundMatchesRequest(existing, req) {
			return nil, adminRefundHTTPError(http.StatusConflict, "idempotency key was already used for a different refund request")
		}
		switch strings.ToLower(strings.TrimSpace(existing.Status)) {
		case "", "completed":
			return &adminRefundPrepared{existing: existing}, nil
		case "pending":
			return nil, adminRefundHTTPError(http.StatusConflict, "refund request is already pending")
		default:
			return nil, adminRefundHTTPError(http.StatusConflict, "refund request already failed; retry with a new idempotency key")
		}
	} else if !repo.IsNotFound(err) {
		return nil, fmt.Errorf("load existing refund request: %w", err)
	}
	if err := paymentService.ValidateRefund(ctx, payment, req.Amount); err != nil {
		return nil, adminRefundHTTPError(http.StatusBadRequest, err.Error())
	}

	prepared := &adminRefundPrepared{payment: payment}
	var stripeRefundTargetID string
	var nmiClient *nmi.NMIClient
	switch {
	case payment.Processor == models.ProcessorCCBill:
		return nil, adminRefundHTTPError(http.StatusBadRequest, "CCBill refunds must be processed through CCBill's admin portal. After issuing the refund in CCBill, it will be recorded automatically via webhook.")
	case payment.Processor == models.ProcessorStripe:
		refundTargetID, err := subscriptions.ResolveStripeRefundTarget(payment)
		if err != nil {
			return nil, adminRefundHTTPError(http.StatusBadRequest, "payment cannot be refunded: "+err.Error())
		}
		stripeRefundTargetID = refundTargetID
	case processors.IsNMIBackedProcessor(payment.Processor):
		providerName := strings.ToLower(string(payment.Processor))
		client, ok := r.State.NMIClients[providerName]
		if !ok {
			return nil, adminRefundHTTPError(http.StatusInternalServerError, "payment processor not configured")
		}
		nmiClient = client
	default:
		return nil, adminRefundHTTPError(http.StatusBadRequest, fmt.Sprintf("refunds not supported for processor: %s", payment.Processor))
	}
	prepared.stripeRefundTargetID = stripeRefundTargetID
	prepared.nmiClient = nmiClient

	reservationMetadata := adminRefundMetadata(idempotencyKey, req, "pending", "")
	reservation, err := paymentService.ReserveRefund(ctx, paymentID, adminRefundReservationTransactionID(paymentID, idempotencyKey), req.Amount, reservationMetadata)
	if err != nil {
		return nil, fmt.Errorf("reserve refund: %w", err)
	}
	prepared.reservation = reservation
	return prepared, nil
}

func adminRefundMatchesRequest(existing *models.Payment, req refundRequest) bool {
	if existing == nil {
		return false
	}
	amount := existing.Amount
	if amount < 0 {
		amount = -amount
	}
	if amount != req.Amount {
		return false
	}
	return strings.TrimSpace(adminRefundMetadataString(existing.Metadata, "admin_refund_reason")) == strings.TrimSpace(req.Reason)
}

func adminRefundMetadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, ok := metadata[key]
	if !ok || value == nil {
		return ""
	}
	str, ok := value.(string)
	if !ok {
		return ""
	}
	return str
}

func issuePreparedAdminRefund(ctx context.Context, r *httprequest.Request, paymentService *payments.PaymentService, prepared *adminRefundPrepared, req refundRequest, idempotencyKey string) (*models.Payment, string, error) {
	if prepared == nil {
		return nil, "", errors.New("refund preparation is required")
	}
	if prepared.existing != nil {
		return prepared.existing, prepared.existing.TransactionID, nil
	}
	if prepared.payment == nil || prepared.reservation == nil {
		return nil, "", errors.New("refund preparation is incomplete")
	}

	var refundTransactionID string
	switch {
	case prepared.payment.Processor == models.ProcessorStripe:
		stripeService := &subscriptions.StripeRefundService{Config: r.State.Config}
		result, err := stripeService.CreateRefund(ctx, subscriptions.RefundParams{ChargeID: prepared.stripeRefundTargetID, Amount: req.Amount, Reason: req.Reason, IdempotencyKey: adminRefundProviderIdempotencyKey(prepared.payment.ID, idempotencyKey)})
		if err != nil {
			_ = paymentService.MarkFailed(ctx, prepared.reservation.ID)
			return nil, "", adminRefundHTTPError(http.StatusBadGateway, "refund failed")
		}
		refundTransactionID = result.ID
	case processors.IsNMIBackedProcessor(prepared.payment.Processor):
		result, err := prepared.nmiClient.Refund(nmi.RefundParams{TransactionID: prepared.payment.TransactionID, Amount: req.Amount})
		if err != nil {
			_ = paymentService.MarkFailed(ctx, prepared.reservation.ID)
			return nil, "", adminRefundHTTPError(http.StatusBadGateway, "refund failed")
		}
		refundTransactionID = result.TransactionID
	default:
		_ = paymentService.MarkFailed(ctx, prepared.reservation.ID)
		return nil, "", adminRefundHTTPError(http.StatusBadRequest, fmt.Sprintf("refunds not supported for processor: %s", prepared.payment.Processor))
	}

	refund, err := paymentService.CompleteRefundReservation(ctx, prepared.reservation.ID, refundTransactionID, adminRefundMetadata(idempotencyKey, req, "completed", refundTransactionID))
	if err != nil {
		return nil, refundTransactionID, err
	}
	return refund, refundTransactionID, nil
}

func adminRefundReservationTransactionID(paymentID uuid.UUID, idempotencyKey string) string {
	return "admin_refund_reservation:" + paymentID.String() + ":" + adminRefundHash(idempotencyKey)
}

func adminRefundProviderIdempotencyKey(paymentID uuid.UUID, idempotencyKey string) string {
	return "admin_refund:" + paymentID.String() + ":" + adminRefundHash(idempotencyKey)
}

func adminRefundHash(value string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return hex.EncodeToString(sum[:16])
}

func adminRefundMetadata(idempotencyKey string, req refundRequest, status string, refundTransactionID string) map[string]any {
	metadata := map[string]any{
		"admin_refund_idempotency_key": strings.TrimSpace(idempotencyKey),
		"admin_refund_status":          status,
		"admin_refund_amount":          req.Amount,
	}
	if reason := strings.TrimSpace(req.Reason); reason != "" {
		metadata["admin_refund_reason"] = reason
	}
	if refundTransactionID != "" {
		metadata["provider_refund_id"] = refundTransactionID
	}
	return metadata
}

func GetAdminPayments(r *httprequest.Request) {
	queryOpts := query.QueryOptions[payments.GetPaymentsFilters]{Limit: 50, Offset: 0}
	if err := r.ShouldBindQuery(&queryOpts); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	payments, total, err := r.State.PaymentService.GetPayments(r.Request.Context(), queryOpts)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	paymentObjects := make([]api.PaymentObject, len(payments))
	for i, p := range payments {
		paymentObjects[i] = PaymentToAPI(p, nil)
	}
	r.SuccessJSONPaginated(paymentObjects, total, queryOpts.Limit, queryOpts.Offset)
}

func GetAdminPayment(r *httprequest.Request) {
	var path paymentPath
	if err := r.ShouldBindURI(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	paymentID, err := api.ParsePaymentID(path.PaymentID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid payment ID")
		return
	}
	payment, refunds, err := r.State.PaymentService.GetByIDWithDetails(r.Request.Context(), paymentID)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "payment not found")
		return
	}
	r.SuccessJSON(PaymentToAPI(payment, refunds))
}

func GetAdminUserPayments(r *httprequest.Request) {
	var path adminUserPath
	if err := r.ShouldBindURI(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	page := 1
	pageSize := 50
	if p := r.Query("page"); p != "" {
		if v, err := strconv.Atoi(p); err == nil && v > 0 {
			page = v
		}
	}
	if ps := r.Query("page_size"); ps != "" {
		if v, err := strconv.Atoi(ps); err == nil && v > 0 && v <= 200 {
			pageSize = v
		}
	}
	payments, total, err := r.State.PaymentService.GetPaginatedByUserID(r.Request.Context(), path.UserID, page, pageSize)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	data := make([]api.PaymentObject, len(payments))
	for i, p := range payments {
		data[i] = PaymentToAPI(p, nil)
	}
	offset := (page - 1) * pageSize
	hasMore := offset+len(data) < total
	r.JSON(http.StatusOK, map[string]interface{}{"object": "list", "data": data, "total": total, "limit": pageSize, "offset": offset, "has_more": hasMore})
}

func AdminCreateOffChannelPayment(r *httprequest.Request) {
	var path adminOffChannelPaymentPath
	if err := r.ShouldBindURI(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	var req adminOffChannelPaymentRequest
	if !r.BindJSON(&req) {
		return
	}
	priceID, err := api.ParsePriceID(strings.TrimSpace(req.PriceID))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid price_id")
		return
	}
	transactionID := strings.TrimSpace(req.TransactionID)
	if transactionID == "" {
		r.ErrorJSON(http.StatusBadRequest, "transaction_id is required")
		return
	}
	if req.Amount != nil && *req.Amount < 0 {
		r.ErrorJSON(http.StatusBadRequest, "amount must be >= 0")
		return
	}
	var purchasedAt *time.Time
	if strings.TrimSpace(req.PurchasedAt) != "" {
		tm, err := time.Parse(time.RFC3339, strings.TrimSpace(req.PurchasedAt))
		if err != nil {
			r.ErrorJSON(http.StatusBadRequest, "purchased_at must be RFC3339")
			return
		}
		tm = tm.UTC()
		purchasedAt = &tm
	}
	if existing, err := r.State.PaymentService.GetByTransactionID(r.Request.Context(), models.ProcessorManual, transactionID); err == nil {
		r.JSON(http.StatusOK, map[string]any{"payment_id": existing.ID.String(), "status": "exists"})
		return
	}
	amount := int64(0)
	if req.Amount != nil {
		amount = *req.Amount
	}
	result, err := r.State.CheckoutService.RegisterPurchase(r.Request.Context(), &payments.RegisterPurchaseRequest{UserID: path.UserID, PriceID: priceID, Processor: string(models.ProcessorManual), TransactionID: transactionID, Amount: amount, Currency: strings.TrimSpace(req.Currency), PurchasedAt: purchasedAt, DiscountCode: req.DiscountCode, DiscountReason: req.DiscountReason, DiscountMetadata: req.DiscountMetadata})
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.JSON(http.StatusCreated, map[string]any{"payment_id": result.PaymentID.String(), "entitlements": result.Entitlements, "delayed_start": result.DelayedStart, "eligibility": result.Eligibility})
}
