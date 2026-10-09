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
	"strings"
	"time"

	"github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/providerposture"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	log "github.com/sirupsen/logrus"
)

type paymentPath struct {
	PaymentID string `uri:"id" binding:"required"`
}

// RefundRequest names either an exact native amount or the full remaining
// refundable amount; exactly one is required.
type RefundRequest = billing.RefundPaymentParams

func validateRefund(req RefundRequest) error {
	switch {
	case req.Full && req.Amount != 0:
		return api.Coded(billing.CodeInvalidParam, "amount and full are mutually exclusive").WithParam("amount")
	case !req.Full && req.Amount <= 0:
		return api.Coded(billing.CodeInvalidParam, "amount must be a positive native amount, or full must be true").WithParam("amount")
	}
	return nil
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

// RefundPayment (POST /merchant/payments/{id}/refunds) refunds a charge.
func RefundPayment(r *httprequest.Request) {
	var path paymentPath
	if err := r.ShouldBindURI(&path); err != nil {
		r.ErrorCode(billing.CodeInvalidParam, err.Error())
		return
	}
	typedPaymentID, err := billing.ParsePaymentID(path.PaymentID)
	if err != nil || typedPaymentID.IsZero() {
		r.ErrorCode(billing.CodeInvalidParam, "invalid payment ID")
		return
	}
	paymentID := typedPaymentID.UUID()
	var req RefundRequest
	if !r.BindJSON(&req) {
		return
	}
	if err := validateRefund(req); err != nil {
		writeAdminRefundError(r, err)
		return
	}
	idempotencyKey := strings.TrimSpace(strings.TrimSpace(r.Header("Idempotency-Key")))
	if idempotencyKey == "" {
		r.ErrorCode(billing.CodeInvalidParam, adminRefundIdempotencyHeader+" is required")
		return
	}
	refund, status, err := executeAdminRefund(r.Request.Context(), r, paymentID, req, idempotencyKey)
	if err != nil {
		log.WithError(err).WithField("payment_id", paymentID).Warn("admin refund request failed")
		writeAdminRefundError(r, err)
		return
	}
	r.JSON(status, PaymentToAPI(refund, nil))
}

// writeAdminRefundError answers a refused refund by its code; anything else
// is an internal failure.
func writeAdminRefundError(r *httprequest.Request, err error) {
	var refusal *api.APIError
	if errors.As(err, &refusal) {
		r.APIError(refusal)
		return
	}
	r.InternalError("refund request failed", err)
}

func executeAdminRefund(ctx context.Context, r *httprequest.Request, paymentID uuid.UUID, req RefundRequest, idempotencyKey string) (*models.Payment, int, error) {
	if r.State.DB == nil {
		// The provider-side mutation rides the intent ledger, which lives in
		// the database; without one there is nothing durable to execute.
		return nil, 0, errors.New("refund ledger unavailable: runtime has no database")
	}
	if err := checkAdminRefundRail(ctx, r, paymentID, idempotencyKey, req.RevokeAccess); err != nil {
		return nil, 0, err
	}
	var prepared *adminRefundPrepared
	err := r.State.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := gen.New(tx).LockAdminRefund(ctx, adminRefundLockKey(paymentID.String())); err != nil {
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

// checkAdminRefundRail refuses a new refund whose rail cannot execute it,
// before anything is reserved. Provider credentials are resolved outside the
// reservation transaction: custody loads commit independently. A replay of an
// accepted key skips the check and reports the recorded refund.
func checkAdminRefundRail(ctx context.Context, r *httprequest.Request, paymentID uuid.UUID, idempotencyKey string, revokeAccess bool) error {
	paymentService := payments.NewPaymentService(r.State.DB, r.Clock)
	if _, err := paymentService.GetRefundByAdminIdempotencyKey(ctx, paymentID, idempotencyKey); err == nil {
		return nil
	} else if !db.IsNotFound(err) {
		return fmt.Errorf("load existing refund request: %w", err)
	}
	payment, err := paymentService.GetByID(ctx, paymentID)
	if err != nil {
		return api.Coded(codePaymentNotFound, "")
	}
	if revokeAccess && payment.SubscriptionID != nil {
		// Revoking a provider-billed membership's access ends it and deletes
		// the provider schedule; refused (nothing reserved) while provider
		// deletes are disarmed, or NMI would keep charging a revoked member.
		sub, err := subscriptions.NewSubscriptionRepo(r.State.DB).GetByID(ctx, *payment.SubscriptionID)
		if err != nil && !db.IsNotFound(err) {
			return fmt.Errorf("load refunded subscription: %w", err)
		}
		if err == nil && sub.Status.Live() {
			if _, err := subscriptions.RequireProviderCancelArmed(ctx, r.State.DB, sub, false); err != nil {
				if errors.Is(err, subscriptions.ErrProviderCancelHeld) {
					return api.Coded(billing.CodeProviderCancelHeld, err.Error())
				}
				return err
			}
		}
	}
	switch {
	case payment.Rail == models.RailCCBill:
		return api.Coded(refundCodeUnsupported, ccbill.ErrRefundUnsupported.Error())
	case payment.Rail == models.RailStripe:
		if _, _, err := subscriptions.RequireStripeSecretKey(ctx, r.State.RailConfigs); err != nil {
			return api.Coded(refundCodeRailUnavailable, "stripe refunds are unavailable: rail is not configured")
		}
	case rails.IsNMI(payment.Rail):
		mid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		client, ok, err := r.State.CollectionResolver.ResolveNMIClient(ctx, mid.UUID(), payment.PspID)
		if err != nil || !ok || client == nil {
			log.WithError(err).WithField("payment_id", paymentID).Warn("nmi refund rail unavailable")
			return api.Coded(refundCodeRailUnavailable, "nmi refunds are unavailable: the payment's rail account is not armed")
		}
		// A credential set already known to fail sandbox posture refuses here;
		// an unverified one is verified by the dispatch gate itself.
		if client.TestMode && !client.LoopbackFixture {
			if status, known := providerposture.Process().Lookup(client.PostureKey()); known && !status.Armed() {
				return api.Coded(refundCodeRailUnavailable, "nmi refunds are unavailable: sandbox posture is not verified ("+status.Verdict.String()+")")
			}
		}
	default:
		return api.Coded(refundCodeUnsupported, fmt.Sprintf("refunds not supported for rail: %s", payment.Rail))
	}
	return nil
}

// Refund refusal codes (openrails.ErrRefund*); each fixes its status.
const (
	codePaymentNotFound       = "payment_not_found"
	refundCodeRailUnavailable = "refund_rail_unavailable"
	refundCodeUnsupported     = "refund_unsupported"
	refundCodeNotRefundable   = "payment_not_refundable"
	refundCodeFailed          = "refund_failed"
)

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

func prepareAdminRefund(ctx context.Context, r *httprequest.Request, txDB *db.DB, paymentService *payments.PaymentService, paymentID uuid.UUID, req RefundRequest, idempotencyKey string) (*adminRefundPrepared, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	payment, err := paymentService.GetByID(ctx, paymentID)
	if err != nil {
		return nil, api.Coded(codePaymentNotFound, "")
	}
	if existing, err := paymentService.GetRefundByAdminIdempotencyKey(ctx, paymentID, idempotencyKey); err == nil {
		if !adminRefundMatchesRequest(existing, req) {
			return nil, api.Coded(billing.CodeIdempotencyKeyReused, "idempotency key was already used for a different refund request")
		}
		intent, err := txDB.Gen(ctx).GetProviderIntentByIdempotencyKey(ctx, gen.GetProviderIntentByIdempotencyKeyParams{
			MerchantID: mid.UUID(), IdempotencyKey: intents.RefundIdempotencyKey(paymentID, idempotencyKey),
		})
		if err == nil && (intent.PaymentID == nil || *intent.PaymentID != paymentID) {
			err = pgx.ErrNoRows
		}
		if err != nil {
			return nil, fmt.Errorf("load refund intent: %w", err)
		}
		return &adminRefundPrepared{reservationID: existing.ID, intentID: intent.ID}, nil
	} else if !db.IsNotFound(err) {
		return nil, fmt.Errorf("load existing refund request: %w", err)
	}
	if req.Full {
		refunded, err := paymentService.GetRefundTotalByPaymentID(ctx, paymentID)
		if err != nil {
			return nil, fmt.Errorf("load refunded total: %w", err)
		}
		if req.Amount = payment.Amount - refunded; req.Amount <= 0 {
			return nil, api.Coded(refundCodeNotRefundable, "payment has no remaining refundable amount")
		}
	}
	if err := paymentService.ValidateRefund(ctx, payment, req.Amount); err != nil {
		return nil, api.Coded(refundCodeNotRefundable, err.Error())
	}
	amountCents, err := refundAmountCents(payment.Currency, req.Amount)
	if err != nil {
		return nil, api.Coded(billing.CodeInvalidParam, err.Error()).WithParam("amount")
	}

	if payment.Rail == models.RailCCBill {
		return nil, api.Coded(refundCodeUnsupported, ccbill.ErrRefundUnsupported.Error())
	}
	var stripeRefundTargetID string
	if payment.Rail == models.RailStripe {
		refundTargetID, err := subscriptions.ResolveStripeRefundTarget(payment)
		if err != nil {
			return nil, api.Coded(refundCodeNotRefundable, "payment cannot be refunded: "+err.Error())
		}
		stripeRefundTargetID = refundTargetID
	}

	reservationMetadata := adminRefundMetadata(idempotencyKey, req, "pending", "")
	reservation, err := paymentService.ReserveRefund(ctx, paymentID, adminRefundReservationTransactionID(paymentID, idempotencyKey), req.Amount, reservationMetadata)
	if err != nil {
		return nil, fmt.Errorf("reserve refund: %w", err)
	}
	if payment.PspID == nil {
		return nil, api.Coded(refundCodeUnsupported, "payment carries no PSP; it cannot be refunded on a rail")
	}
	providerTarget := payment.TransactionID
	if payment.Rail == models.RailStripe {
		providerTarget = stripeRefundTargetID
	}
	intentType, provider, intentKey, err := intents.RefundIntentFor(payment, idempotencyKey)
	if err != nil {
		return nil, err
	}
	intent, err := intents.NewStoreGated(txDB, r.State.RateCeiling()).Enqueue(ctx, intents.EnqueueParams{
		MerchantID: mid.UUID(), Provider: provider, IntentType: intentType,
		SubscriptionID: payment.SubscriptionID, PaymentID: &payment.ID, PspID: *payment.PspID,
		Payload: intents.RefundPayload{OriginalPaymentID: payment.ID, ReservationID: reservation.ID,
			AmountCents: amountCents, Currency: payment.Currency, Reason: strings.TrimSpace(req.Reason), RevokeAccess: req.RevokeAccess,
			ProviderTarget: providerTarget},
		IdempotencyKey: intentKey, NextAttemptAt: r.Clock.Now().UTC(),
		Origin: intents.OriginAdmin, OriginReason: "admin refund request",
	})
	if err != nil {
		return nil, fmt.Errorf("enqueue refund intent: %w", err)
	}
	return &adminRefundPrepared{reservationID: reservation.ID, intentID: intent.ID}, nil
}

func adminRefundMatchesRequest(existing *models.Payment, req RefundRequest) bool {
	if existing == nil {
		return false
	}
	full, _ := existing.Metadata["admin_refund_full"].(bool)
	if full != req.Full {
		return false
	}
	amount := existing.Amount
	if amount < 0 {
		amount = -amount
	}
	if !req.Full && amount != req.Amount {
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
	if err := paymentService.AttachRelations(ctx, refund); err != nil {
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
		return nil, 0, api.Coded(refundCodeFailed, message)
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

func adminRefundMetadata(idempotencyKey string, req RefundRequest, status string, refundTransactionID string) map[string]any {
	metadata := map[string]any{
		"admin_refund_idempotency_key": strings.TrimSpace(idempotencyKey),
		"admin_refund_status":          status,
		"admin_refund_amount":          req.Amount,
		"admin_refund_revoke_access":   req.RevokeAccess,
	}
	if req.Full {
		metadata["admin_refund_full"] = true
	}
	if reason := strings.TrimSpace(req.Reason); reason != "" {
		metadata["admin_refund_reason"] = reason
	}
	if refundTransactionID != "" {
		metadata["provider_refund_id"] = refundTransactionID
	}
	return metadata
}

// ListPayments (GET /merchant/payments) is one page of the merchant's
// payments, newest first.
func ListPayments(r *httprequest.Request) {
	params, ok := paymentListParams(r)
	if !ok {
		return
	}
	if raw := strings.TrimSpace(r.Query("customer_id")); raw != "" {
		id, err := billing.ParseCustomerID(raw)
		if err != nil || id.IsZero() {
			r.APIError(api.Coded(billing.CodeInvalidQuery, "customer_id is invalid").WithParam("customer_id"))
			return
		}
		params.CustomerID = id
	}
	if params.IDs, ok = listIDs(r, billing.ParsePaymentID); !ok {
		return
	}
	writePaymentPage(r, params)
}

// ListMyPayments (GET /me/payments) is one page of the caller's payments,
// newest first.
func ListMyPayments(r *httprequest.Request) {
	customer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	params, ok := paymentListParams(r)
	if !ok {
		return
	}
	params.CustomerID = billing.CustomerID(customer)
	writePaymentPage(r, params)
}

// paymentListParams reads the filters both payment lists share.
func paymentListParams(r *httprequest.Request) (billing.PaymentListParams, bool) {
	page, ok := r.Page()
	if !ok {
		return billing.PaymentListParams{}, false
	}
	params := billing.PaymentListParams{PageRequest: page, Rail: strings.TrimSpace(r.Query("rail")), TransactionID: strings.TrimSpace(r.Query("transaction_id"))}
	for name, parse := range map[string]func(string) error{
		"subscription_id": func(v string) (err error) { params.SubscriptionID, err = billing.ParseSubscriptionID(v); return },
		"price_id":        func(v string) (err error) { params.PriceID, err = billing.ParsePriceID(v); return },
	} {
		if raw := strings.TrimSpace(r.Query(name)); raw != "" {
			if parse(raw) != nil {
				r.APIError(api.Coded(billing.CodeInvalidQuery, name+" is invalid").WithParam(name))
				return billing.PaymentListParams{}, false
			}
		}
	}
	if kind := billing.PaymentKind(strings.TrimSpace(r.Query("kind"))); kind != "" {
		switch kind {
		case billing.PaymentCharge, billing.PaymentRefund, billing.PaymentChargeback, billing.PaymentDisputeReversal:
			params.Kind = kind
		default:
			r.APIError(api.Coded(billing.CodeInvalidQuery, "kind is invalid").WithParam("kind"))
			return billing.PaymentListParams{}, false
		}
	}
	return params, true
}

func writePaymentPage(r *httprequest.Request, params billing.PaymentListParams) {
	page, err := r.State.PaymentService.ListPage(r.Request.Context(), params)
	if err != nil {
		writeRefusal(r, err, "failed to list payments")
		return
	}
	charges := make([]uuid.UUID, 0, len(page.Items))
	for _, p := range page.Items {
		if p.Amount > 0 && p.RefundedPaymentID == nil {
			charges = append(charges, p.ID)
		}
	}
	refunded, err := r.State.PaymentService.RefundTotals(r.Request.Context(), charges)
	if err != nil {
		r.InternalError("failed to read payment refunds", err)
		return
	}
	out := billing.ListPage[billing.Payment]{Items: make([]billing.Payment, 0, len(page.Items)), Next: page.Next}
	for _, p := range page.Items {
		out.Items = append(out.Items, paymentView(p, refunded[p.ID]))
	}
	r.SuccessJSON(out)
}

// GetPayment (GET /merchant/payments/{id}) reads one payment with its
// refunds.
func GetPayment(r *httprequest.Request) {
	id, err := billing.ParsePaymentID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid payment id").WithParam("id"))
		return
	}
	payment, refunds, err := r.State.PaymentService.GetByIDWithDetails(r.Request.Context(), id.UUID())
	if err != nil {
		r.ErrorCode(billing.CodeResourceNotFound, "payment not found")
		return
	}
	if refunds == nil {
		refunds = []*models.Payment{}
	}
	r.SuccessJSON(PaymentToAPI(payment, refunds))
}

// CreateOffChannelPayment (POST /merchant/customers/{customer_id}/payments/off-channel)
// records a purchase paid outside any rail: 201 with the payment, 200 when the
// same transaction_id was already recorded with the same terms, and 409
// idempotency_key_reused when it was recorded with other terms.
func CreateOffChannelPayment(r *httprequest.Request) {
	customer, ok := commerceCustomer(r, customerIDParam(r.Param("customer_id")))
	if !ok {
		return
	}
	var req billing.CreateOffChannelPaymentParams
	if !r.BindJSON(&req) {
		return
	}
	if req.PriceID.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "price_id is required").WithParam("price_id"))
		return
	}
	transactionID := strings.TrimSpace(req.TransactionID)
	if transactionID == "" || len(transactionID) > 255 {
		r.APIError(api.Coded(billing.CodeInvalidParam, "transaction_id of 1-255 bytes is required").WithParam("transaction_id"))
		return
	}
	if req.Amount != nil && *req.Amount < 0 {
		r.APIError(api.Coded(billing.CodeInvalidParam, "amount must not be negative").WithParam("amount"))
		return
	}
	var purchasedAt *time.Time
	if req.PurchasedAt != nil {
		at := req.PurchasedAt.UTC()
		purchasedAt = &at
	}
	currency := strings.ToUpper(strings.TrimSpace(req.Currency))
	ctx := r.Request.Context()
	if existing, err := r.State.PaymentService.GetManualByTransactionID(ctx, transactionID); err == nil {
		if !offChannelTermsMatch(existing, customer.UUID(), req.PriceID.UUID(), req.Amount, currency, purchasedAt) {
			r.ErrorCode(billing.CodeIdempotencyKeyReused, "transaction_id was already recorded with other terms")
			return
		}
		writeRecordedPayment(r, http.StatusOK, existing.ID)
		return
	}
	register := &payments.RegisterPurchaseRequest{UserID: customer.String(), PriceID: req.PriceID.UUID(), Channel: models.ChannelManual, TransactionID: transactionID,
		Currency: currency, PurchasedAt: purchasedAt, DiscountCode: req.DiscountCode, DiscountReason: req.DiscountReason, DiscountMetadata: req.DiscountMetadata}
	if req.Amount != nil {
		register.Amount, register.AmountProvided = *req.Amount, true
	}
	result, err := r.State.CheckoutService.RegisterPurchase(ctx, register)
	if err != nil {
		writeRefusal(r, err, "failed to record the payment")
		return
	}
	writeRecordedPayment(r, http.StatusCreated, result.PaymentID)
}

// offChannelTermsMatch reports whether a recorded off-channel payment is the
// one a retry describes.
func offChannelTermsMatch(p *models.Payment, customer, price uuid.UUID, amount *int64, currency string, purchasedAt *time.Time) bool {
	switch {
	case p.CustomerID != customer, p.PriceID != price:
		return false
	case amount != nil && *amount != p.Amount:
		return false
	case currency != "" && currency != p.Currency:
		return false
	case purchasedAt != nil && !purchasedAt.Truncate(time.Microsecond).Equal(p.PurchasedAt.UTC().Truncate(time.Microsecond)):
		return false
	}
	return true
}

func writeRecordedPayment(r *httprequest.Request, status int, id uuid.UUID) {
	payment, refunds, err := r.State.PaymentService.GetByIDWithDetails(r.Request.Context(), id)
	if err != nil {
		r.InternalError("failed to read the recorded payment", err)
		return
	}
	if refunds == nil {
		refunds = []*models.Payment{}
	}
	r.JSON(status, PaymentToAPI(payment, refunds))
}
