package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
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
	idempotencyKey := strings.TrimSpace(r.Header(adminRefundIdempotencyHeader))
	if idempotencyKey == "" {
		r.ErrorJSON(http.StatusBadRequest, adminRefundIdempotencyHeader+" is required")
		return
	}
	unlock := lockAdminRefund(paymentID.String())
	defer unlock()

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
	if req.RevokeAccess {
		if err := revokeAccessForRefund(r, paymentID); err != nil {
			log.WithError(err).WithField("payment_id", paymentID).Error("admin refund succeeded but access revocation failed")
			r.ErrorJSON(http.StatusInternalServerError, "refund succeeded but access revocation failed")
			return
		}
	}
	r.JSON(status, PaymentToAPI(refund, nil))
}

func revokeAccessForRefund(r *httprequest.Request, paymentID uuid.UUID) error {
	if r.State.EntitlementService != nil {
		if err := r.State.EntitlementService.EndActiveByPayment(r.Request.Context(), paymentID, models.EntitlementRevokeRefund); err != nil {
			return err
		}
	}
	if svc := productAccessService(r); svc != nil {
		if _, err := svc.RevokeProductAccessByPayment(r.Request.Context(), paymentID, models.ProductAccessRevokeRefund); err != nil {
			return err
		}
	}
	return nil
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
		paymentService := payments.NewPaymentService(db.NewWithPgxTx(tx), r.Clock)
		result, err := prepareAdminRefund(ctx, r, paymentService, paymentID, req, idempotencyKey)
		if err != nil {
			return err
		}
		prepared = result
		return nil
	})
	if err != nil {
		return nil, 0, err
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
	case payment.Rail == models.RailCCBill:
		return nil, adminRefundHTTPError(http.StatusBadRequest, "CCBill refunds must be processed through CCBill's admin portal. After issuing the refund in CCBill, it will be recorded automatically via webhook.")
	case payment.Rail == models.RailStripe:
		refundTargetID, err := subscriptions.ResolveStripeRefundTarget(payment)
		if err != nil {
			return nil, adminRefundHTTPError(http.StatusBadRequest, "payment cannot be refunded: "+err.Error())
		}
		stripeRefundTargetID = refundTargetID
	case rails.IsNMIBackedRail(payment.Rail):
		providerName := strings.ToLower(string(payment.Rail))
		client, ok := r.State.NMIClients[providerName]
		if !ok {
			return nil, adminRefundHTTPError(http.StatusInternalServerError, "payment rail not configured")
		}
		nmiClient = client
	default:
		return nil, adminRefundHTTPError(http.StatusBadRequest, fmt.Sprintf("refunds not supported for rail: %s", payment.Rail))
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

// releaseAdminRefundReservation marks a refund reservation failed after the
// provider-side refund could not be issued (intent-build error, an unexpected
// rail reaching the issue stage, or a conflicting in-flight refund). A
// failure to release is surfaced as 500 reservation_release_failed rather than
// swallowed: a silently stranded `pending` reservation would otherwise block
// this idempotency key from ever retrying. cause is the original trigger.
func releaseAdminRefundReservation(ctx context.Context, paymentService *payments.PaymentService, reservationID uuid.UUID, cause error) error {
	if err := paymentService.MarkFailed(ctx, reservationID); err != nil {
		log.WithError(err).WithFields(log.Fields{
			"reservation_id": reservationID,
			"cause":          cause,
		}).Error("admin refund: could not release reservation after refund error; reservation stranded in pending")
		return adminRefundHTTPError(http.StatusInternalServerError, "reservation_release_failed")
	}
	return nil
}

// issuePreparedAdminRefund executes the provider-side money movement through
// the intent ledger (#358 phase B): enqueue an admin-origin refund intent and
// run it synchronously through the gate/execute/classify pipeline. The local
// reservation flow is unchanged — the intent handler's finalize completes the
// reservation on success (also when the success only lands later via the
// scheduled executor/verifier) and releases it on a terminal refusal.
func issuePreparedAdminRefund(ctx context.Context, r *httprequest.Request, paymentService *payments.PaymentService, prepared *adminRefundPrepared, req refundRequest, idempotencyKey string) (*models.Payment, int, error) {
	if prepared == nil {
		return nil, 0, errors.New("refund preparation is required")
	}
	if prepared.existing != nil {
		return prepared.existing, http.StatusCreated, nil
	}
	if prepared.payment == nil || prepared.reservation == nil {
		return nil, 0, errors.New("refund preparation is incomplete")
	}

	var providerTarget string
	switch {
	case prepared.payment.Rail == models.RailStripe:
		providerTarget = prepared.stripeRefundTargetID
	case rails.IsNMIBackedRail(prepared.payment.Rail):
		providerTarget = prepared.payment.TransactionID
	default:
		// Unreachable by construction: prepareAdminRefund already rejects CCBill
		// and unsupported rails before a reservation exists. Reaching here
		// means the issue-stage switch drifted from the prepare-stage guard —
		// an internal invariant violation, not a user error.
		cause := fmt.Errorf("rail %s reached refund issue stage unguarded", prepared.payment.Rail)
		log.WithError(cause).WithField("payment_id", prepared.payment.ID).Error("admin refund: issue-stage rail switch drifted from prepare-stage guard")
		if relErr := releaseAdminRefundReservation(ctx, paymentService, prepared.reservation.ID, cause); relErr != nil {
			return nil, 0, relErr
		}
		return nil, 0, adminRefundHTTPError(http.StatusInternalServerError, "refund processing error")
	}

	intentType, provider, intentKey, err := intents.RefundIntentFor(prepared.payment, providerTarget, req.Amount, req.Reason)
	if err != nil {
		log.WithError(err).WithField("payment_id", prepared.payment.ID).Warn("admin refund: building refund intent failed")
		if relErr := releaseAdminRefundReservation(ctx, paymentService, prepared.reservation.ID, err); relErr != nil {
			return nil, 0, relErr
		}
		return nil, 0, adminRefundHTTPError(http.StatusBadRequest, err.Error())
	}

	paymentRowID := prepared.payment.ID
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, 0, adminRefundHTTPError(http.StatusInternalServerError, "no merchant resolved on request")
	}
	intent, err := r.State.IntentRunner().EnqueueAndExecute(ctx, intents.EnqueueParams{
		MerchantID:     tid.UUID(),
		Provider:       provider,
		IntentType:     intentType,
		SubscriptionID: prepared.payment.SubscriptionID,
		PaymentID:      &paymentRowID,
		Payload: intents.RefundPayload{
			OriginalPaymentID: prepared.payment.ID,
			ReservationID:     prepared.reservation.ID,
			AmountCents:       req.Amount,
			Reason:            strings.TrimSpace(req.Reason),
			ProviderTarget:    providerTarget,
		},
		IdempotencyKey: intentKey,
		NextAttemptAt:  time.Now().UTC(),
		Origin:         intents.OriginAdmin,
		OriginReason:   "admin refund request",
	})
	if err != nil {
		return nil, 0, fmt.Errorf("enqueue refund intent: %w", err)
	}

	// The intent identity is content-addressed (payment + amount [+ reason]):
	// a conflict pointing at a DIFFERENT reservation means this exact refund
	// was already requested through another admin idempotency key. Never
	// re-execute it — release this reservation and surface the conflict.
	var intentPayload intents.RefundPayload
	if len(intent.Payload) > 0 {
		_ = json.Unmarshal(intent.Payload, &intentPayload)
	}
	if intentPayload.ReservationID != prepared.reservation.ID {
		cause := fmt.Errorf("content-addressed refund intent already bound to reservation %s", intentPayload.ReservationID)
		if relErr := releaseAdminRefundReservation(ctx, paymentService, prepared.reservation.ID, cause); relErr != nil {
			return nil, 0, relErr
		}
		if intent.Status == intents.StatusSucceeded {
			return nil, 0, adminRefundHTTPError(http.StatusConflict, "an identical refund (same payment, amount and reason) was already issued")
		}
		return nil, 0, adminRefundHTTPError(http.StatusConflict, "an identical refund request is already in progress")
	}

	switch intent.Status {
	case intents.StatusSucceeded:
		refund, err := paymentService.GetByID(ctx, prepared.reservation.ID)
		if err != nil {
			return nil, 0, fmt.Errorf("refund issued but failed to load: %w", err)
		}
		return refund, http.StatusCreated, nil
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
		reservation, err := paymentService.GetByID(ctx, prepared.reservation.ID)
		if err != nil {
			return nil, 0, fmt.Errorf("refund queued but reservation failed to load: %w", err)
		}
		return reservation, http.StatusAccepted, nil
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
	if existing, err := r.State.PaymentService.GetByTransactionID(r.Request.Context(), models.RailManual, transactionID); err == nil {
		r.JSON(http.StatusOK, map[string]any{"payment_id": existing.ID.String(), "status": "exists"})
		return
	}
	amount := int64(0)
	if req.Amount != nil {
		amount = *req.Amount
	}
	result, err := r.State.CheckoutService.RegisterPurchase(r.Request.Context(), &payments.RegisterPurchaseRequest{UserID: path.UserID, PriceID: priceID, Rail: string(models.RailManual), TransactionID: transactionID, Amount: amount, Currency: strings.TrimSpace(req.Currency), PurchasedAt: purchasedAt, DiscountCode: req.DiscountCode, DiscountReason: req.DiscountReason, DiscountMetadata: req.DiscountMetadata})
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.JSON(http.StatusCreated, map[string]any{"payment_id": result.PaymentID.String(), "entitlements": result.Entitlements, "delayed_start": result.DelayedStart, "eligibility": result.Eligibility})
}
