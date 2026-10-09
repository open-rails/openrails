package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
)

type SubscriptionFilters struct {
	UserID         string
	Status         string
	PriceID        uuid.UUID
	Rail           string
	CreatedAfter   *time.Time
	CreatedBefore  *time.Time
	CanceledAfter  *time.Time
	CanceledBefore *time.Time
	ExpiresBefore  *time.Time
	// Dunning keeps the subscriptions past_due or awaiting_method.
	Dunning bool
}

type SubscriptionRepo struct {
	db *db.DB
}

func NewSubscriptionRepo(d *db.DB) *SubscriptionRepo { return &SubscriptionRepo{db: d} }

func subscriptionInsertParams(s *models.Subscription) (gen.CreateSubscriptionParams, error) {
	var priceID *uuid.UUID
	if s.PriceID != uuid.Nil {
		priceID = &s.PriceID
	}
	var cancelType *string
	if s.CancelType != nil {
		ct := string(*s.CancelType)
		cancelType = &ct
	}
	quantity, err := safecast.Convert[int32](s.Quantity)
	if err != nil {
		return gen.CreateSubscriptionParams{}, fmt.Errorf("subscription quantity: %w", err)
	}
	return gen.CreateSubscriptionParams{
		ID:                          s.ID,
		CustomerID:                  s.CustomerID,
		ProductID:                   s.ProductID,
		PriceID:                     priceID,
		AccessDurationHoursSnapshot: models.IntPtrTo32(s.AccessDurationHoursSnapshot),
		Status:                      string(s.Status),
		PspID:                       s.PspID,
		Quantity:                    quantity,
		StartedAt:                   s.StartedAt,
		EndedAt:                     s.EndedAt,
		CurrentPeriodStartsAt:       s.CurrentPeriodStartsAt,
		CurrentPeriodEndsAt:         s.CurrentPeriodEndsAt,
		Rail:                        string(s.Rail),
		RailSubscriptionID:          s.RailSubscriptionID,
		CollectionPolicy:            string(s.CollectionPolicy),
		PaymentMethodID:             s.PaymentMethodID,
		LastRetryAt:                 s.LastRetryAt,
		RetryAttempts:               models.IntPtrTo32(s.RetryAttempts),
		NextRetryAt:                 s.NextRetryAt,
		GraceEndsAt:                 s.GraceEndsAt,
		CancelFeedback:              s.CancelFeedback,
		CancelType:                  cancelType,
		CanceledAt:                  s.CanceledAt,
		DeletionScheduledAt:         s.DeletionScheduledAt,
		GatewayResponse:             s.Metadata,
		CreatedAt:                   s.CreatedAt,
		UpdatedAt:                   s.UpdatedAt,
	}, nil
}

func (r *SubscriptionRepo) Create(ctx context.Context, s *models.Subscription) error {
	if err := db.EnsureCustomerRow(ctx, r.db.Qx(ctx), uuid.Nil, s.CustomerID); err != nil {
		return err
	}
	params, err := subscriptionInsertParams(s)
	if err != nil {
		return err
	}
	tid, terr := merchant.Require(ctx)
	if terr != nil {
		return terr
	}
	params.MerchantID = tid.UUID()
	if params.PspID == uuid.Nil {
		if params.PspID, err = db.RequirePSPID(ctx); err != nil {
			return fmt.Errorf("create subscription %s/%s: %w", s.Rail, s.RailSubscriptionID, err)
		}
	}
	rows, err := r.db.Gen(ctx).CreateSubscription(ctx, params)
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	s.LifecycleRev, s.RowVersion = 0, 0
	s.RememberLifecycle()
	return nil
}

func (r *SubscriptionRepo) Update(ctx context.Context, s *models.Subscription) error {
	return r.UpdateAt(ctx, s, time.Now())
}

// ReplaceForTierChange atomically persists a tier change: it writes oldSub
// (pre-mutated by the caller to its canceled state) and inserts newSub in ONE
// transaction. The partial unique index
// subscriptions_customer_id_tier_group_key allows only one live
// subscription per (tenant_subject, tier_group), so the old row's cancel and
// the new row's insert must commit together — and the cancel must execute
// first. On any failure the transaction rolls back and the old subscription
// remains active locally (SEC-10: upgrade compensation never has to
// reactivate it).
func (r *SubscriptionRepo) ReplaceForTierChange(ctx context.Context, oldSub, newSub *models.Subscription, now time.Time) error {
	return r.db.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txRepo := NewSubscriptionRepo(r.db.NewWithPgxTx(tx))
		if err := txRepo.UpdateAt(ctx, oldSub, now); err != nil {
			return err
		}
		return txRepo.Create(ctx, newSub)
	})
}

func (r *SubscriptionRepo) UpdateAt(ctx context.Context, s *models.Subscription, now time.Time) error {
	// All columns are written explicitly so nil values CLEAR fields
	// (CanceledAt, EndedAt, ...) when reactivating subscriptions. Because this
	// is a full-row write from an in-memory image, webhook-apply
	// read-modify-writes must read via GetByPSPSubscriptionIDForUpdate inside
	// one tx or a concurrent writer's committed changes get reverted (#675).
	if now.IsZero() {
		now = time.Now()
	}
	s.UpdatedAt = now

	var priceID *uuid.UUID
	if s.PriceID != uuid.Nil {
		priceID = &s.PriceID
	}
	var cancelType *string
	if s.CancelType != nil {
		ct := string(*s.CancelType)
		cancelType = &ct
	}
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return scopeErr
	}
	p := gen.UpdateSubscriptionAtParams{
		MerchantID:                  scopeMerchantID.UUID(),
		ID:                          s.ID,
		PriceID:                     priceID,
		ProductID:                   s.ProductID,
		AccessDurationHoursSnapshot: models.IntPtrTo32(s.AccessDurationHoursSnapshot),
		Status:                      string(s.Status),
		StartedAt:                   s.StartedAt,
		EndedAt:                     s.EndedAt,
		CurrentPeriodStartsAt:       s.CurrentPeriodStartsAt,
		CurrentPeriodEndsAt:         s.CurrentPeriodEndsAt,
		Rail:                        string(s.Rail),
		RailSubscriptionID:          s.RailSubscriptionID,
		PaymentMethodID:             s.PaymentMethodID,
		LastRetryAt:                 s.LastRetryAt,
		RetryAttempts:               models.IntPtrTo32(s.RetryAttempts),
		TransientRetries:            *models.IntPtrTo32(&s.TransientRetries),
		NextRetryAt:                 s.NextRetryAt,
		GraceEndsAt:                 s.GraceEndsAt,
		CancelFeedback:              s.CancelFeedback,
		CancelType:                  cancelType,
		CanceledAt:                  s.CanceledAt,
		DeletionScheduledAt:         s.DeletionScheduledAt,
		GatewayResponse:             s.Metadata,
		UpdatedAt:                   s.UpdatedAt,
		ExpectedVersion:             s.RowVersion,
		DunningPolicy:               s.DunningPolicy,
	}
	if s.LifecycleChanged() {
		// #1091 part C: status, paid period and cancellation change only in a
		// named lifecycle decision, against the revision it was decided on.
		if s.LifecycleDecision() == "" {
			return fmt.Errorf("%w: subscription %s (caller %s)", ErrLifecycleUndecided, s.ID, callerName(2))
		}
		dp := decidedParams(p, s.LifecycleRev)
		dp.Decision = s.LifecycleDecision()
		rows, err := r.db.Gen(ctx).UpdateSubscriptionDecided(ctx, dp)
		if err != nil {
			return fmt.Errorf("save %s decision on subscription %s (caller %s): %w", s.LifecycleDecision(), s.ID, callerName(2), err)
		}
		if rows < 1 {
			return fmt.Errorf("%w: subscription %s changed since it was read (%s, caller %s)", ErrSubscriptionMoved, s.ID, s.LifecycleDecision(), callerName(2))
		}
		s.LifecycleRev++
		s.RowVersion++
		s.RememberLifecycle()
		return nil
	}
	rows, err := r.db.Gen(ctx).UpdateSubscriptionAt(ctx, p)
	if err != nil {
		return fmt.Errorf("update subscription %s (caller %s): %w", s.ID, callerName(2), err)
	}
	if rows < 1 {
		return fmt.Errorf("%w: subscription %s changed since it was read (caller %s)", ErrSubscriptionMoved, s.ID, callerName(2))
	}
	s.RowVersion++
	return nil
}

var (
	// ErrLifecycleUndecided is a write that changes status, paid period or
	// cancellation without a lifecycle decision.
	ErrLifecycleUndecided = errors.New("subscription lifecycle changed outside a lifecycle decision")
	// ErrSubscriptionMoved is a decision made on a row another writer has
	// since changed; the caller re-reads and decides again.
	ErrSubscriptionMoved = errors.New("subscription moved since it was read")
)

func callerName(skip int) string {
	pc, _, line, ok := runtime.Caller(skip)
	if !ok {
		return "unknown"
	}
	name := "unknown"
	if fn := runtime.FuncForPC(pc); fn != nil {
		name = fn.Name()
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
	}
	return fmt.Sprintf("%s:%d", name, line)
}

// attachSubscriptionRelations stitches Price and PaymentMethod (the bun-era
// selectWithDetails relations) onto subs; withProduct additionally loads
// Price.Product (selectWithProduct).
func (r *SubscriptionRepo) attachSubscriptionRelations(ctx context.Context, subs []*models.Subscription, withProduct bool) error {
	if len(subs) == 0 {
		return nil
	}
	priceIDs := make([]uuid.UUID, 0, len(subs))
	pmIDs := make([]uuid.UUID, 0, len(subs))
	seenPrice := map[uuid.UUID]bool{}
	seenPM := map[uuid.UUID]bool{}
	for _, s := range subs {
		if s.PriceID != uuid.Nil && !seenPrice[s.PriceID] {
			seenPrice[s.PriceID] = true
			priceIDs = append(priceIDs, s.PriceID)
		}
		if s.PaymentMethodID != nil && !seenPM[*s.PaymentMethodID] {
			seenPM[*s.PaymentMethodID] = true
			pmIDs = append(pmIDs, *s.PaymentMethodID)
		}
	}

	q := r.db.Gen(ctx)
	prices := map[uuid.UUID]*models.Price{}
	if len(priceIDs) > 0 {
		if withProduct {
			scopeMerchantID, scopeErr := merchant.Require(ctx)
			if scopeErr != nil {
				return scopeErr
			}
			rows, err := q.ListPricesWithProductByIDs(ctx, gen.ListPricesWithProductByIDsParams{MerchantID: scopeMerchantID.UUID(), Ids: priceIDs})
			if err != nil {
				return err
			}
			for _, row := range rows {
				price, err := models.PriceFromGen(row.BillingPrice)
				if err != nil {
					return err
				}
				product, err := r.db.ProductFromGen(ctx, row.BillingProduct)
				if err != nil {
					return err
				}
				price.Product = product
				prices[price.ID] = price
			}
		} else {
			scopeMerchantID, scopeErr := merchant.Require(ctx)
			if scopeErr != nil {
				return scopeErr
			}
			rows, err := q.ListPricesByIDs(ctx, gen.ListPricesByIDsParams{MerchantID: scopeMerchantID.UUID(), Ids: priceIDs})
			if err != nil {
				return err
			}
			for _, row := range rows {
				price, err := models.PriceFromGen(row)
				if err != nil {
					return err
				}
				prices[price.ID] = price
			}
		}
	}
	bindingPrices := make([]*models.Price, 0, len(prices))
	for _, price := range prices {
		bindingPrices = append(bindingPrices, price)
	}
	if err := r.db.LoadPricePSPBindings(ctx, bindingPrices, nil); err != nil {
		return err
	}
	pms := map[uuid.UUID]*models.PaymentMethod{}
	if len(pmIDs) > 0 {
		scopeMerchantID, scopeErr := merchant.Require(ctx)
		if scopeErr != nil {
			return scopeErr
		}
		rows, err := q.ListPaymentMethodsByIDs(ctx, gen.ListPaymentMethodsByIDsParams{MerchantID: scopeMerchantID.UUID(), Ids: pmIDs})
		if err != nil {
			return err
		}
		for _, row := range rows {
			pm, err := models.PaymentMethodFromGen(row)
			if err != nil {
				return err
			}
			pms[pm.ID] = pm
		}
	}
	for _, s := range subs {
		s.Price = prices[s.PriceID].ForPSP(s.PspID)
		if s.PaymentMethodID != nil {
			s.PaymentMethod = pms[*s.PaymentMethodID]
		}
	}
	return nil
}

// oneWithDetails maps a single gen row and attaches the standard relations.
func (r *SubscriptionRepo) oneWithDetails(ctx context.Context, row gen.BillingSubscription, withProduct bool) (*models.Subscription, error) {
	sub, err := models.SubscriptionFromGen(row)
	if err != nil {
		return nil, err
	}
	if err := r.attachSubscriptionRelations(ctx, []*models.Subscription{sub}, withProduct); err != nil {
		return nil, err
	}
	return sub, nil
}

func (r *SubscriptionRepo) manyWithDetails(ctx context.Context, rows []gen.BillingSubscription) ([]*models.Subscription, error) {
	subs, err := models.SubscriptionsFromGen(rows)
	if err != nil {
		return nil, err
	}
	if err := r.attachSubscriptionRelations(ctx, subs, false); err != nil {
		return nil, err
	}
	return subs, nil
}

func (r *SubscriptionRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.Subscription, error) {
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}
	row, err := r.db.Gen(ctx).GetSubscriptionByID(ctx, gen.GetSubscriptionByIDParams{MerchantID: scopeMerchantID.UUID(), ID: id})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}

// GetByIDForUpdate holds the subscription row until the caller's transaction
// finishes. Lifecycle mutations must lock before reading a full-row snapshot.
func (r *SubscriptionRepo) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*models.Subscription, error) {
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}
	row, err := r.db.Gen(ctx).GetSubscriptionByIDForUpdate(ctx, gen.GetSubscriptionByIDForUpdateParams{MerchantID: scopeMerchantID.UUID(), ID: id})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}

func (r *SubscriptionRepo) GetLatestByUserID(ctx context.Context, userID string) (*models.Subscription, error) {
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}
	row, err := r.db.Gen(ctx).GetLatestSubscriptionByCustomer(ctx, gen.GetLatestSubscriptionByCustomerParams{MerchantID: scopeMerchantID.UUID(), CustomerID: tsid})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}

func (r *SubscriptionRepo) GetByUserIDAndPriceID(ctx context.Context, userID string, priceID uuid.UUID) (*models.Subscription, error) {
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}
	row, err := r.db.Gen(ctx).GetSubscriptionByCustomerAndPrice(ctx, gen.GetSubscriptionByCustomerAndPriceParams{
		MerchantID: scopeMerchantID.UUID(),
		CustomerID: tsid,
		PriceID:    &priceID,
	})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}

// GetActiveOrPendingByUserIDAndProductID finds any lifecycle-owning subscription for a user and product.
// Returns the subscription with the latest period end date.
func (r *SubscriptionRepo) GetActiveOrPendingByUserIDAndProductID(ctx context.Context, userID string, productID uuid.UUID) (*models.Subscription, error) {
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}
	row, err := r.db.Gen(ctx).GetLifecycleSubscriptionByCustomerAndProduct(ctx, gen.GetLifecycleSubscriptionByCustomerAndProductParams{
		MerchantID: scopeMerchantID.UUID(),
		CustomerID: tsid,
		ProductID:  productID,
	})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}

func (r *SubscriptionRepo) GetActiveSubscription(ctx context.Context, userID string) (*models.Subscription, error) {
	return r.GetActiveSubscriptionAt(ctx, userID, time.Now())
}

func (r *SubscriptionRepo) GetActiveSubscriptionAt(ctx context.Context, userID string, now time.Time) (*models.Subscription, error) {
	if now.IsZero() {
		now = time.Now()
	}
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}
	row, err := r.db.Gen(ctx).GetActiveSubscriptionByCustomerAt(ctx, gen.GetActiveSubscriptionByCustomerAtParams{
		MerchantID: scopeMerchantID.UUID(),
		CustomerID: tsid,
		Now:        now,
	})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}

// GetByPSPSubscriptionIDForUpdate is the row-locked (FOR UPDATE) variant for
// webhook-apply read-modify-writes (#675). Must run inside a transaction;
// UpdateAt is a full-row write, so the lock must be held from read to write.
func (r *SubscriptionRepo) GetByPSPSubscriptionIDForUpdate(ctx context.Context, rail, railSubscriptionID string) (*models.Subscription, error) {
	railSubscriptionID = strings.TrimSpace(railSubscriptionID)
	if railSubscriptionID == "" {
		return nil, errors.New("provider subscription reference is required")
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	pspID, err := db.RequirePSPID(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).GetSubscriptionByPSPSubIDForUpdate(ctx, gen.GetSubscriptionByPSPSubIDForUpdateParams{
		MerchantID:         merchantID.UUID(),
		PspID:              pspID,
		Rail:               rail,
		RailSubscriptionID: railSubscriptionID,
	})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}

// GetByPSPSubscriptionID finds a subscription by rail and rail_subscription_id.
func (r *SubscriptionRepo) GetByPSPSubscriptionID(ctx context.Context, rail, railSubscriptionID string) (*models.Subscription, error) {
	railSubscriptionID = strings.TrimSpace(railSubscriptionID)
	if railSubscriptionID == "" {
		return nil, errors.New("provider subscription reference is required")
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	pspID, err := db.RequirePSPID(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).GetSubscriptionByPSPSubID(ctx, gen.GetSubscriptionByPSPSubIDParams{
		MerchantID:         merchantID.UUID(),
		PspID:              pspID,
		Rail:               rail,
		RailSubscriptionID: railSubscriptionID,
	})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}

// GetByGatewayOrder reads the subscription whose gateway response names the
// order reference.
func (r *SubscriptionRepo) GetByGatewayOrder(ctx context.Context, rail, orderID string) (*models.Subscription, error) {
	if strings.TrimSpace(orderID) == "" {
		return nil, errors.New("gateway order reference is required")
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	pspID, err := db.RequirePSPID(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).GetSubscriptionByGatewayOrder(ctx, gen.GetSubscriptionByGatewayOrderParams{
		MerchantID: merchantID.UUID(),
		PspID:      pspID,
		Rail:       strings.TrimSpace(rail),
		OrderID:    strings.TrimSpace(orderID),
	})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}

func (r *SubscriptionRepo) GetActiveSubscriptionsByUserID(ctx context.Context, userID string) ([]models.Subscription, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListActiveSubscriptionsByCustomer(ctx, gen.ListActiveSubscriptionsByCustomerParams{MerchantID: tid.UUID(), CustomerID: tsid})
	if err != nil {
		return nil, err
	}
	subs, err := r.manyWithDetails(ctx, rows)
	if err != nil {
		return nil, err
	}
	return derefSubs(subs), nil
}

func (r *SubscriptionRepo) GetActiveSubscriptionsForPSP(ctx context.Context, rail string) ([]*models.Subscription, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	pspID, err := db.RequirePSPID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListActiveSubscriptionsForPSP(ctx, gen.ListActiveSubscriptionsForPSPParams{MerchantID: mid.UUID(), PspID: pspID, Rail: rail})
	if err != nil {
		return nil, err
	}
	return r.manyWithDetails(ctx, rows)
}

func (r *SubscriptionRepo) GetPaginatedByUserID(ctx context.Context, userID string, page, pageSize int) ([]models.Subscription, int, error) {
	return r.GetSubscriptionsWithDetailsForUser(ctx, userID, page, pageSize)
}

func (r *SubscriptionRepo) GetSubscriptionsWithDetailsForUser(ctx context.Context, userID string, page, pageSize int) ([]models.Subscription, int, error) {
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, 0, err
	}
	q := r.db.Gen(ctx)
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return nil, 0, scopeErr
	}
	total, err := q.CountSubscriptionsByCustomer(ctx, gen.CountSubscriptionsByCustomerParams{MerchantID: scopeMerchantID.UUID(), CustomerID: tsid})
	if err != nil {
		return nil, 0, err
	}
	pageSize32, _ := safecast.Convert[int32](pageSize)
	pageOffset32, _ := safecast.Convert[int32]((page - 1) * pageSize)
	rows, err := q.ListSubscriptionsByCustomerPaged(ctx, gen.ListSubscriptionsByCustomerPagedParams{
		MerchantID: scopeMerchantID.UUID(),
		CustomerID: tsid,
		PageLimit:  pageSize32,
		PageOffset: pageOffset32,
	})
	if err != nil {
		return nil, 0, err
	}
	subs, err := r.manyWithDetails(ctx, rows)
	if err != nil {
		return nil, 0, err
	}
	return derefSubs(subs), int(total), nil
}

// filterArgs are a list's filters as query arguments.
func (r *SubscriptionRepo) filterArgs(f SubscriptionFilters) (customer *uuid.UUID, status, rail *string, price *uuid.UUID, err error) {
	if f.UserID != "" {
		id, err := db.ResolveCustomerID(f.UserID)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		customer = &id
	}
	if f.Status != "" {
		status = &f.Status
	}
	if f.Rail != "" {
		rail = &f.Rail
	}
	if f.PriceID != uuid.Nil {
		price = &f.PriceID
	}
	return customer, status, rail, price, nil
}

// ListPage is one page of the merchant's subscriptions matching f, newest
// first, with their details: up to fetch rows after (afterAt, afterID).
func (r *SubscriptionRepo) ListPage(ctx context.Context, f SubscriptionFilters, fetch int32, afterAt *time.Time, afterID *uuid.UUID) ([]*models.Subscription, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	customer, status, rail, price, err := r.filterArgs(f)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListSubscriptionsPage(ctx, gen.ListSubscriptionsPageParams{
		MerchantID: mid.UUID(), CustomerID: customer, Status: status, PriceID: price, Rail: rail,
		CreatedAfter: f.CreatedAfter, CreatedBefore: f.CreatedBefore, CanceledAfter: f.CanceledAfter,
		CanceledBefore: f.CanceledBefore, ExpiresBefore: f.ExpiresBefore, Dunning: f.Dunning,
		AfterAt: afterAt, AfterID: afterID, RowLimit: fetch,
	})
	if err != nil {
		return nil, err
	}
	return r.manyWithDetails(ctx, rows)
}

// ListByIDs reads the named subscriptions, newest first.
func (r *SubscriptionRepo) ListByIDs(ctx context.Context, ids []uuid.UUID) ([]*models.Subscription, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListSubscriptionsByIDs(ctx, gen.ListSubscriptionsByIDsParams{MerchantID: mid.UUID(), Ids: ids})
	if err != nil {
		return nil, err
	}
	return r.manyWithDetails(ctx, rows)
}

// Count is how many of the merchant's subscriptions match f.
func (r *SubscriptionRepo) Count(ctx context.Context, f SubscriptionFilters) (int64, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	customer, status, rail, price, err := r.filterArgs(f)
	if err != nil {
		return 0, err
	}
	return r.db.Gen(ctx).CountSubscriptionsFiltered(ctx, gen.CountSubscriptionsFilteredParams{
		MerchantID: mid.UUID(), CustomerID: customer, Status: status, PriceID: price, Rail: rail,
		CreatedAfter: f.CreatedAfter, CreatedBefore: f.CreatedBefore, CanceledAfter: f.CanceledAfter,
		CanceledBefore: f.CanceledBefore, ExpiresBefore: f.ExpiresBefore, Dunning: f.Dunning,
	})
}

// GetActiveOrPendingByUserIDAndTierGroup finds a lifecycle-owning subscription for a user
// where the product belongs to the specified tier group.
// Returns the subscription with its Price and Product loaded.
func (r *SubscriptionRepo) GetActiveOrPendingByUserIDAndTierGroup(ctx context.Context, userID string, tierGroup string) (*models.Subscription, error) {
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}
	row, err := r.db.Gen(ctx).GetLifecycleSubscriptionByCustomerAndTierGroup(ctx, gen.GetLifecycleSubscriptionByCustomerAndTierGroupParams{
		MerchantID: scopeMerchantID.UUID(),
		CustomerID: tsid,
		TierGroup:  &tierGroup,
	})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, true)
}

// GetUnknownByUserIDAndProductID finds an `unknown`-status subscription for a
// user and product (#691 checkout guard): parked pending provider verification,
// possibly still alive/billing at the provider.
func (r *SubscriptionRepo) GetUnknownByUserIDAndProductID(ctx context.Context, userID string, productID uuid.UUID) (*models.Subscription, error) {
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}
	row, err := r.db.Gen(ctx).GetUnknownSubscriptionByCustomerAndProduct(ctx, gen.GetUnknownSubscriptionByCustomerAndProductParams{
		MerchantID: scopeMerchantID.UUID(),
		CustomerID: tsid,
		ProductID:  productID,
	})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}

// GetUnknownByUserIDAndTierGroup is the tier-group variant of the #691 checkout
// guard lookup. Returns the subscription with Price and Product loaded.
func (r *SubscriptionRepo) GetUnknownByUserIDAndTierGroup(ctx context.Context, userID string, tierGroup string) (*models.Subscription, error) {
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}
	row, err := r.db.Gen(ctx).GetUnknownSubscriptionByCustomerAndTierGroup(ctx, gen.GetUnknownSubscriptionByCustomerAndTierGroupParams{
		MerchantID: scopeMerchantID.UUID(),
		CustomerID: tsid,
		TierGroup:  &tierGroup,
	})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, true)
}

func derefSubs(subs []*models.Subscription) []models.Subscription {
	out := make([]models.Subscription, 0, len(subs))
	for _, s := range subs {
		out = append(out, *s)
	}
	return out
}

// DueDunningBatch bounds ONE merchant's dunning pass (or#837). Each returned
// row can charge a card and terminate a subscription, so an uncapped list was
// an unbounded burst of provider calls in a single job. Most-overdue first (the
// query's order), and the claim lease means the remainder is simply the next
// pass's head — nothing is skipped, only paced.
const DueDunningBatch = 500

// ListDueDunningSubscriptions returns past_due subscriptions on the given
// rails whose next retry is due (Price + PaymentMethod relations
// attached) — the dunning worker's work list, capped at DueDunningBatch.
func (r *SubscriptionRepo) ListDueDunningSubscriptions(ctx context.Context, rails []string, now time.Time, includeEngine bool) ([]models.Subscription, error) {
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}
	rows, err := r.db.Gen(ctx).ListDueDunningSubscriptions(ctx, gen.ListDueDunningSubscriptionsParams{
		IncludeEngine: includeEngine,
		MerchantID:    scopeMerchantID.UUID(),
		Rails:         rails,
		Now:           now,
		RowLimit:      DueDunningBatch,
	})
	if err != nil {
		return nil, err
	}
	subs, err := r.manyWithDetails(ctx, rows)
	if err != nil {
		return nil, err
	}
	out := make([]models.Subscription, 0, len(subs))
	for _, s := range subs {
		out = append(out, *s)
	}
	return out, nil
}

// ListOverdueRebills returns auto-renewing subscriptions whose period ended
// before its owner's cutoff with neither an attempt nor a recorded miss for
// that cycle (#1112).
func (r *SubscriptionRepo) ListOverdueRebills(ctx context.Context, engineCutoff, nmiCutoff time.Time) ([]*models.Subscription, error) {
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}
	rows, err := r.db.Gen(ctx).ListOverdueRebills(ctx, gen.ListOverdueRebillsParams{
		MerchantID: scopeMerchantID.UUID(), EngineCutoff: engineCutoff, NmiCutoff: nmiCutoff, RowLimit: DueDunningBatch,
	})
	if err != nil {
		return nil, err
	}
	return r.manyWithDetails(ctx, rows)
}

// GetLatestResumableCanceled returns the payer's most recent canceled
// subscription whose paid period has not elapsed (resume candidate), or
// pgx.ErrNoRows.
func (r *SubscriptionRepo) GetLatestResumableCanceled(ctx context.Context, tenantSubjectID uuid.UUID, now time.Time) (*models.Subscription, error) {
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}
	row, err := r.db.Gen(ctx).GetLatestResumableCanceledSubscription(ctx, gen.GetLatestResumableCanceledSubscriptionParams{
		MerchantID: scopeMerchantID.UUID(),
		CustomerID: tenantSubjectID,
		Now:        now,
	})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}

func decidedParams(p gen.UpdateSubscriptionAtParams, rev int64) gen.UpdateSubscriptionDecidedParams {
	return gen.UpdateSubscriptionDecidedParams{
		ID: p.ID, PriceID: p.PriceID, ProductID: p.ProductID, AccessDurationHoursSnapshot: p.AccessDurationHoursSnapshot, Status: p.Status,
		StartedAt: p.StartedAt, EndedAt: p.EndedAt, CurrentPeriodStartsAt: p.CurrentPeriodStartsAt, CurrentPeriodEndsAt: p.CurrentPeriodEndsAt,
		Rail: p.Rail, RailSubscriptionID: p.RailSubscriptionID, PaymentMethodID: p.PaymentMethodID,
		LastRetryAt: p.LastRetryAt, RetryAttempts: p.RetryAttempts, TransientRetries: p.TransientRetries, NextRetryAt: p.NextRetryAt,
		GraceEndsAt: p.GraceEndsAt, CancelFeedback: p.CancelFeedback, CancelType: p.CancelType, CanceledAt: p.CanceledAt,
		DeletionScheduledAt: p.DeletionScheduledAt, GatewayResponse: p.GatewayResponse,
		UpdatedAt: p.UpdatedAt, MerchantID: p.MerchantID, ExpectedRev: rev, ExpectedVersion: p.ExpectedVersion, DunningPolicy: p.DunningPolicy,
	}
}
