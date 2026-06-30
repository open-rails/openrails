package repo

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
	RefundsOnly    bool       `form:"refunds_only"`
	SortBy         string     `form:"sort_by"`    // created_at (default), amount, purchased_at
	SortOrder      string     `form:"sort_order"` // asc, desc (default)
}

type PaymentRepo struct {
	db *db.DB
}

const internalNMISubscriptionAttemptMetadataKey = "nmi_subscription_order_id"

func NewPaymentRepo(d *db.DB) *PaymentRepo { return &PaymentRepo{db: d} }

// paymentInsertParams maps a model onto the insert parameter set shared by
// CreatePayment and CreatePaymentIfNotExists (identical column lists).
func paymentInsertParams(p *models.Payment) (gen.CreatePaymentParams, error) {
	currency := strings.TrimSpace(p.Currency)
	if currency == "" {
		return gen.CreatePaymentParams{}, fmt.Errorf("payment currency required")
	}
	discountMeta, err := toJSONB(p.DiscountMetadata)
	if err != nil {
		return gen.CreatePaymentParams{}, err
	}
	meta, err := toJSONB(p.Metadata)
	if err != nil {
		return gen.CreatePaymentParams{}, err
	}
	entSnap, err := toJSONB(p.EntitlementsSpecSnapshot)
	if err != nil {
		return gen.CreatePaymentParams{}, err
	}
	credSnap, err := toJSONB(p.CreditsSpecSnapshot)
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
	}, nil
}

func (r *PaymentRepo) Create(ctx context.Context, payment *models.Payment) error {
	if err := ensureCustomerRow(ctx, r.db.Qx(ctx), uuid.Nil, payment.CustomerID); err != nil {
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
	if err := ensureCustomerRow(ctx, r.db.Qx(ctx), uuid.Nil, payment.CustomerID); err != nil {
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
	return paymentFromGen(row)
}

// GetByIDWithDetails fetches a payment with all related entities (Price, Product, Subscription)
// and also loads any refund entries linked to this payment
func (r *PaymentRepo) GetByIDWithDetails(ctx context.Context, id uuid.UUID) (*models.Payment, []*models.Payment, error) {
	q := r.db.Gen(ctx)
	row, err := q.GetPaymentWithPriceProduct(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	payment, err := paymentFromGen(row.OpenrailsPayment)
	if err != nil {
		return nil, nil, err
	}
	price, err := priceFromGen(row.OpenrailsPrice)
	if err != nil {
		return nil, nil, err
	}
	product, err := productFromGen(row.OpenrailsProduct)
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
			sub, err := subscriptionFromGen(subRow)
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
	refunds, err := paymentsFromGen(refundRows)
	if err != nil {
		return nil, nil, err
	}
	return payment, refunds, nil
}

func (r *PaymentRepo) GetByUserID(ctx context.Context, userID string) ([]*models.Payment, error) {
	tsid, err := ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListPaymentsByCustomer(ctx, tsid)
	if err != nil {
		return nil, err
	}
	return paymentsFromGen(rows)
}

func (r *PaymentRepo) GetByTransactionID(ctx context.Context, rail models.Rail, transactionID string) (*models.Payment, error) {
	row, err := r.db.Gen(ctx).GetPaymentByTransactionID(ctx, gen.GetPaymentByTransactionIDParams{
		Rail:          string(rail),
		TransactionID: transactionID,
	})
	if err != nil {
		return nil, err
	}
	return paymentFromGen(row)
}

func (r *PaymentRepo) GetByPriceID(ctx context.Context, priceID uuid.UUID) ([]*models.Payment, error) {
	rows, err := r.db.Gen(ctx).ListPaymentsByPriceID(ctx, priceID)
	if err != nil {
		return nil, err
	}
	return paymentsFromGen(rows)
}

func (r *PaymentRepo) GetByRail(ctx context.Context, rail models.Rail) ([]*models.Payment, error) {
	rows, err := r.db.Gen(ctx).ListPaymentsByRail(ctx, string(rail))
	if err != nil {
		return nil, err
	}
	return paymentsFromGen(rows)
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
	return paymentFromGen(row)
}

func (r *PaymentRepo) CompleteRefundReservation(ctx context.Context, reservationID uuid.UUID, refundTransactionID string, metadata map[string]any) error {
	meta, err := toJSONB(metadata)
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
	return paymentFromGen(row)
}

func (r *PaymentRepo) CompleteProviderAttempt(ctx context.Context, attemptID uuid.UUID, providerTransactionID string, metadata map[string]any) error {
	meta, err := toJSONB(metadata)
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
	meta, err := toJSONB(metadata)
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
	tsid, err := ResolveCustomerID(userID)
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
	payments, err := paymentsFromGen(rows)
	if err != nil {
		return nil, 0, err
	}
	return payments, int(count), nil
}

func (r *PaymentRepo) GetPayments(ctx context.Context, opts query.QueryOptions[PaymentFilters]) ([]*models.Payment, int64, error) {
	f := opts.Filters

	var tsid *uuid.UUID
	if f.UserID != "" {
		id, err := ResolveCustomerID(f.UserID)
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
	var rail, transactionID *string
	if f.Rail != "" {
		rail = &f.Rail
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
		RefundsOnly:     f.RefundsOnly,
		SortBy:          sortBy,
		SortDesc:        f.SortOrder != "asc",
		PageLimit:       optsLimit32,
		PageOffset:      optsOffset32,
	})
	if err != nil {
		return nil, 0, err
	}
	payments, err := paymentsFromGen(rows)
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
			price, err := priceFromGen(row.OpenrailsPrice)
			if err != nil {
				return err
			}
			product, err := productFromGen(row.OpenrailsProduct)
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
			sub, err := subscriptionFromGen(row)
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

func (r *PaymentRepo) GetLatestByUserAndRail(ctx context.Context, userID string, rail models.Rail) (*models.Payment, error) {
	tsid, err := ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).GetLatestPaymentByCustomerRail(ctx, gen.GetLatestPaymentByCustomerRailParams{
		CustomerID: tsid,
		Rail:       string(rail),
	})
	if err != nil {
		return nil, err
	}
	return paymentFromGen(row)
}

func (r *PaymentRepo) GetLatestBySubscriptionID(ctx context.Context, subscriptionID uuid.UUID) (*models.Payment, error) {
	row, err := r.db.Gen(ctx).GetLatestPaymentBySubscriptionID(ctx, &subscriptionID)
	if err != nil {
		return nil, err
	}
	return paymentFromGen(row)
}

func (r *PaymentRepo) GetLatestChargeBySubscriptionID(ctx context.Context, subscriptionID uuid.UUID) (*models.Payment, error) {
	row, err := r.db.Gen(ctx).GetLatestChargeBySubscriptionID(ctx, &subscriptionID)
	if err != nil {
		return nil, err
	}
	return paymentFromGen(row)
}

func (r *PaymentRepo) CountByUserAndRail(ctx context.Context, userID string, rail models.Rail) (successful int, failed int, err error) {
	if userID == "" {
		return 0, 0, fmt.Errorf("user_id is required")
	}
	tsid, err := ResolveCustomerID(userID)
	if err != nil {
		return 0, 0, err
	}
	row, err := r.db.Gen(ctx).CountPaymentOutcomesBySubjectRail(ctx, gen.CountPaymentOutcomesBySubjectRailParams{
		CustomerID: tsid,
		Rail:       string(rail),
	})
	if err != nil {
		return 0, 0, err
	}
	return int(row.Successful), int(row.Failed), nil
}

func (r *PaymentRepo) MarkFailed(ctx context.Context, id uuid.UUID) error {
	return r.db.Gen(ctx).MarkPaymentFailed(ctx, id)
}
