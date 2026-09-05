package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/open-rails/openrails/pkg/query"
	log "github.com/sirupsen/logrus"
)

type paymentPath struct {
	PaymentID string `uri:"id" binding:"required"`
}

type refundRequest struct {
	Amount       int64  `json:"amount" binding:"required,gt=0"`
	Reason       string `json:"reason,omitempty"`
	RevokeAccess bool   `json:"revoke_access,omitempty"`
}

const adminRefundIdempotencyHeader = "Idempotency-Key"

func adminRefundLockKey(paymentID string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("admin_refund:" + paymentID))
	// Mask to 63 bits so the FNV hash is always a non-negative int64. Advisory-lock
	// keys are opaque, so dropping the top bit is harmless and avoids any overflow.
	key, _ := safecast.Convert[int64](h.Sum64() & math.MaxInt64)
	return key
}

type adminOffChannelPaymentPath struct {
	UserID string `uri:"customer_id" binding:"required"`
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
	idempotencyKey := strings.TrimSpace(middleware.IdempotencyKeyFromRequest(r.Request))
	if idempotencyKey == "" {
		r.ErrorJSON(http.StatusBadRequest, adminRefundIdempotencyHeader+" is required")
		return
	}
	refund, status, err := executeAdminRefund(r.Request.Context(), r, paymentID, req, idempotencyKey)
	if err != nil {
		status, message := adminRefundErrorResponse(err)
		log.WithError(err).WithFields(log.Fields{
			"payment_id": paymentID,
			"status":     status,
		}).Warn("admin refund request failed")
		r.ErrorJSON(status, message)
		return
	}
	r.JSON(status, PaymentToAPI(refund, nil))
}

func executeAdminRefund(ctx context.Context, r *httprequest.Request, paymentID uuid.UUID, req refundRequest, idempotencyKey string) (*models.Payment, int, error) {
	if r.State.DB == nil {
		// The provider-side mutation rides the intent ledger, which lives in
		// the database; without one there is nothing durable to execute.
		return nil, 0, errors.New("refund ledger unavailable: runtime has no database")
	}
	var prepared *adminRefundPrepared
	err := r.State.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", adminRefundLockKey(paymentID.String())); err != nil {
			return fmt.Errorf("lock refund: %w", err)
		}
		txDB := db.NewWithPgxTx(tx)
		paymentService := payments.NewPaymentService(txDB, r.Clock)
		result, err := prepareAdminRefund(ctx, r, txDB, paymentService, paymentID, req, idempotencyKey)
		if err != nil {
			return err
		}
		prepared = result
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return issuePreparedAdminRefund(ctx, r, prepared)
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
	reservationID uuid.UUID
	intentID      uuid.UUID
}

// refundAmountCents converts an admin refund request amount (internal units at
// the PAYMENT's currency scale) to the provider minor amount. Refunds must be
// exact: a sub-minor remainder is an error, never rounded. Registry-driven
// (or#863) — a payment whose currency is blank or unregistered cannot be
// refunded at a guessed scale.
func refundAmountCents(currency string, amountNative int64) (moneyutil.Cents, error) {
	cents, err := moneyutil.NativeToRailMinorExact(currency, amountNative)
	if err != nil {
		return 0, fmt.Errorf("refund amount must be a whole number of cents: %w", err)
	}
	return cents, nil
}

func prepareAdminRefund(ctx context.Context, r *httprequest.Request, txDB *db.DB, paymentService *payments.PaymentService, paymentID uuid.UUID, req refundRequest, idempotencyKey string) (*adminRefundPrepared, error) {
	payment, err := paymentService.GetByID(ctx, paymentID)
	if err != nil {
		return nil, adminRefundHTTPError(http.StatusNotFound, "payment not found")
	}
	if existing, err := paymentService.GetRefundByAdminIdempotencyKey(ctx, paymentID, idempotencyKey); err == nil {
		if !adminRefundMatchesRequest(existing, req) {
			return nil, adminRefundHTTPError(http.StatusConflict, "idempotency key was already used for a different refund request")
		}
		var intentID uuid.UUID
		if err := txDB.Qx(ctx).QueryRow(ctx, `SELECT id FROM openrails.rail_intents WHERE payment_id=$1 AND idempotency_key=$2`, paymentID, intents.RefundIdempotencyKey(paymentID, idempotencyKey)).Scan(&intentID); err != nil {
			return nil, fmt.Errorf("load refund intent: %w", err)
		}
		return &adminRefundPrepared{reservationID: existing.ID, intentID: intentID}, nil
	} else if !db.IsNotFound(err) {
		return nil, fmt.Errorf("load existing refund request: %w", err)
	}
	if err := paymentService.ValidateRefund(ctx, payment, req.Amount); err != nil {
		return nil, adminRefundHTTPError(http.StatusBadRequest, err.Error())
	}
	amountCents, err := refundAmountCents(payment.Currency, req.Amount)
	if err != nil {
		return nil, adminRefundHTTPError(http.StatusBadRequest, err.Error())
	}

	var stripeRefundTargetID string
	switch {
	case payment.Rail == models.RailCCBill:
		return nil, adminRefundHTTPError(http.StatusBadRequest, ccbill.ErrRefundUnsupported.Error())
	case payment.Rail == models.RailStripe:
		refundTargetID, err := subscriptions.ResolveStripeRefundTarget(payment)
		if err != nil {
			return nil, adminRefundHTTPError(http.StatusBadRequest, "payment cannot be refunded: "+err.Error())
		}
		stripeRefundTargetID = refundTargetID
	case rails.IsNMI(payment.Rail):
		// #788: arm the ctx merchant's NMI client from the armed rail state
		// (the payment's stamped provenance account when present).
		mid, merr := merchant.Require(ctx)
		if merr != nil {
			return nil, adminRefundHTTPError(http.StatusInternalServerError, "payment rail not configured")
		}
		client, ok, cerr := r.State.CollectionResolver.ResolveNMIClient(ctx, mid.UUID(), payment.PspID)
		if cerr != nil || !ok || client == nil {
			return nil, adminRefundHTTPError(http.StatusInternalServerError, "payment rail not configured")
		}
	default:
		return nil, adminRefundHTTPError(http.StatusBadRequest, fmt.Sprintf("refunds not supported for rail: %s", payment.Rail))
	}

	reservationMetadata := adminRefundMetadata(idempotencyKey, req, "pending", "")
	reservation, err := paymentService.ReserveRefund(ctx, paymentID, adminRefundReservationTransactionID(paymentID, idempotencyKey), req.Amount, reservationMetadata)
	if err != nil {
		return nil, fmt.Errorf("reserve refund: %w", err)
	}
	if payment.PspID == nil {
		return nil, adminRefundHTTPError(http.StatusConflict, "payment carries no PSP; it cannot be refunded on a rail")
	}
	providerTarget := payment.TransactionID
	if payment.Rail == models.RailStripe {
		providerTarget = stripeRefundTargetID
	}
	intentType, provider, intentKey, err := intents.RefundIntentFor(payment, idempotencyKey)
	if err != nil {
		return nil, err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	intent, err := intents.NewStoreGated(txDB, r.State.RateCeiling()).Enqueue(ctx, intents.EnqueueParams{
		MerchantID: mid.UUID(), Provider: provider, IntentType: intentType,
		SubscriptionID: payment.SubscriptionID, PaymentID: &payment.ID, PspID: *payment.PspID,
		Payload: intents.RefundPayload{OriginalPaymentID: payment.ID, ReservationID: reservation.ID,
			AmountCents: amountCents, Reason: strings.TrimSpace(req.Reason), RevokeAccess: req.RevokeAccess,
			ProviderTarget: providerTarget},
		IdempotencyKey: intentKey, NextAttemptAt: r.Clock.Now().UTC(),
		Origin: intents.OriginAdmin, OriginReason: "admin refund request",
	})
	if err != nil {
		return nil, fmt.Errorf("enqueue refund intent: %w", err)
	}
	return &adminRefundPrepared{reservationID: reservation.ID, intentID: intent.ID}, nil
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
	revoke, _ := existing.Metadata["admin_refund_revoke_access"].(bool)
	return revoke == req.RevokeAccess && strings.TrimSpace(adminRefundMetadataString(existing.Metadata, "admin_refund_reason")) == strings.TrimSpace(req.Reason)
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

// The reservation and intent are already committed. Request cancellation cannot
// discard the work; the scheduled executor can finish the same operation.
func issuePreparedAdminRefund(ctx context.Context, r *httprequest.Request, prepared *adminRefundPrepared) (*models.Payment, int, error) {
	intent, err := r.State.IntentRunner().ExecuteByID(ctx, prepared.intentID)
	if err != nil {
		return nil, 0, fmt.Errorf("execute refund intent: %w", err)
	}
	paymentService := r.State.PaymentService
	// Finalization and access revocation commit together, before the intent's
	// success transition. Recover that outcome even if its lease expired there.
	refund, err := paymentService.GetByID(ctx, prepared.reservationID)
	if err != nil {
		return nil, 0, err
	}
	if payments.PaymentStatusCompleted(refund.Status) {
		return refund, http.StatusCreated, nil
	}
	switch intent.Status {
	case intents.StatusSucceeded:
		return nil, 0, errors.New("successful refund intent has an incomplete reservation")
	case intents.StatusFailedTerminal:
		message := "refund failed"
		if intent.LastFailureReason != nil && *intent.LastFailureReason != "" {
			message = "refund failed: " + *intent.LastFailureReason
		}
		return nil, 0, adminRefundHTTPError(http.StatusBadGateway, message)
	default:
		// Parked (mode/kill switch — deliberately not an error), ambiguous
		// (verifier resolving) or retryable: the durable intent finishes the
		// job and its finalize completes the reservation. Report 202 with the
		// pending reservation.
		log.WithFields(log.Fields{
			"intent_id":     intent.ID,
			"intent_status": intent.Status,
			"reason": func() string {
				if intent.LastFailureReason != nil {
					return *intent.LastFailureReason
				}
				return ""
			}(),
		}).Warn("admin refund queued on the provider intent ledger (not completed inline)")
		return refund, http.StatusAccepted, nil
	}
}

func adminRefundReservationTransactionID(paymentID uuid.UUID, idempotencyKey string) string {
	return "admin_refund_reservation:" + paymentID.String() + ":" + adminRefundHash(idempotencyKey)
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
		"admin_refund_revoke_access":   req.RevokeAccess,
	}
	if reason := strings.TrimSpace(req.Reason); reason != "" {
		metadata["admin_refund_reason"] = reason
	}
	if refundTransactionID != "" {
		metadata["provider_refund_id"] = refundTransactionID
	}
	return metadata
}

// adminPaymentsMaxLimit caps the admin payments list page size, mirroring
// adminCustomersMaxLimit (#785).
const adminPaymentsMaxLimit = 200

func GetAdminPayments(r *httprequest.Request) {
	queryOpts := query.QueryOptions[payments.GetPaymentsFilters]{Limit: 50, Offset: 0}
	if err := r.ShouldBindQuery(&queryOpts); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	// #785: validate + clamp pagination like ListAdminCustomers. Without this a
	// negative limit flows through to a 200 with an inconsistent
	// {"limit":-1,"has_more":true,…} envelope.
	if queryOpts.Limit <= 0 {
		r.ErrorJSON(http.StatusBadRequest, "limit must be a positive integer")
		return
	}
	if queryOpts.Offset < 0 {
		r.ErrorJSON(http.StatusBadRequest, "offset must be a non-negative integer")
		return
	}
	queryOpts.Limit = min(queryOpts.Limit, adminPaymentsMaxLimit)
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
	if existing, err := r.State.PaymentService.GetByTransactionID(r.Request.Context(), models.Rail(models.ChannelManual), transactionID); err == nil {
		r.JSON(http.StatusOK, map[string]any{"payment_id": existing.ID.String(), "status": "exists"})
		return
	}
	amount := int64(0)
	if req.Amount != nil {
		amount = *req.Amount
	}
	result, err := r.State.CheckoutService.RegisterPurchase(r.Request.Context(), &payments.RegisterPurchaseRequest{UserID: path.UserID, PriceID: priceID, Rail: string(models.ChannelManual), TransactionID: transactionID, Amount: amount, Currency: strings.TrimSpace(req.Currency), PurchasedAt: purchasedAt, DiscountCode: req.DiscountCode, DiscountReason: req.DiscountReason, DiscountMetadata: req.DiscountMetadata})
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.JSON(http.StatusCreated, map[string]any{"payment_id": result.PaymentID.String(), "entitlements": result.Entitlements, "delayed_start": result.DelayedStart, "eligibility": result.Eligibility})
}
