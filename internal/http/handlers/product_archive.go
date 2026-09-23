package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/reconcile/recommend"
	billingservice "github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Product archive operations (#1058). The receipt fixes the product, action,
// resolved window and reason. Every call re-derives the qualifying purchases
// from the ledger, so replays converge: refunds use one deterministic key per
// operation and payment, and reviews are one insert-if-absent finding per
// payment. Only rail money movement is refunded automatically; anything the
// refund path refuses becomes a review instead of being dropped.

const (
	productArchiveFindingType = "life.product_archived_purchase"
	// productArchiveRefundBudget bounds provider calls per request; a replay
	// continues where the previous call stopped.
	productArchiveRefundBudget = 50
	productArchiveMaxReason    = 500
)

type productArchiveRequest struct {
	ProductID  string                  `json:"product_id,omitempty"`
	ProductKey string                  `json:"product_key,omitempty"`
	Purchases  productArchivePurchases `json:"purchases"`
	Reason     string                  `json:"reason,omitempty"`
}

type productArchivePurchases struct {
	Action         string     `json:"action"`
	PurchasedSince *time.Time `json:"purchased_since,omitempty"`
	Window         string     `json:"window,omitempty"`
}

type productArchiveOperation struct {
	ID             uuid.UUID
	ProductID      uuid.UUID
	ProductKey     string
	Action         openrails.PurchaseAction
	PurchasedSince *time.Time
	Reason         string
	CreatedAt      time.Time
}

func productArchiveError(status int, code, message string) *api.APIError {
	errType := api.ErrorTypeInvalidRequest
	if status >= 500 {
		errType = api.ErrorTypeAPI
	}
	return api.NewAPIError(status, errType, code, message)
}

// fingerprint canonicalizes the caller's terms. A relative window is part of
// the identity, never the instant it resolved to, so a replay matches.
func (req productArchiveRequest) fingerprint() []byte {
	canonical := map[string]string{
		"product_id": strings.TrimSpace(req.ProductID), "product_key": strings.TrimSpace(req.ProductKey),
		"action": req.Purchases.Action, "reason": strings.TrimSpace(req.Reason),
	}
	if req.Purchases.PurchasedSince != nil {
		canonical["purchased_since"] = req.Purchases.PurchasedSince.UTC().Format(time.RFC3339Nano)
	}
	if req.Purchases.Window != "" {
		window, _ := time.ParseDuration(req.Purchases.Window)
		canonical["window"] = window.String()
	}
	raw, _ := json.Marshal(canonical)
	sum := sha256.Sum256(raw)
	return sum[:]
}

func (req *productArchiveRequest) validate() *api.APIError {
	invalid := func(msg string) *api.APIError {
		return productArchiveError(http.StatusBadRequest, "invalid_request", msg)
	}
	if (strings.TrimSpace(req.ProductID) == "") == (strings.TrimSpace(req.ProductKey) == "") {
		return invalid("exactly one of product_id or product_key is required")
	}
	if len(strings.TrimSpace(req.Reason)) > productArchiveMaxReason {
		return invalid("reason is too long")
	}
	req.Purchases.Action = strings.ToLower(strings.TrimSpace(req.Purchases.Action))
	if req.Purchases.Action == "" {
		req.Purchases.Action = string(openrails.PurchaseActionNone)
	}
	windowed := req.Purchases.PurchasedSince != nil || strings.TrimSpace(req.Purchases.Window) != ""
	switch openrails.PurchaseAction(req.Purchases.Action) {
	case openrails.PurchaseActionNone:
		if windowed {
			return invalid("purchases.action none takes no purchase window")
		}
		return nil
	case openrails.PurchaseActionRefund, openrails.PurchaseActionReview:
	default:
		return invalid(`purchases.action must be "none", "refund" or "review"`)
	}
	if (req.Purchases.PurchasedSince != nil) == (strings.TrimSpace(req.Purchases.Window) != "") {
		return invalid("exactly one of purchases.purchased_since or purchases.window is required")
	}
	if req.Purchases.Window != "" {
		window, err := time.ParseDuration(strings.TrimSpace(req.Purchases.Window))
		if err != nil || window <= 0 {
			return invalid("purchases.window must be a positive duration such as \"720h\"")
		}
		req.Purchases.Window = window.String()
	}
	return nil
}

// CreateProductArchive archives a product and applies the caller's purchase
// policy. POST /merchant/catalog/product-archives, Idempotency-Key required.
func CreateProductArchive(r *httprequest.Request) {
	var req productArchiveRequest
	if !bindCatalogJSON(r, &req) {
		return
	}
	if apiErr := req.validate(); apiErr != nil {
		r.APIError(apiErr)
		return
	}
	key := strings.TrimSpace(r.Header("Idempotency-Key"))
	if key == "" || len(key) > 255 {
		r.APIError(productArchiveError(http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header is required (at most 255 bytes)"))
		return
	}
	if r.State == nil || r.State.DB == nil {
		r.ErrorJSON(http.StatusInternalServerError, "product archive ledger unavailable")
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
	id, err := uuid.Parse(strings.TrimSpace(r.Param("id")))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid product archive id")
		return
	}
	if r.State == nil || r.State.DB == nil {
		r.ErrorJSON(http.StatusInternalServerError, "product archive ledger unavailable")
		return
	}
	ctx := r.Request.Context()
	op, err := loadProductArchive(ctx, r.State.DB, "id=$2", id)
	if err != nil {
		if db.IsNotFound(err) || errors.Is(err, pgx.ErrNoRows) {
			r.ErrorJSON(http.StatusNotFound, "product archive not found")
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

const productArchiveColumns = `o.id, o.product_id, p.key, o.purchase_action, o.purchased_since, o.reason, o.created_at
FROM openrails.product_archive_operations o JOIN openrails.products p ON p.merchant_id=o.merchant_id AND p.id=o.product_id`

func loadProductArchive(ctx context.Context, d *db.DB, predicate string, arg any) (productArchiveOperation, error) {
	var op productArchiveOperation
	mid, err := merchant.Require(ctx)
	if err != nil {
		return op, err
	}
	var action string
	err = d.Qx(ctx).QueryRow(ctx, `SELECT `+productArchiveColumns+` WHERE o.merchant_id=$1 AND o.`+predicate, mid.UUID(), arg).
		Scan(&op.ID, &op.ProductID, &op.ProductKey, &action, &op.PurchasedSince, &op.Reason, &op.CreatedAt)
	op.Action = openrails.PurchaseAction(action)
	return op, err
}

// acceptProductArchive records the receipt once per key; a replay with other
// terms is refused before anything else happens.
func acceptProductArchive(ctx context.Context, r *httprequest.Request, req productArchiveRequest, key string) (productArchiveOperation, *api.APIError) {
	var op productArchiveOperation
	var refusal *api.APIError
	fingerprint := req.fingerprint()
	err := r.State.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		mid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "product_archive:"+mid.String()+":"+key); err != nil {
			return fmt.Errorf("lock product archive: %w", err)
		}
		txDB := db.NewWithPgxTx(tx)
		var stored []byte
		err = tx.QueryRow(ctx, `SELECT request_sha256 FROM openrails.product_archive_operations WHERE merchant_id=$1 AND idempotency_key=$2`, mid.UUID(), key).Scan(&stored)
		switch {
		case err == nil:
			if string(stored) != string(fingerprint) {
				refusal = productArchiveError(http.StatusConflict, "idempotency_key_reused", "Idempotency-Key was already used for a different product archive request")
				return nil
			}
			op, err = loadProductArchive(ctx, txDB, "idempotency_key=$2", key)
			return err
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("load product archive: %w", err)
		}
		var productID uuid.UUID
		if raw := strings.TrimSpace(req.ProductID); raw != "" {
			typed, perr := openrails.ParseProductID(raw)
			if perr != nil || typed.IsZero() {
				refusal = productArchiveError(http.StatusBadRequest, "invalid_request", "invalid product_id")
				return nil
			}
			err = tx.QueryRow(ctx, `SELECT id FROM openrails.products WHERE merchant_id=$1 AND id=$2`, mid.UUID(), typed.UUID()).Scan(&productID)
		} else {
			err = tx.QueryRow(ctx, `SELECT id FROM openrails.products WHERE merchant_id=$1 AND key=$2`, mid.UUID(), strings.TrimSpace(req.ProductKey)).Scan(&productID)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			refusal = productArchiveError(http.StatusNotFound, "resource_missing", "product not found")
			return nil
		}
		if err != nil {
			return fmt.Errorf("resolve product: %w", err)
		}
		var since *time.Time
		if req.Purchases.PurchasedSince != nil {
			at := req.Purchases.PurchasedSince.UTC()
			since = &at
		} else if req.Purchases.Window != "" {
			window, _ := time.ParseDuration(req.Purchases.Window)
			at := r.Clock.Now().UTC().Add(-window)
			since = &at
		}
		if _, err := tx.Exec(ctx, `INSERT INTO openrails.product_archive_operations(merchant_id,idempotency_key,request_sha256,product_id,purchase_action,purchased_since,reason)
			VALUES($1,$2,$3,$4,$5,$6,$7)`, mid.UUID(), key, fingerprint, productID, req.Purchases.Action, since, strings.TrimSpace(req.Reason)); err != nil {
			return fmt.Errorf("record product archive: %w", err)
		}
		op, err = loadProductArchive(ctx, txDB, "idempotency_key=$2", key)
		return err
	})
	if err != nil {
		return op, productArchiveError(http.StatusInternalServerError, "api_error", "product archive could not be recorded")
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
	current, err := svc.GetProduct(ctx, openrails.ProductID(productID))
	if err != nil {
		return err
	}
	if current.Archived {
		return nil
	}
	archived := true
	_, err = svc.UpdateProduct(ctx, openrails.ProductID(productID), openrails.UpdateProductRequest{Archived: &archived})
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
	if op.Action == openrails.PurchaseActionNone || op.PurchasedSince == nil {
		return nil, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	// One-time completed charges of the product. Subscription payments stay
	// with their grandfathered subscriptions. Bookkeeping rows without money
	// movement qualify only for off-rail channels, which carry real value.
	rows, err := d.Qx(ctx).Query(ctx, `SELECT p.id, p.customer_id, p.amount, p.currency, p.purchased_at, p.money_movement
		FROM openrails.payments p JOIN openrails.prices pr ON pr.merchant_id=p.merchant_id AND pr.id=p.price_id
		WHERE p.merchant_id=$1 AND pr.product_id=$2 AND p.purchased_at >= $3
		  AND p.refunded_payment_id IS NULL AND p.amount > 0 AND p.status='completed'
		  AND p.deleted_at IS NULL AND p.subscription_id IS NULL
		  AND (p.money_movement='rail' OR p.rail IN ('manual','admin'))
		ORDER BY p.purchased_at, p.id`, mid.UUID(), op.ProductID, *op.PurchasedSince)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []qualifyingPurchase
	for rows.Next() {
		var q qualifyingPurchase
		if err := rows.Scan(&q.ID, &q.CustomerID, &q.Amount, &q.Currency, &q.PurchasedAt, &q.MoneyMovement); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func productArchiveRefundKey(op productArchiveOperation) string {
	return "product_archive:" + op.ID.String()
}

func evaluateProductArchive(ctx context.Context, r *httprequest.Request, op productArchiveOperation, act bool) (*openrails.ProductArchive, error) {
	out := &openrails.ProductArchive{
		ID: op.ID.String(), Object: "product_archive", ProductID: openrails.ProductID(op.ProductID), ProductKey: op.ProductKey,
		Action: op.Action, PurchasedSince: op.PurchasedSince, Reason: op.Reason, CreatedAt: op.CreatedAt,
		Complete: true, Purchases: []openrails.ArchivedPurchase{},
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
		if item.Outcome == openrails.ArchivedPurchaseNotStarted {
			out.Complete = false
		}
		out.Purchases = append(out.Purchases, item)
	}
	return out, nil
}

func evaluateArchivedPurchase(ctx context.Context, r *httprequest.Request, op productArchiveOperation, purchase qualifyingPurchase, act bool, budget *int) (openrails.ArchivedPurchase, error) {
	item := openrails.ArchivedPurchase{
		PaymentID: openrails.PaymentID(purchase.ID), CustomerID: purchase.CustomerID.String(),
		Amount: purchase.Amount, Currency: purchase.Currency, PurchasedAt: purchase.PurchasedAt,
	}
	paymentService := payments.NewPaymentService(r.State.DB, r.Clock)
	if op.Action == openrails.PurchaseActionRefund {
		refund, err := paymentService.GetRefundByAdminIdempotencyKey(ctx, purchase.ID, productArchiveRefundKey(op))
		switch {
		case err == nil && !strings.EqualFold(refund.Status, payments.PaymentStatusFailedValue):
			id := openrails.PaymentID(refund.ID)
			item.RefundID = &id
			item.Outcome = openrails.ArchivedPurchaseRefunded
			if !payments.PaymentStatusCompleted(refund.Status) {
				item.Outcome = openrails.ArchivedPurchaseRefundPending
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
		item.Outcome = openrails.ArchivedPurchaseAlreadyRefunded
		return item, nil
	}
	if op.Action == openrails.PurchaseActionReview {
		return reviewArchivedPurchase(ctx, r, op, purchase, item, "", act)
	}
	if purchase.MoneyMovement != string(models.MoneyMovementRail) {
		return reviewArchivedPurchase(ctx, r, op, purchase, item, "off-rail purchase; refund it out of band", act)
	}
	if !act || *budget <= 0 {
		item.Outcome = openrails.ArchivedPurchaseNotStarted
		return item, nil
	}
	*budget--
	reason := op.Reason
	if reason == "" {
		reason = "product archived"
	}
	refund, status, err := executeAdminRefund(ctx, r, purchase.ID, refundRequest{Full: true, Reason: reason, RevokeAccess: true}, productArchiveRefundKey(op))
	if err != nil {
		var refusal *adminRefundStatusError
		if !errors.As(err, &refusal) {
			return item, fmt.Errorf("refund archived purchase %s: %w", purchase.ID, err)
		}
		return reviewArchivedPurchase(ctx, r, op, purchase, item, "automatic refund refused: "+refusal.Message, act)
	}
	id := openrails.PaymentID(refund.ID)
	item.RefundID = &id
	item.Outcome = openrails.ArchivedPurchaseRefunded
	if status == http.StatusAccepted {
		item.Outcome = openrails.ArchivedPurchaseRefundPending
	}
	return item, nil
}

func reviewArchivedPurchase(ctx context.Context, r *httprequest.Request, op productArchiveOperation, purchase qualifyingPurchase, item openrails.ArchivedPurchase, detail string, act bool) (openrails.ArchivedPurchase, error) {
	if !act {
		if review, found, err := loadPurchaseReview(ctx, r.State.DB, purchase.ID); err != nil || found {
			return applyReviewOutcome(item, review), err
		}
		item.Outcome = openrails.ArchivedPurchaseNotStarted
		return item, nil
	}
	review, err := recordPurchaseReview(ctx, r.State.DB, op, purchase, detail)
	if err != nil {
		return item, err
	}
	return applyReviewOutcome(item, review), nil
}

func applyReviewOutcome(item openrails.ArchivedPurchase, review openrails.PurchaseReview) openrails.ArchivedPurchase {
	// The review keeps the reason recorded when it was opened, so every
	// projection of the operation reports the same detail.
	item.ReviewID = review.ID
	item.Detail = review.Detail
	switch review.Status {
	case openrails.PurchaseReviewRefunded:
		item.Outcome = openrails.ArchivedPurchaseReviewRefunded
		item.RefundID = review.RefundID
	case openrails.PurchaseReviewDismissed:
		item.Outcome = openrails.ArchivedPurchaseReviewDismissed
	default:
		item.Outcome = openrails.ArchivedPurchaseReviewOpen
	}
	return item
}

// recordPurchaseReview inserts one open finding per payment. It never reopens
// a finding the merchant already resolved.
func recordPurchaseReview(ctx context.Context, d *db.DB, op productArchiveOperation, purchase qualifyingPurchase, detail string) (openrails.PurchaseReview, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return openrails.PurchaseReview{}, err
	}
	payment := openrails.PaymentID(purchase.ID)
	evidence := map[string]any{
		recommend.EvidenceKey: recommend.Recommendation{Action: recommend.ActionCancelAndRefund, Params: map[string]any{
			"refund_payment_id": payment.String(), "revoke_access": true,
		}}.Map(),
		"local": map[string]any{
			"product_archive_id": op.ID.String(), "product_id": openrails.ProductID(op.ProductID).String(), "product_key": op.ProductKey,
			"payment_id": payment.String(), "customer_id": purchase.CustomerID.String(), "amount": fmt.Sprint(purchase.Amount),
			"currency": purchase.Currency, "purchased_at": purchase.PurchasedAt.UTC().Format(time.RFC3339Nano), "detail": detail,
		},
	}
	raw, err := json.Marshal(evidence)
	if err != nil {
		return openrails.PurchaseReview{}, err
	}
	if _, err := d.Qx(ctx).Exec(ctx, `INSERT INTO openrails.reconciliation_findings(merchant_id,finding_type,subject_key,severity,status,recommended_action,evidence)
		VALUES($1,$2,$3,'medium','requires_review',$4,$5::jsonb) ON CONFLICT (merchant_id,finding_type,subject_key) DO NOTHING`,
		mid.UUID(), productArchiveFindingType, purchase.ID.String(), "Refund or dismiss a purchase of an archived product", raw); err != nil {
		return openrails.PurchaseReview{}, fmt.Errorf("record purchase review: %w", err)
	}
	review, _, err := loadPurchaseReview(ctx, d, purchase.ID)
	return review, err
}

const purchaseReviewColumns = `id, status, evidence, operator_notes, created_at, resolved_at`

func loadPurchaseReview(ctx context.Context, d *db.DB, paymentID uuid.UUID) (openrails.PurchaseReview, bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return openrails.PurchaseReview{}, false, err
	}
	rows, err := d.Qx(ctx).Query(ctx, `SELECT `+purchaseReviewColumns+` FROM openrails.reconciliation_findings
		WHERE merchant_id=$1 AND finding_type=$2 AND subject_key=$3`, mid.UUID(), productArchiveFindingType, paymentID.String())
	if err != nil {
		return openrails.PurchaseReview{}, false, err
	}
	reviews, err := scanPurchaseReviews(rows)
	if err != nil || len(reviews) == 0 {
		return openrails.PurchaseReview{}, false, err
	}
	return reviews[0], true, nil
}

func scanPurchaseReviews(rows pgx.Rows) ([]openrails.PurchaseReview, error) {
	defer rows.Close()
	var out []openrails.PurchaseReview
	for rows.Next() {
		var (
			id       uuid.UUID
			status   string
			raw      []byte
			notes    *string
			created  time.Time
			resolved *time.Time
		)
		if err := rows.Scan(&id, &status, &raw, &notes, &created, &resolved); err != nil {
			return nil, err
		}
		out = append(out, purchaseReviewFromFinding(id, status, raw, notes, created, resolved))
	}
	return out, rows.Err()
}

func purchaseReviewFromFinding(id uuid.UUID, status string, raw []byte, notes *string, created time.Time, resolved *time.Time) openrails.PurchaseReview {
	var evidence struct {
		Local      map[string]string `json:"local"`
		Resolution map[string]any    `json:"resolution"`
	}
	_ = json.Unmarshal(raw, &evidence)
	local := evidence.Local
	review := openrails.PurchaseReview{
		ID: id.String(), Object: "purchase_review", ProductArchiveID: local["product_archive_id"], ProductKey: local["product_key"],
		CustomerID: local["customer_id"], Currency: local["currency"], Detail: local["detail"], CreatedAt: created, ResolvedAt: resolved,
	}
	review.ProductID, _ = openrails.ParseProductID(local["product_id"])
	review.PaymentID, _ = openrails.ParsePaymentID(local["payment_id"])
	fmt.Sscan(local["amount"], &review.Amount)
	review.PurchasedAt, _ = time.Parse(time.RFC3339Nano, local["purchased_at"])
	if notes != nil {
		review.Notes = *notes
	}
	switch status {
	case "fixed":
		review.Status = openrails.PurchaseReviewRefunded
		if refundID, ok := evidence.Resolution["refund_id"].(string); ok {
			if parsed, err := uuid.Parse(refundID); err == nil {
				typed := openrails.PaymentID(parsed)
				review.RefundID = &typed
			}
		}
	case "ignored":
		review.Status = openrails.PurchaseReviewDismissed
	default:
		review.Status = openrails.PurchaseReviewOpen
	}
	return review
}

// ListPurchaseReviews lists archive purchase reviews, oldest first.
//
//	GET /merchant/purchase-reviews?status=open|refunded|dismissed&product_archive_id=&limit=&offset=
func ListPurchaseReviews(r *httprequest.Request) {
	if r.State == nil || r.State.DB == nil {
		r.ErrorJSON(http.StatusInternalServerError, "purchase review ledger unavailable")
		return
	}
	ctx := r.Request.Context()
	statuses := map[string][]string{
		"": {"requires_review", "reconcile_required"}, openrails.PurchaseReviewOpen: {"requires_review", "reconcile_required"},
		openrails.PurchaseReviewRefunded: {"fixed"}, openrails.PurchaseReviewDismissed: {"ignored"},
	}
	findingStatuses, ok := statuses[strings.TrimSpace(r.Query("status"))]
	if !ok {
		r.ErrorJSON(http.StatusBadRequest, `status must be "open", "refunded" or "dismissed"`)
		return
	}
	limit := min(max(parseIntDefault(r.Query("limit"), 50), 1), 200)
	offset := max(parseIntDefault(r.Query("offset"), 0), 0)
	archive := strings.TrimSpace(r.Query("product_archive_id"))
	mid, err := merchant.Require(ctx)
	if err != nil {
		r.InternalError("purchase reviews unavailable", err)
		return
	}
	var total int64
	where := `merchant_id=$1 AND finding_type=$2 AND status=ANY($3) AND ($4='' OR evidence->'local'->>'product_archive_id'=$4)`
	args := []any{mid.UUID(), productArchiveFindingType, findingStatuses, archive}
	if err := r.State.DB.Qx(ctx).QueryRow(ctx, `SELECT count(*) FROM openrails.reconciliation_findings WHERE `+where, args...).Scan(&total); err != nil {
		r.InternalError("purchase reviews could not be counted", err)
		return
	}
	rows, err := r.State.DB.Qx(ctx).Query(ctx, `SELECT `+purchaseReviewColumns+` FROM openrails.reconciliation_findings WHERE `+where+
		` ORDER BY created_at, id LIMIT $5 OFFSET $6`, append(args, limit, offset)...)
	if err != nil {
		r.InternalError("purchase reviews could not be listed", err)
		return
	}
	reviews, err := scanPurchaseReviews(rows)
	if err != nil {
		r.InternalError("purchase reviews could not be read", err)
		return
	}
	if reviews == nil {
		reviews = []openrails.PurchaseReview{}
	}
	r.JSON(http.StatusOK, openrails.Page[openrails.PurchaseReview]{Object: "list", Data: reviews, Total: total, Limit: limit, Offset: offset, HasMore: int64(offset+len(reviews)) < total})
}

// ResolvePurchaseReview refunds (approve) or dismisses (ignore) one review
// through the findings queue. Repeating the recorded decision is a no-op.
//
//	POST /merchant/purchase-reviews/{id}/resolve {decision, notes}
func ResolvePurchaseReview(r *httprequest.Request) {
	store, ok := findingsStore(r)
	if !ok {
		return
	}
	ctx := r.Request.Context()
	id, err := uuid.Parse(strings.TrimSpace(r.Param("id")))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid purchase review id")
		return
	}
	var req openrails.ResolvePurchaseReviewParams
	if !r.BindJSON(&req) {
		return
	}
	var outcome, want string
	switch req.Decision {
	case openrails.PurchaseReviewDecisionRefund:
		outcome, want = "approve", openrails.PurchaseReviewRefunded
	case openrails.PurchaseReviewDecisionDismiss:
		outcome, want = "ignore", openrails.PurchaseReviewDismissed
	default:
		r.ErrorJSON(http.StatusBadRequest, `decision must be "refund" or "dismiss"`)
		return
	}
	finding, err := store.GetFinding(ctx, id)
	if err != nil || string(finding.Type) != productArchiveFindingType {
		r.ErrorJSON(http.StatusNotFound, "purchase review not found")
		return
	}
	notes := strings.TrimSpace(req.Notes)
	if notes == "" {
		notes = "purchase review " + string(req.Decision)
	}
	if findingIsOpen(finding) {
		if status, message := resolveFinding(r, store, finding, outcome, notes, nil); status != http.StatusOK {
			r.ErrorJSON(status, message)
			return
		}
	}
	review, err := loadPurchaseReviewByID(ctx, r.State.DB, id)
	if err != nil {
		r.InternalError("purchase review could not be reloaded", err)
		return
	}
	if review.Status != want {
		r.APIError(productArchiveError(http.StatusConflict, "purchase_review_resolved", "purchase review was already resolved as "+review.Status))
		return
	}
	r.JSON(http.StatusOK, review)
}

func loadPurchaseReviewByID(ctx context.Context, d *db.DB, id uuid.UUID) (openrails.PurchaseReview, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return openrails.PurchaseReview{}, err
	}
	rows, err := d.Qx(ctx).Query(ctx, `SELECT `+purchaseReviewColumns+` FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND finding_type=$2 AND id=$3`, mid.UUID(), productArchiveFindingType, id)
	if err != nil {
		return openrails.PurchaseReview{}, err
	}
	reviews, err := scanPurchaseReviews(rows)
	if err != nil {
		return openrails.PurchaseReview{}, err
	}
	if len(reviews) == 0 {
		return openrails.PurchaseReview{}, pgx.ErrNoRows
	}
	return reviews[0], nil
}
