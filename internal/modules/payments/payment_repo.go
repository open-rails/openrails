package payments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/open-rails/openrails/pkg/query"
)

type PaymentFilters struct {
	UserID         string     `form:"user_id"`
	PriceID        uuid.UUID  `form:"price_id"`
	SubscriptionID string     `form:"subscription_id"` // UUID string, parsed in handler
	Rail           string     `form:"rail"`
	TransactionID  string     `form:"transaction_id"`
	StartDate      *time.Time `form:"created_after" time_format:"2006-01-02"`
	EndDate        *time.Time `form:"created_before" time_format:"2006-01-02"`
	MinAmount      *int64     `form:"min_amount"`
	MaxAmount      *int64     `form:"max_amount"`
	Status         string     `form:"status"` // pending|completed|failed|refunded (#733 deep-link)
	RefundsOnly    bool       `form:"refunds_only"`
	SortBy         string     `form:"sort_by"`    // created_at (default), amount, purchased_at
	SortOrder      string     `form:"sort_order"` // asc, desc (default)
}

type PaymentRepo struct {
	db *db.DB
}

const internalNMISubscriptionAttemptMetadataKey = "nmi_subscription_order_id"

func NewPaymentRepo(d *db.DB) *PaymentRepo { return &PaymentRepo{db: d} }

// IsSettlementCandidate reports whether this row will land as a completed,
// positive, non-reversal charge — the shape the host settlement feed's enqueue
// trigger considers. An empty status is 'completed' (the column default), so it
// counts here exactly as it does in SQL.
func IsSettlementCandidate(p *models.Payment) bool {
	return p != nil && PaymentStatusCompleted(p.Status) && p.Amount > 0 && p.RefundedPaymentID == nil
}

// resolveMoneyMovement enforces or#827's positive marker at the one place every
// payment row is minted. The feed publishes on this value alone, so on a row
// that would otherwise be a settlement candidate an undeclared marker is a bug,
// not a default: refusing the write is how a new synthetic payment shape gets
// caught here instead of at a host that was told money arrived.
func resolveMoneyMovement(p *models.Payment) (models.MoneyMovement, error) {
	switch {
	case p.MoneyMovement.Valid():
		return p.MoneyMovement, nil
	case p.MoneyMovement != models.MoneyMovementUndeclared:
		return "", fmt.Errorf("payment money_movement %q is not a known value", string(p.MoneyMovement))
	case IsSettlementCandidate(p):
		return "", errors.New("payment money_movement must be declared on a completed positive charge (or#827)")
	default:
		return models.MoneyMovementNone, nil
	}
}

// paymentInsertParams maps a model onto the insert parameter set shared by
// CreatePayment and CreatePaymentIfNotExists (identical column lists).
func paymentInsertParams(p *models.Payment) (gen.CreatePaymentParams, error) {
	// CUR-6: the ONE place both CreatePayment and CreatePaymentIfNotExists
	// build their params, so canonicalising here covers every payment row the
	// repo mints regardless of which rail's ingestion produced it.
	currency := moneyutil.NormalizeCurrency(p.Currency)
	if currency == "" {
		return gen.CreatePaymentParams{}, fmt.Errorf("payment currency required")
	}
	discountMeta, err := models.ToJSONB(p.DiscountMetadata)
	if err != nil {
		return gen.CreatePaymentParams{}, err
	}
	meta, err := models.ToJSONB(p.Metadata)
	if err != nil {
		return gen.CreatePaymentParams{}, err
	}
	entSnap, err := models.ToJSONB(p.EntitlementsSpecSnapshot)
	if err != nil {
		return gen.CreatePaymentParams{}, err
	}
	credSnap, err := models.ToJSONB(p.CreditsSpecSnapshot)
	if err != nil {
		return gen.CreatePaymentParams{}, err
	}
	movement, err := resolveMoneyMovement(p)
	if err != nil {
		return gen.CreatePaymentParams{}, err
	}
	return gen.CreatePaymentParams{
		ID:                       p.ID,
		PriceID:                  p.PriceID,
		Rail:                     string(p.Rail),
		TransactionID:            p.TransactionID,
		Amount:                   p.Amount,
		ListAmount:               p.ListAmount,
		Currency:                 currency,
		Status:                   p.Status,
		PspID:                    p.PspID,
		SubscriptionID:           p.SubscriptionID,
		RefundedPaymentID:        p.RefundedPaymentID,
		DiscountCode:             p.DiscountCode,
		DiscountReason:           p.DiscountReason,
		DiscountMetadata:         discountMeta,
		EntitlementsSpecSnapshot: entSnap,
		CreditsSpecSnapshot:      credSnap,
		Metadata:                 meta,
		PurchasedAt:              p.PurchasedAt,
		CreatedAt:                p.CreatedAt,
		CardBrand:                p.CardBrand,
		CardLast4:                p.CardLast4,
		CustomerID:               p.CustomerID,
		AttemptKind:              p.AttemptKind,
		FailureCode:              p.FailureCode,
		FailureReason:            p.FailureReason,
		ReversalKind:             p.ReversalKind,
		TokenType:                p.TokenType,
		MoneyMovement:            string(movement),
	}, nil
}

func (r *PaymentRepo) Create(ctx context.Context, payment *models.Payment) error {
	if err := db.EnsureCustomerRow(ctx, r.db.Qx(ctx), uuid.Nil, payment.CustomerID); err != nil {
		return err
	}
	params, err := paymentInsertParams(payment)
	if err != nil {
		return err
	}
	tid, terr := merchant.Require(ctx)
	if terr != nil {
		return terr
	}
	params.MerchantID = tid.UUID()
	if params.PspID == nil {
		params.PspID = db.ResolveRailMerchantAccountIDForStamp(ctx)
	}
	rows, err := r.db.Gen(ctx).CreatePayment(ctx, params)
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (r *PaymentRepo) CreateIfNotExists(ctx context.Context, payment *models.Payment) (bool, error) {
	if err := db.EnsureCustomerRow(ctx, r.db.Qx(ctx), uuid.Nil, payment.CustomerID); err != nil {
		return false, err
	}
	params, err := paymentInsertParams(payment)
	if err != nil {
		return false, err
	}
	tid, terr := merchant.Require(ctx)
	if terr != nil {
		return false, terr
	}
	params.MerchantID = tid.UUID()
	if params.PspID == nil {
		params.PspID = db.ResolveRailMerchantAccountIDForStamp(ctx)
	}
	rows, err := r.db.Gen(ctx).CreatePaymentIfNotExists(ctx, gen.CreatePaymentIfNotExistsParams(params))
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

func (r *PaymentRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.Payment, error) {
	row, err := r.db.Gen(ctx).GetPaymentByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return models.PaymentFromGen(row)
}

// GetByIDWithDetails fetches a payment with all related entities (Price, Product, Subscription)
// and also loads any refund entries linked to this payment
func (r *PaymentRepo) GetByIDWithDetails(ctx context.Context, id uuid.UUID) (*models.Payment, []*models.Payment, error) {
	q := r.db.Gen(ctx)
	row, err := q.GetPaymentWithPriceProduct(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	payment, err := models.PaymentFromGen(row.OpenrailsPayment)
	if err != nil {
		return nil, nil, err
	}
	price, err := models.PriceFromGen(row.OpenrailsPrice)
	if err != nil {
		return nil, nil, err
	}
	product, err := models.ProductFromGen(row.OpenrailsProduct)
	if err != nil {
		return nil, nil, err
	}
	price.Product = product
	payment.Price = price

	if payment.SubscriptionID != nil {
		subRow, err := q.GetSubscriptionByID(ctx, *payment.SubscriptionID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, err
		}
		if err == nil {
			sub, err := models.SubscriptionFromGen(subRow)
			if err != nil {
				return nil, nil, err
			}
			payment.Subscription = sub
		}
	}

	refundRows, err := q.ListRefundsForPayment(ctx, &id)
	if err != nil {
		return nil, nil, err
	}
	refunds, err := models.PaymentsFromGen(refundRows)
	if err != nil {
		return nil, nil, err
	}
	return payment, refunds, nil
}

func (r *PaymentRepo) GetByUserID(ctx context.Context, userID string) ([]*models.Payment, error) {
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListPaymentsByCustomer(ctx, tsid)
	if err != nil {
		return nil, err
	}
	return models.PaymentsFromGen(rows)
}

func (r *PaymentRepo) GetByTransactionID(ctx context.Context, rail models.Rail, transactionID string) (*models.Payment, error) {
	row, err := r.db.Gen(ctx).GetPaymentByTransactionID(ctx, gen.GetPaymentByTransactionIDParams{
		Rail:          string(rail),
		TransactionID: transactionID,
	})
	if err != nil {
		return nil, err
	}
	return models.PaymentFromGen(row)
}

func (r *PaymentRepo) Delete(ctx context.Context, id uuid.UUID) error {
	rows, err := r.db.Gen(ctx).DeletePayment(ctx, id)
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (r *PaymentRepo) GetRefundTotalByPaymentID(ctx context.Context, paymentID uuid.UUID) (int64, error) {
	rows, err := r.db.Gen(ctx).ListRefundRowsForTotal(ctx, &paymentID)
	if err != nil {
		return 0, err
	}
	refunds := make([]refundTotalRow, 0, len(rows))
	for _, row := range rows {
		refunds = append(refunds, refundTotalRow{Amount: row.Amount, Status: string(row.Status)})
	}
	return effectiveRefundTotalFromLinkedRows(refunds), nil
}

func (r *PaymentRepo) LinkRefundedPayment(ctx context.Context, paymentID, originalPaymentID uuid.UUID) error {
	rows, err := r.db.Gen(ctx).LinkRefundedPayment(ctx, gen.LinkRefundedPaymentParams{
		ID:                paymentID,
		RefundedPaymentID: &originalPaymentID,
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

type refundTotalRow struct {
	Amount int64
	Status string
}

func effectiveRefundTotalFromLinkedRows(refunds []refundTotalRow) int64 {
	var total int64
	for _, refund := range refunds {
		if !RefundStatusCountsTowardTotal(refund.Status) {
			continue
		}
		total -= refund.Amount
	}
	if total < 0 {
		return 0
	}
	return total
}

func RefundStatusCountsTowardTotal(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed":
		return false
	default:
		return true
	}
}

func (r *PaymentRepo) GetRefundByAdminIdempotencyKey(ctx context.Context, originalPaymentID uuid.UUID, key string) (*models.Payment, error) {
	row, err := r.db.Gen(ctx).GetRefundByAdminIdempotencyKey(ctx, gen.GetRefundByAdminIdempotencyKeyParams{
		RefundedPaymentID: &originalPaymentID,
		IdemKey:           strings.TrimSpace(key),
	})
	if err != nil {
		return nil, err
	}
	return models.PaymentFromGen(row)
}

func (r *PaymentRepo) CompleteRefundReservation(ctx context.Context, reservationID uuid.UUID, refundTransactionID string, metadata map[string]any) error {
	meta, err := models.ToJSONB(metadata)
	if err != nil {
		return err
	}
	rows, err := r.db.Gen(ctx).CompleteRefundReservation(ctx, gen.CompleteRefundReservationParams{
		ID:            reservationID,
		TransactionID: refundTransactionID,
		Metadata:      meta,
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (r *PaymentRepo) GetByMetadataValue(ctx context.Context, key, value string) (*models.Payment, error) {
	row, err := r.db.Gen(ctx).GetPaymentByMetadataValue(ctx, gen.GetPaymentByMetadataValueParams{
		Key:   strings.TrimSpace(key),
		Value: strings.TrimSpace(value),
	})
	if err != nil {
		return nil, err
	}
	return models.PaymentFromGen(row)
}

func (r *PaymentRepo) CompleteProviderAttempt(ctx context.Context, attemptID uuid.UUID, providerTransactionID string, metadata map[string]any) error {
	meta, err := models.ToJSONB(metadata)
	if err != nil {
		return err
	}
	rows, err := r.db.Gen(ctx).CompleteProviderAttempt(ctx, gen.CompleteProviderAttemptParams{
		ID:            attemptID,
		TransactionID: strings.TrimSpace(providerTransactionID),
		Metadata:      meta,
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (r *PaymentRepo) CompleteProviderAttemptInPlace(ctx context.Context, attemptID uuid.UUID, metadata map[string]any) error {
	meta, err := models.ToJSONB(metadata)
	if err != nil {
		return err
	}
	rows, err := r.db.Gen(ctx).CompleteProviderAttemptInPlace(ctx, gen.CompleteProviderAttemptInPlaceParams{
		ID:       attemptID,
		Metadata: meta,
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (r *PaymentRepo) GetPaginatedByUserID(ctx context.Context, userID string, page, pageSize int) ([]*models.Payment, int, error) {
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, 0, err
	}
	q := r.db.Gen(ctx)
	count, err := q.CountPaymentsByCustomer(ctx, tsid)
	if err != nil {
		return nil, 0, err
	}
	pageSize32, _ := safecast.Convert[int32](pageSize)
	pageOffset32, _ := safecast.Convert[int32]((page - 1) * pageSize)
	rows, err := q.ListPaymentsByCustomerPaged(ctx, gen.ListPaymentsByCustomerPagedParams{
		CustomerID: tsid,
		PageLimit:  pageSize32,
		PageOffset: pageOffset32,
	})
	if err != nil {
		return nil, 0, err
	}
	payments, err := models.PaymentsFromGen(rows)
	if err != nil {
		return nil, 0, err
	}
	return payments, int(count), nil
}

func (r *PaymentRepo) GetPayments(ctx context.Context, opts query.QueryOptions[PaymentFilters]) ([]*models.Payment, int64, error) {
	f := opts.Filters

	var tsid *uuid.UUID
	if f.UserID != "" {
		id, err := db.ResolveCustomerID(f.UserID)
		if err != nil {
			return nil, 0, err
		}
		tsid = &id
	}
	var priceID *uuid.UUID
	if f.PriceID != uuid.Nil {
		priceID = &f.PriceID
	}
	var subID *uuid.UUID
	if f.SubscriptionID != "" {
		if parsed, err := api.ParseSubscriptionID(f.SubscriptionID); err == nil {
			subID = &parsed
		}
	}
	var rail, transactionID, status *string
	if f.Rail != "" {
		rail = &f.Rail
	}
	if f.Status != "" {
		status = &f.Status
	}
	if f.TransactionID != "" {
		transactionID = &f.TransactionID
	}

	q := r.db.Gen(ctx)
	total, err := q.CountPaymentsFiltered(ctx, gen.CountPaymentsFilteredParams{
		CustomerID:      tsid,
		PriceID:         priceID,
		SubscriptionID:  subID,
		Rail:            rail,
		TransactionID:   transactionID,
		PurchasedAfter:  f.StartDate,
		PurchasedBefore: f.EndDate,
		MinAmount:       f.MinAmount,
		MaxAmount:       f.MaxAmount,
		Status:          status,
		RefundsOnly:     f.RefundsOnly,
	})
	if err != nil {
		return nil, 0, err
	}

	sortBy := f.SortBy
	switch sortBy {
	case "amount", "purchased_at":
	default:
		sortBy = "created_at"
	}
	optsLimit32, _ := safecast.Convert[int32](opts.GetLimit())
	optsOffset32, _ := safecast.Convert[int32](opts.GetOffset())
	rows, err := q.ListPaymentsFiltered(ctx, gen.ListPaymentsFilteredParams{
		CustomerID:      tsid,
		PriceID:         priceID,
		SubscriptionID:  subID,
		Rail:            rail,
		TransactionID:   transactionID,
		PurchasedAfter:  f.StartDate,
		PurchasedBefore: f.EndDate,
		MinAmount:       f.MinAmount,
		MaxAmount:       f.MaxAmount,
		Status:          status,
		RefundsOnly:     f.RefundsOnly,
		SortBy:          sortBy,
		SortDesc:        f.SortOrder != "asc",
		PageLimit:       optsLimit32,
		PageOffset:      optsOffset32,
	})
	if err != nil {
		return nil, 0, err
	}
	payments, err := models.PaymentsFromGen(rows)
	if err != nil {
		return nil, 0, err
	}
	if err := r.attachPaymentRelations(ctx, payments); err != nil {
		return nil, 0, err
	}
	return payments, total, nil
}

// attachPaymentRelations stitches Price (+Product) and Subscription onto the
// supplied payments in two batched lookups — the sqlc replacement for bun's
// Relation("Price").Relation("Price.Product").Relation("Subscription").
func (r *PaymentRepo) attachPaymentRelations(ctx context.Context, payments []*models.Payment) error {
	if len(payments) == 0 {
		return nil
	}
	priceIDs := make([]uuid.UUID, 0, len(payments))
	subIDs := make([]uuid.UUID, 0, len(payments))
	seenPrice := map[uuid.UUID]bool{}
	seenSub := map[uuid.UUID]bool{}
	for _, p := range payments {
		if p.PriceID != uuid.Nil && !seenPrice[p.PriceID] {
			seenPrice[p.PriceID] = true
			priceIDs = append(priceIDs, p.PriceID)
		}
		if p.SubscriptionID != nil && !seenSub[*p.SubscriptionID] {
			seenSub[*p.SubscriptionID] = true
			subIDs = append(subIDs, *p.SubscriptionID)
		}
	}

	q := r.db.Gen(ctx)
	prices := map[uuid.UUID]*models.Price{}
	if len(priceIDs) > 0 {
		rows, err := q.ListPricesWithProductByIDs(ctx, priceIDs)
		if err != nil {
			return err
		}
		for _, row := range rows {
			price, err := models.PriceFromGen(row.OpenrailsPrice)
			if err != nil {
				return err
			}
			product, err := models.ProductFromGen(row.OpenrailsProduct)
			if err != nil {
				return err
			}
			price.Product = product
			prices[price.ID] = price
		}
	}
	subs := map[uuid.UUID]*models.Subscription{}
	if len(subIDs) > 0 {
		rows, err := q.ListSubscriptionsByIDs(ctx, subIDs)
		if err != nil {
			return err
		}
		for _, row := range rows {
			sub, err := models.SubscriptionFromGen(row)
			if err != nil {
				return err
			}
			subs[sub.ID] = sub
		}
	}
	for _, p := range payments {
		p.Price = prices[p.PriceID]
		if p.SubscriptionID != nil {
			p.Subscription = subs[*p.SubscriptionID]
		}
	}
	return nil
}

func (r *PaymentRepo) GetLatestChargeBySubscriptionID(ctx context.Context, subscriptionID uuid.UUID) (*models.Payment, error) {
	row, err := r.db.Gen(ctx).GetLatestChargeBySubscriptionID(ctx, &subscriptionID)
	if err != nil {
		return nil, err
	}
	return models.PaymentFromGen(row)
}

func (r *PaymentRepo) MarkFailed(ctx context.Context, id uuid.UUID) error {
	return r.db.Gen(ctx).MarkPaymentFailed(ctx, id)
}
