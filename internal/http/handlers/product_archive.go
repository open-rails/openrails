package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/reconcile/recommend"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// Product archive operations. The receipt fixes the product, action,
// resolved window and reason. Every call re-derives the qualifying purchases
// from the ledger, so replays converge: refunds use one deterministic key per
// operation and payment, and a purchase under review is one insert-if-absent
// finding per payment, resolved through the findings queue. Only rail money
// movement is refunded automatically; anything the refund path refuses
// becomes a finding instead of being dropped.

const (
	productArchiveFindingType = "life.product_archived_purchase"
	// productArchiveRefundBudget bounds provider calls per request; a replay
	// continues where the previous call stopped.
	productArchiveRefundBudget = 50
	productArchiveMaxReason    = 500
)

// purchaseReview is the finding that holds one archived purchase for the
// merchant's decision.
type purchaseReview struct {
	ID       billing.FindingID
	Detail   string
	RefundID *billing.PaymentID
	// Outcome is review_open, review_refunded or review_dismissed.
	Outcome billing.ArchivedPurchaseOutcome
}

type productArchiveOperation struct {
	ID             uuid.UUID
	ProductID      uuid.UUID
	ProductKey     string
	Action         billing.PurchaseAction
	PurchaseWindowStartsAt *time.Time
	Reason         string
	CreatedAt      time.Time
}

// productArchiveFingerprint canonicalizes the caller's terms. A relative
// window is part of the identity, never the instant it resolved to, so a
// replay matches.
func productArchiveFingerprint(req billing.ArchiveProductParams) []byte {
	canonical := map[string]string{
		"product_id": req.ProductID.String(), "product_key": strings.TrimSpace(req.ProductKey),
		"action": string(req.PurchaseAction), "reason": strings.TrimSpace(req.Reason),
	}
	if !req.PurchaseWindowStartsAt.IsZero() {
		canonical["purchase_window_starts_at"] = req.PurchaseWindowStartsAt.UTC().Format(time.RFC3339Nano)
	}
	if req.WindowSeconds != 0 {
		canonical["window_seconds"] = strconv.FormatInt(req.WindowSeconds, 10)
	}
	raw, _ := json.Marshal(canonical)
	sum := sha256.Sum256(raw)
	return sum[:]
}

func validateProductArchive(req *billing.ArchiveProductParams) *api.APIError {
	invalid := func(param, msg string) *api.APIError {
		return api.Coded(billing.CodeInvalidParam, msg).WithParam(param)
	}
	if req.ProductID.IsZero() == (strings.TrimSpace(req.ProductKey) == "") {
		return invalid("product_id", "exactly one of product_id or product_key is required")
	}
	if len(strings.TrimSpace(req.Reason)) > productArchiveMaxReason {
		return invalid("reason", "reason is too long")
	}
	if req.PurchaseAction == "" {
		req.PurchaseAction = billing.PurchaseActionNone
	}
	windowed := !req.PurchaseWindowStartsAt.IsZero() || req.WindowSeconds != 0
	switch req.PurchaseAction {
	case billing.PurchaseActionNone:
		if windowed {
			return invalid("purchase_action", "purchase_action none takes no purchase window")
		}
		return nil
	case billing.PurchaseActionRefund, billing.PurchaseActionReview:
	default:
		return invalid("purchase_action", `purchase_action must be "none", "refund" or "review"`)
	}
	if req.PurchaseWindowStartsAt.IsZero() == (req.WindowSeconds == 0) {
		return invalid("purchase_window_starts_at", "exactly one of purchase_window_starts_at or window_seconds is required")
	}
	if req.WindowSeconds < 0 {
		return invalid("window_seconds", "window_seconds must be positive")
	}
	return nil
}

// CreateProductArchive archives a product and applies the caller's purchase
// policy. POST /merchant/catalog/product-archives, Idempotency-Key required.
func CreateProductArchive(r *httprequest.Request) {
	var req billing.ArchiveProductParams
	if !bindCatalogJSON(r, &req) {
		return
	}
	if apiErr := validateProductArchive(&req); apiErr != nil {
		r.APIError(apiErr)
		return
	}
	key := strings.TrimSpace(r.Header("Idempotency-Key"))
	if key == "" || len(key) > 255 {
		r.ErrorCode("idempotency_key_required", "Idempotency-Key header is required (at most 255 bytes)")
		return
	}
	if r.State == nil || r.State.DB == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "product archive ledger unavailable")
		return
	}
	ctx := r.Request.Context()
	op, apiErr := acceptProductArchive(ctx, r, req, key)
	if apiErr != nil {
		r.APIError(apiErr)
		return
	}
	if err := archiveProductRow(ctx, r, op.ProductID); err != nil {
		writeCatalogError(r, err)
		return
	}
	out, err := evaluateProductArchive(ctx, r, op, true)
	if err != nil {
		r.InternalError("product archive purchases could not be evaluated", err)
		return
	}
	r.JSON(http.StatusOK, out)
}

// GetProductArchive projects the current outcome of each qualifying purchase
// without issuing refunds or reviews.
func GetProductArchive(r *httprequest.Request) {
	id, err := billing.ParseProductArchiveID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid product archive id").WithParam("id"))
		return
	}
	if r.State == nil || r.State.DB == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "product archive ledger unavailable")
		return
	}
	ctx := r.Request.Context()
	op, err := loadProductArchiveByID(ctx, r.State.DB, id.UUID())
	if err != nil {
		if db.IsNotFound(err) || errors.Is(err, pgx.ErrNoRows) {
			r.ErrorCode(billing.CodeResourceNotFound, "product archive not found")
			return
		}
		r.InternalError("product archive could not be loaded", err)
		return
	}
	out, err := evaluateProductArchive(ctx, r, op, false)
	if err != nil {
		r.InternalError("product archive purchases could not be evaluated", err)
		return
	}
	r.JSON(http.StatusOK, out)
}

func loadProductArchiveByID(ctx context.Context, d *db.DB, id uuid.UUID) (productArchiveOperation, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return productArchiveOperation{}, err
	}
	row, err := d.Gen(ctx).GetProductArchiveByID(ctx, gen.GetProductArchiveByIDParams{MerchantID: mid.UUID(), ID: id})
	if err != nil {
		return productArchiveOperation{}, err
	}
	return productArchiveOperation{ID: row.ID, ProductID: row.ProductID, ProductKey: row.ProductKey, Action: billing.PurchaseAction(row.PurchaseAction),
		PurchaseWindowStartsAt: row.PurchaseWindowStartsAt, Reason: row.Reason, CreatedAt: row.CreatedAt}, nil
}

func loadProductArchiveByKey(ctx context.Context, d *db.DB, key string) (productArchiveOperation, []byte, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return productArchiveOperation{}, nil, err
	}
	row, err := d.Gen(ctx).GetProductArchiveByKey(ctx, gen.GetProductArchiveByKeyParams{MerchantID: mid.UUID(), IdempotencyKey: key})
	if err != nil {
		return productArchiveOperation{}, nil, err
	}
	return productArchiveOperation{ID: row.ID, ProductID: row.ProductID, ProductKey: row.ProductKey, Action: billing.PurchaseAction(row.PurchaseAction),
		PurchaseWindowStartsAt: row.PurchaseWindowStartsAt, Reason: row.Reason, CreatedAt: row.CreatedAt}, row.RequestSha256, nil
}

// acceptProductArchive records the receipt once per key; a replay with other
// terms is refused before anything else happens.
func acceptProductArchive(ctx context.Context, r *httprequest.Request, req billing.ArchiveProductParams, key string) (productArchiveOperation, *api.APIError) {
	var op productArchiveOperation
	var refusal *api.APIError
	fingerprint := productArchiveFingerprint(req)
	err := r.State.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		mid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		txDB := db.NewWithPgxTx(tx)
		q := txDB.Gen(ctx)
		if err := q.LockProductArchiveKey(ctx, "product_archive:"+mid.String()+":"+key); err != nil {
			return fmt.Errorf("lock product archive: %w", err)
		}
		existing, stored, err := loadProductArchiveByKey(ctx, txDB, key)
		switch {
		case err == nil:
			if string(stored) != string(fingerprint) {
				refusal = api.Coded(billing.CodeIdempotencyKeyReused, "Idempotency-Key was already used for a different product archive request")
				return nil
			}
			op = existing
			return nil
		case !db.IsNotFound(err) && !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("load product archive: %w", err)
		}
		var product gen.BillingProduct
		if !req.ProductID.IsZero() {
			product, err = q.GetProductByID(ctx, gen.GetProductByIDParams{MerchantID: mid.UUID(), ID: req.ProductID.UUID()})
		} else {
			product, err = q.GetProductByKey(ctx, gen.GetProductByKeyParams{MerchantID: mid.UUID(), Key: strings.TrimSpace(req.ProductKey)})
		}
		if db.IsNotFound(err) || errors.Is(err, pgx.ErrNoRows) {
			refusal = api.Coded("product_not_found", "product not found")
			return nil
		}
		if err != nil {
			return fmt.Errorf("resolve product: %w", err)
		}
		var since *time.Time
		if !req.PurchaseWindowStartsAt.IsZero() {
			at := req.PurchaseWindowStartsAt.UTC()
			since = &at
		} else if req.WindowSeconds != 0 {
			at := r.Clock.Now().UTC().Add(-time.Duration(req.WindowSeconds) * time.Second)
			since = &at
		}
		if err := q.InsertProductArchive(ctx, gen.InsertProductArchiveParams{MerchantID: mid.UUID(), IdempotencyKey: key, RequestSha256: fingerprint,
			ProductID: product.ID, PurchaseAction: string(req.PurchaseAction), PurchaseWindowStartsAt: since, Reason: strings.TrimSpace(req.Reason)}); err != nil {
			return fmt.Errorf("record product archive: %w", err)
		}
		op, _, err = loadProductArchiveByKey(ctx, txDB, key)
		return err
	})
	if err != nil {
		logrus.WithContext(ctx).WithError(err).Error("product archive could not be recorded")
		return op, api.Coded(billing.CodeInternalError, "product archive could not be recorded")
	}
	return op, refusal
}

// archiveProductRow uses the ordinary catalog write: one row update that
// retires every offer of the product and propagates to Stripe.
func archiveProductRow(ctx context.Context, r *httprequest.Request, productID uuid.UUID) error {
	svc, err := billingservice.New(r.State)
	if err != nil {
		return err
	}
	current, err := svc.GetProduct(ctx, billing.ProductID(productID))
	if err != nil {
		return err
	}
	if current.Archived {
		return nil
	}
	archived := true
	_, err = svc.UpdateProduct(ctx, billing.ProductID(productID), billing.UpdateProductParams{Archived: catalog.Value(archived)})
	return err
}

type qualifyingPurchase struct {
	ID            uuid.UUID
	CustomerID    uuid.UUID
	Amount        int64
	Currency      string
	PurchasedAt   time.Time
	MoneyMovement string
}

func qualifyingPurchases(ctx context.Context, d *db.DB, op productArchiveOperation) ([]qualifyingPurchase, error) {
	if op.Action == billing.PurchaseActionNone || op.PurchaseWindowStartsAt == nil {
		return nil, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := d.Gen(ctx).ListProductArchivePurchases(ctx, gen.ListProductArchivePurchasesParams{MerchantID: mid.UUID(), ProductID: op.ProductID, PurchaseWindowStartsAt: *op.PurchaseWindowStartsAt})
	if err != nil {
		return nil, err
	}
	out := make([]qualifyingPurchase, 0, len(rows))
	for _, row := range rows {
		out = append(out, qualifyingPurchase{ID: row.ID, CustomerID: row.CustomerID, Amount: row.Amount, Currency: row.Currency, PurchasedAt: row.PurchasedAt, MoneyMovement: row.MoneyMovement})
	}
	return out, nil
}

func productArchiveRefundKey(op productArchiveOperation) string {
	return "product_archive:" + op.ID.String()
}

func evaluateProductArchive(ctx context.Context, r *httprequest.Request, op productArchiveOperation, act bool) (*billing.ProductArchive, error) {
	out := &billing.ProductArchive{
		ID: billing.ProductArchiveID(op.ID), ProductID: billing.ProductID(op.ProductID), ProductKey: op.ProductKey,
		PurchaseAction: op.Action, PurchaseWindowStartsAt: op.PurchaseWindowStartsAt, CreatedAt: op.CreatedAt,
		Complete: true, Purchases: []billing.ArchivedPurchase{},
	}
	if op.Reason != "" {
		out.Reason = &op.Reason
	}
	purchases, err := qualifyingPurchases(ctx, r.State.DB, op)
	if err != nil {
		return nil, err
	}
	budget := productArchiveRefundBudget
	for _, purchase := range purchases {
		item, err := evaluateArchivedPurchase(ctx, r, op, purchase, act, &budget)
		if err != nil {
			return nil, err
		}
		if item.Outcome == billing.ArchivedPurchaseNotStarted {
			out.Complete = false
		}
		out.Purchases = append(out.Purchases, item)
	}
	return out, nil
}

func evaluateArchivedPurchase(ctx context.Context, r *httprequest.Request, op productArchiveOperation, purchase qualifyingPurchase, act bool, budget *int) (billing.ArchivedPurchase, error) {
	item := billing.ArchivedPurchase{
		PaymentID: billing.PaymentID(purchase.ID), CustomerID: billing.CustomerID(purchase.CustomerID),
		Amount: purchase.Amount, Currency: purchase.Currency, PurchasedAt: purchase.PurchasedAt,
	}
	paymentService := payments.NewPaymentService(r.State.DB, r.Clock)
	if op.Action == billing.PurchaseActionRefund {
		refund, err := paymentService.GetRefundByAdminIdempotencyKey(ctx, purchase.ID, productArchiveRefundKey(op))
		switch {
		case err == nil && !strings.EqualFold(refund.Status, payments.PaymentStatusFailedValue):
			id := billing.PaymentID(refund.ID)
			item.RefundID = &id
			item.Outcome = billing.ArchivedPurchaseRefunded
			if !payments.PaymentStatusCompleted(refund.Status) {
				item.Outcome = billing.ArchivedPurchaseRefundPending
			}
			return item, nil
		case err == nil:
			// A terminally refused refund becomes a review, never a retry.
			return reviewArchivedPurchase(ctx, r, op, purchase, item, "automatic refund failed", act)
		case !db.IsNotFound(err) && !errors.Is(err, pgx.ErrNoRows):
			return item, fmt.Errorf("load archive refund: %w", err)
		}
	}
	if review, found, err := loadPurchaseReview(ctx, r.State.DB, purchase.ID); err != nil {
		return item, err
	} else if found {
		return applyReviewOutcome(item, review), nil
	}
	refunded, err := paymentService.GetRefundTotalByPaymentID(ctx, purchase.ID)
	if err != nil {
		return item, err
	}
	if purchase.Amount-refunded <= 0 {
		item.Outcome = billing.ArchivedPurchaseAlreadyRefunded
		return item, nil
	}
	if op.Action == billing.PurchaseActionReview {
		return reviewArchivedPurchase(ctx, r, op, purchase, item, "", act)
	}
	if purchase.MoneyMovement != string(models.MoneyMovementRail) {
		return reviewArchivedPurchase(ctx, r, op, purchase, item, "off-rail purchase; refund it out of band", act)
	}
	if !act || *budget <= 0 {
		item.Outcome = billing.ArchivedPurchaseNotStarted
		return item, nil
	}
	*budget--
	reason := op.Reason
	if reason == "" {
		reason = "product archived"
	}
	refund, status, err := executeAdminRefund(ctx, r, purchase.ID, RefundRequest{Full: true, Reason: reason, RevokeAccess: true}, productArchiveRefundKey(op))
	if err != nil {
		var refusal *api.APIError
		if !errors.As(err, &refusal) {
			return item, fmt.Errorf("refund archived purchase %s: %w", purchase.ID, err)
		}
		return reviewArchivedPurchase(ctx, r, op, purchase, item, "automatic refund refused: "+refusal.Message, act)
	}
	id := billing.PaymentID(refund.ID)
	item.RefundID = &id
	item.Outcome = billing.ArchivedPurchaseRefunded
	if status == http.StatusAccepted {
		item.Outcome = billing.ArchivedPurchaseRefundPending
	}
	return item, nil
}

func reviewArchivedPurchase(ctx context.Context, r *httprequest.Request, op productArchiveOperation, purchase qualifyingPurchase, item billing.ArchivedPurchase, detail string, act bool) (billing.ArchivedPurchase, error) {
	if !act {
		if review, found, err := loadPurchaseReview(ctx, r.State.DB, purchase.ID); err != nil || found {
			return applyReviewOutcome(item, review), err
		}
		item.Outcome = billing.ArchivedPurchaseNotStarted
		return item, nil
	}
	review, err := recordPurchaseReview(ctx, r.State.DB, op, purchase, detail)
	if err != nil {
		return item, err
	}
	return applyReviewOutcome(item, review), nil
}

func applyReviewOutcome(item billing.ArchivedPurchase, review purchaseReview) billing.ArchivedPurchase {
	// The finding keeps the reason recorded when it was opened, so every
	// projection of the operation reports the same detail.
	item.FindingID = &review.ID
	if review.Detail != "" {
		item.Detail = &review.Detail
	}
	item.Outcome = review.Outcome
	if review.Outcome == billing.ArchivedPurchaseReviewRefunded {
		item.RefundID = review.RefundID
	}
	return item
}

// recordPurchaseReview inserts one open finding per payment. It never reopens
// a finding the merchant already resolved.
func recordPurchaseReview(ctx context.Context, d *db.DB, op productArchiveOperation, purchase qualifyingPurchase, detail string) (purchaseReview, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return purchaseReview{}, err
	}
	payment := billing.PaymentID(purchase.ID)
	evidence := map[string]any{
		recommend.EvidenceKey: recommend.Recommendation{Action: recommend.ActionCancelAndRefund, Params: map[string]any{
			"refund_payment_id": payment.String(), "revoke_access": true,
		}}.Map(),
		"local": map[string]any{
			"product_archive_id": op.ID.String(), "product_id": billing.ProductID(op.ProductID).String(), "product_key": op.ProductKey,
			"payment_id": payment.String(), "customer_id": purchase.CustomerID.String(), "amount": fmt.Sprint(purchase.Amount),
			"currency": purchase.Currency, "purchased_at": purchase.PurchasedAt.UTC().Format(time.RFC3339Nano), "detail": detail,
		},
	}
	raw, err := json.Marshal(evidence)
	if err != nil {
		return purchaseReview{}, err
	}
	if err := d.Gen(ctx).InsertPurchaseReview(ctx, gen.InsertPurchaseReviewParams{MerchantID: mid.UUID(), FindingType: productArchiveFindingType,
		SubjectKey: purchase.ID.String(), RecommendedAction: "Refund or dismiss a purchase of an archived product", Evidence: raw}); err != nil {
		return purchaseReview{}, fmt.Errorf("record purchase review: %w", err)
	}
	review, _, err := loadPurchaseReview(ctx, d, purchase.ID)
	return review, err
}

func loadPurchaseReview(ctx context.Context, d *db.DB, paymentID uuid.UUID) (purchaseReview, bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return purchaseReview{}, false, err
	}
	row, err := d.Gen(ctx).GetPurchaseReviewBySubject(ctx, gen.GetPurchaseReviewBySubjectParams{MerchantID: mid.UUID(), FindingType: productArchiveFindingType, SubjectKey: paymentID.String()})
	if db.IsNotFound(err) || errors.Is(err, pgx.ErrNoRows) {
		return purchaseReview{}, false, nil
	}
	if err != nil {
		return purchaseReview{}, false, err
	}
	var evidence struct {
		Local      map[string]string `json:"local"`
		Resolution map[string]any    `json:"resolution"`
	}
	_ = json.Unmarshal(row.Evidence, &evidence)
	review := purchaseReview{ID: billing.FindingID(row.ID), Detail: evidence.Local["detail"], Outcome: billing.ArchivedPurchaseReviewOpen}
	switch row.Status {
	case "fixed":
		review.Outcome = billing.ArchivedPurchaseReviewRefunded
		if refundID, ok := evidence.Resolution["refund_id"].(string); ok {
			if parsed, err := uuid.Parse(refundID); err == nil {
				typed := billing.PaymentID(parsed)
				review.RefundID = &typed
			}
		}
	case "ignored":
		review.Outcome = billing.ArchivedPurchaseReviewDismissed
	}
	return review, true, nil
}
