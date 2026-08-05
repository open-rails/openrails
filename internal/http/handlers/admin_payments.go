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
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/integrations/nmi"
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
	idempotencyKey := strings.TrimSpace(middleware.IdempotencyKeyFromRequest(r.Request))
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
	existing    *models.Payment
	payment     *models.Payment
	reservation *models.Payment
	// amountCents is req.Amount (micros) converted at the provider boundary:
	// NMI and Stripe both refund in cents (typed, #671). Exact conversion —
	// sub-cent micros are rejected in prepare, never rounded.
	amountCents          moneyutil.Cents
	stripeRefundTargetID string
	nmiClient            *nmi.NMIClient
	// CCBill refund coordinates: the DataLink voidOrRefundTransaction key
	// (subscription.rail_subscription_id) + the original transaction id.
	ccbillSubscriptionID string
	ccbillTransactionID  string
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
		switch strings.ToLower(strings.TrimSpace(existing.Status)) {
		case "", "completed":
			return &adminRefundPrepared{existing: existing}, nil
		case "pending":
			return nil, adminRefundHTTPError(http.StatusConflict, "refund request is already pending")
		default:
			return nil, adminRefundHTTPError(http.StatusConflict, "refund request already failed; retry with a new idempotency key")
		}
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

	prepared := &adminRefundPrepared{payment: payment, amountCents: moneyutil.Cents(amountCents)}
	var stripeRefundTargetID string
	var nmiClient *nmi.NMIClient
	var ccbillSubscriptionID, ccbillTransactionID string
	switch {
	case payment.Rail == models.RailCCBill:
		// #696: refund through OUR admin via the DataLink SMS choke point. The
		// refund action keys off the CCBill subscriptionId (carried on the
		// subscription row) + the payment's transaction id; the provider mutation
		// rides the intent ledger. (API refunds also require CCBill's
		// account-level refund feature; a -7 denial surfaces from the executor.)
		if payment.SubscriptionID == nil {
			return nil, ccbillManualRefundError("CCBill refunds are keyed off the linked subscription's CCBill subscription id, and this payment has no linked subscription")
		}
		sub, err := subscriptions.NewSubscriptionRepo(txDB).GetByID(ctx, *payment.SubscriptionID)
		if err != nil {
			return nil, adminRefundHTTPError(http.StatusBadRequest, "payment cannot be refunded: could not load subscription for the ccbill refund")
		}
		subID, txnID, err := ccbillRefundTarget(payment, sub)
		if err != nil {
			return nil, ccbillManualRefundError(err.Error())
		}
		ccbillSubscriptionID = subID
		ccbillTransactionID = txnID
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
		nmiClient = client
	default:
		return nil, adminRefundHTTPError(http.StatusBadRequest, fmt.Sprintf("refunds not supported for rail: %s", payment.Rail))
	}
	prepared.stripeRefundTargetID = stripeRefundTargetID
	prepared.nmiClient = nmiClient
	prepared.ccbillSubscriptionID = ccbillSubscriptionID
	prepared.ccbillTransactionID = ccbillTransactionID

	reservationMetadata := adminRefundMetadata(idempotencyKey, req, "pending", "")
	reservation, err := paymentService.ReserveRefund(ctx, paymentID, adminRefundReservationTransactionID(paymentID, idempotencyKey), req.Amount, reservationMetadata)
	if err != nil {
		return nil, fmt.Errorf("reserve refund: %w", err)
	}
	prepared.reservation = reservation
	return prepared, nil
}

// ccbillManualRefundError: this specific payment can't resolve its CCBill
// refund coordinates, so the API path is out — direct the operator to the
// manual fallback instead of a dead end.
func ccbillManualRefundError(why string) error {
	return adminRefundHTTPError(http.StatusBadRequest,
		"payment cannot be refunded via the API: "+why+" — refund it manually in the CCBill admin portal")
}

// ccbillRefundTarget derives the CCBill refund coordinates from a CCBill payment
// and its (already loaded) subscription: the CCBill subscriptionId (the DataLink
// voidOrRefundTransaction key, = subscription.rail_subscription_id) + the
// original transactionId. Both are required — refunding without the exact
// transaction id lets CCBill pick the latest charge on the subscription.
func ccbillRefundTarget(payment *models.Payment, sub *models.Subscription) (subscriptionID, transactionID string, err error) {
	transactionID = strings.TrimSpace(payment.TransactionID)
	if transactionID == "" {
		return "", "", errors.New("ccbill payment has no transaction id")
	}
	if sub == nil {
		return "", "", errors.New("ccbill payment has no linked subscription; cannot resolve the CCBill subscription id")
	}
	subscriptionID = strings.TrimSpace(sub.RailSubscriptionID)
	if subscriptionID == "" {
		return "", "", errors.New("subscription has no CCBill rail subscription id")
	}
	return subscriptionID, transactionID, nil
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

	var providerTarget, providerTransactionID string
	switch {
	case prepared.payment.Rail == models.RailStripe:
		providerTarget = prepared.stripeRefundTargetID
	case prepared.payment.Rail == models.RailCCBill:
		// CCBill refunds against the subscriptionId; the transaction id narrows
		// the refund to this specific charge.
		providerTarget = prepared.ccbillSubscriptionID
		providerTransactionID = prepared.ccbillTransactionID
	case rails.IsNMI(prepared.payment.Rail):
		providerTarget = prepared.payment.TransactionID
	default:
		// Unreachable by construction: prepareAdminRefund already rejects
		// unsupported rails before a reservation exists. Reaching here means the
		// issue-stage switch drifted from the prepare-stage guard — an internal
		// invariant violation, not a user error.
		cause := fmt.Errorf("rail %s reached refund issue stage unguarded", prepared.payment.Rail)
		log.WithError(cause).WithField("payment_id", prepared.payment.ID).Error("admin refund: issue-stage rail switch drifted from prepare-stage guard")
		if relErr := releaseAdminRefundReservation(ctx, paymentService, prepared.reservation.ID, cause); relErr != nil {
			return nil, 0, relErr
		}
		return nil, 0, adminRefundHTTPError(http.StatusInternalServerError, "refund processing error")
	}

	intentType, provider, intentKey, err := intents.RefundIntentFor(prepared.payment, providerTarget, prepared.amountCents, req.Reason)
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
	// The charge's own provenance is the only honest answer to "which account
	// refunds this?" — an off-rail payment has no provider to refund through.
	if prepared.payment.PspID == nil {
		return nil, 0, adminRefundHTTPError(http.StatusConflict, "payment carries no PSP; it was not taken through a provider and cannot be refunded on a rail")
	}
	pspID := *prepared.payment.PspID
	intent, err := r.State.IntentRunner().EnqueueAndExecute(ctx, intents.EnqueueParams{
		MerchantID:     tid.UUID(),
		Provider:       provider,
		IntentType:     intentType,
		SubscriptionID: prepared.payment.SubscriptionID,
		PaymentID:      &paymentRowID,
		// or#893: a refund is executed against the account that took the charge.
		PspID: pspID,
		Payload: intents.RefundPayload{
			OriginalPaymentID:     prepared.payment.ID,
			ReservationID:         prepared.reservation.ID,
			AmountCents:           prepared.amountCents,
			Reason:                strings.TrimSpace(req.Reason),
			ProviderTarget:        providerTarget,
			ProviderTransactionID: providerTransactionID,
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
