package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/convergejob"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/history"
	"github.com/open-rails/openrails/internal/merchant"
	log "github.com/sirupsen/logrus"
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
	// Sort picks ListPage's order; Count ignores it.
	Sort billing.SubscriptionSort
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
	return gen.CreateSubscriptionParams{
		ID:                          s.ID,
		CustomerID:                  s.CustomerID,
		ProductID:                   s.ProductID,
		PriceID:                     priceID,
		AccessDurationHoursSnapshot: models.IntPtrTo32(s.AccessDurationHoursSnapshot), ProviderAccessDefaultSnapshot: s.ProviderAccessDefaultSnapshot,
		Status:                string(s.Status),
		PspID:                 s.PspID,
		Quantity:              models.IntPtrTo32(s.Quantity),
		StartedAt:             s.StartedAt,
		EndedAt:               s.EndedAt,
		CurrentPeriodStartsAt: s.CurrentPeriodStartsAt,
		CurrentPeriodEndsAt:   s.CurrentPeriodEndsAt,
		Rail:                  string(s.Rail),
		RailSubscriptionID:    s.RailSubscriptionID,
		CollectionPolicy:      string(s.CollectionPolicy),
		PaymentMethodID:       s.PaymentMethodID,
		LastRetryAt:           s.LastRetryAt,
		RetryAttempts:         models.IntPtrTo32(s.RetryAttempts),
		NextRetryAt:           s.NextRetryAt,
		GraceEndsAt:           s.GraceEndsAt,
		CancelFeedback:        s.CancelFeedback,
		CancelType:            cancelType,
		CanceledAt:            s.CanceledAt,
		DeletionScheduledAt:   s.DeletionScheduledAt,
		GatewayResponse:       s.Metadata,
		CreatedAt:             s.CreatedAt,
		UpdatedAt:             s.UpdatedAt,
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
	params.HistoryAttribution = history.Bind(ctx, tid.UUID(), s.ID, "insert", 0, "created")
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
	// The model is the row: its PSP and the provider's collection when none
	// was named.
	s.PspID = params.PspID
	if s.CollectionPolicy == "" {
		s.CollectionPolicy = models.CollectionPolicyProvider
	}
	s.LifecycleRev, s.RowVersion = 0, 0
	s.RememberLifecycle()
	return convergejob.Queue(ctx, r.db, tid, s.CustomerID)
}

func (r *SubscriptionRepo) Update(ctx context.Context, s *models.Subscription) error {
	return r.UpdateAt(ctx, s, time.Now())
}

// ReplaceForTierChange writes oldSub (pre-mutated by the caller to its
// canceled state) and inserts newSub in one transaction, the cancel first:
// subscriptions_customer_id_tier_group_key allows one live subscription per
// (customer, tier_group). On failure the old subscription stays active.
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
	if s.Status == models.StatusCanceled && r.db.Notifications() != nil {
		return r.db.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			txdb := r.db.NewWithPgxTx(tx)
			if err := NewSubscriptionRepo(txdb).updateAt(ctx, s, now); err != nil {
				return err
			}
			return WithdrawSubscriptionPaymentNotices(ctx, txdb, s.CustomerID, s.ID)
		})
	}
	return r.updateAt(ctx, s, now)
}

func (r *SubscriptionRepo) updateAt(ctx context.Context, s *models.Subscription, now time.Time) error {
	// Every column is written, so nil values clear fields (CanceledAt,
	// EndedAt, ...). A full-row write from an in-memory image: webhook-apply
	// read-modify-writes must read via GetByPSPSubscriptionIDForUpdate in one
	// tx, or a concurrent writer's committed changes are reverted.
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
		AccessDurationHoursSnapshot: models.IntPtrTo32(s.AccessDurationHoursSnapshot), ProviderAccessDefaultSnapshot: s.ProviderAccessDefaultSnapshot,
		Status:                string(s.Status),
		StartedAt:             s.StartedAt,
		EndedAt:               s.EndedAt,
		CurrentPeriodStartsAt: s.CurrentPeriodStartsAt,
		CurrentPeriodEndsAt:   s.CurrentPeriodEndsAt,
		Rail:                  string(s.Rail),
		RailSubscriptionID:    s.RailSubscriptionID,
		PaymentMethodID:       s.PaymentMethodID,
		LastRetryAt:           s.LastRetryAt,
		RetryAttempts:         models.IntPtrTo32(s.RetryAttempts),
		TransientRetries:      *models.IntPtrTo32(&s.TransientRetries),
		NextRetryAt:           s.NextRetryAt,
		GraceEndsAt:           s.GraceEndsAt,
		CancelFeedback:        s.CancelFeedback,
		CancelType:            cancelType,
		CanceledAt:            s.CanceledAt,
		DeletionScheduledAt:   s.DeletionScheduledAt,
		GatewayResponse:       s.Metadata,
		Quantity:              models.IntPtrTo32(s.Quantity),
		UpdatedAt:             s.UpdatedAt,
		ExpectedVersion:       s.RowVersion,
		HistoryAttribution:    history.Bind(ctx, scopeMerchantID.UUID(), s.ID, "update", s.RowVersion, s.HistoryDecision()),
		DunningPolicy:         s.DunningPolicy,
	}
	if s.LifecycleChanged() {
		// Status, paid period and cancellation change only in a named
		// lifecycle decision, against the revision it was decided on.
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
		return convergejob.Queue(ctx, r.db, scopeMerchantID, s.CustomerID)
	}
	rows, err := r.db.Gen(ctx).UpdateSubscriptionAt(ctx, p)
	if err != nil {
		return fmt.Errorf("update subscription %s (caller %s): %w", s.ID, callerName(2), err)
	}
	if rows < 1 {
		return fmt.Errorf("%w: subscription %s changed since it was read (caller %s)", ErrSubscriptionMoved, s.ID, callerName(2))
	}
	s.RowVersion++
	s.RecordHistoryDecision("")
	return nil
}

func (r *SubscriptionRepo) LockTierChangeTx(ctx context.Context, txDB *db.DB, id uuid.UUID) (*models.Subscription, error) {
	return NewSubscriptionRepo(txDB).GetByIDForUpdate(ctx, id)
}

// SaveSolanaTierChangeTx records the confirmation result in the caller's transaction.
func (r *SubscriptionRepo) SaveSolanaTierChangeTx(ctx context.Context, txDB *db.DB, id uuid.UUID, receipt *models.SolanaTierChangeReceipt) error {
	repo := NewSubscriptionRepo(txDB)
	// Re-read after cancellation so its lifecycle fields and revision are retained.
	sub, err := repo.GetByIDForUpdate(ctx, id)
	if err != nil {
		return err
	}
	metadata := make(map[string]json.RawMessage)
	if len(sub.Metadata) != 0 {
		if err := json.Unmarshal(sub.Metadata, &metadata); err != nil {
			return err
		}
	}
	if metadata == nil {
		metadata = make(map[string]json.RawMessage)
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	metadata["solana_tier_change_receipt"] = encoded
	sub.Metadata, err = json.Marshal(metadata)
	if err != nil {
		return err
	}
	return repo.UpdateAt(ctx, sub, sub.UpdatedAt)
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

// attachSubscriptionRelations stitches Price and PaymentMethod onto subs;
// withProduct also loads Price.Product.
func (r *SubscriptionRepo) attachSubscriptionRelations(ctx context.Context, subs []*models.Subscription, withProduct bool) error {
	if len(subs) == 0 {
		return nil
	}
	priceIDs := make([]uuid.UUID, 0, len(subs))
	seenPrice := map[uuid.UUID]bool{}
	for _, s := range subs {
		if s.PriceID != uuid.Nil && !seenPrice[s.PriceID] {
			seenPrice[s.PriceID] = true
			priceIDs = append(priceIDs, s.PriceID)
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
	// PaymentMethod is the card each charges: its own, or the default it follows.
	scope, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	charged, err := paymentMethodsOf(ctx, q, scope.UUID(), subs, prices)
	if err != nil {
		return err
	}
	pmIDs := make([]uuid.UUID, 0, len(charged))
	seenPM := map[uuid.UUID]bool{}
	for _, id := range charged {
		if !seenPM[id] {
			seenPM[id] = true
			pmIDs = append(pmIDs, id)
		}
	}
	pms := map[uuid.UUID]*models.PaymentMethod{}
	if len(pmIDs) > 0 {
		rows, err := q.ListPaymentMethodsByIDs(ctx, gen.ListPaymentMethodsByIDsParams{MerchantID: scope.UUID(), Ids: pmIDs})
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
		if id, ok := charged[s.ID]; ok {
			s.PaymentMethod = pms[id]
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
// webhook-apply read-modify-writes. Must run inside a transaction; UpdateAt is
// a full-row write, so the lock must be held from read to write.
func (r *SubscriptionRepo) GetByPSPSubscriptionIDForUpdate(ctx context.Context, rail, railSubscriptionID string) (*models.Subscription, error) {
	return r.getByPSPSubscriptionID(ctx, rail, railSubscriptionID, func(q *gen.Queries, p gen.GetSubscriptionByPSPSubIDParams) (gen.BillingSubscription, error) {
		return q.GetSubscriptionByPSPSubIDForUpdate(ctx, gen.GetSubscriptionByPSPSubIDForUpdateParams(p))
	})
}

// GetByPSPSubscriptionID finds a subscription by rail and rail_subscription_id.
func (r *SubscriptionRepo) GetByPSPSubscriptionID(ctx context.Context, rail, railSubscriptionID string) (*models.Subscription, error) {
	return r.getByPSPSubscriptionID(ctx, rail, railSubscriptionID, func(q *gen.Queries, p gen.GetSubscriptionByPSPSubIDParams) (gen.BillingSubscription, error) {
		return q.GetSubscriptionByPSPSubID(ctx, p)
	})
}

// getByPSPSubscriptionID matches the provider's id by its identity on the rail
// (billing.rail_subscription_key). The id is the provider's spelling: a row
// spelled otherwise (a CCBill id whose leading zero a legacy import dropped)
// is respelled to it, so outbound calls use the provider's own id.
func (r *SubscriptionRepo) getByPSPSubscriptionID(ctx context.Context, rail, railSubscriptionID string, get func(*gen.Queries, gen.GetSubscriptionByPSPSubIDParams) (gen.BillingSubscription, error)) (*models.Subscription, error) {
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
	q := r.db.Gen(ctx)
	p := gen.GetSubscriptionByPSPSubIDParams{MerchantID: merchantID.UUID(), PspID: pspID, Rail: rail, RailSubscriptionID: railSubscriptionID}
	row, err := get(q, p)
	if err != nil {
		return nil, err
	}
	if models.DerefStr(row.RailSubscriptionID) != railSubscriptionID {
		healed, err := q.RespellSubscription(ctx, gen.RespellSubscriptionParams{MerchantID: row.MerchantID, ID: row.ID, RailSubscriptionID: railSubscriptionID})
		switch {
		case err == nil:
			log.WithContext(ctx).WithFields(log.Fields{"subscription_id": row.ID, "rail": rail, "stored": models.DerefStr(row.RailSubscriptionID), "provider": railSubscriptionID}).
				Info("Respelled the provider subscription id to the provider's spelling")
			row = healed
		case db.IsNotFound(err):
			// A concurrent contact respelled it first.
			if row, err = get(q, p); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("respell subscription %s to the provider's %q: %w", row.ID, railSubscriptionID, err)
		}
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

// ListPage is one page of the merchant's subscriptions matching f in
// f.Sort's order, with their details: up to fetch rows after (afterAt,
// afterID).
func (r *SubscriptionRepo) ListPage(ctx context.Context, f SubscriptionFilters, fetch int32, afterAt *time.Time, afterID *uuid.UUID) ([]*models.Subscription, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	customer, status, rail, price, err := r.filterArgs(f)
	if err != nil {
		return nil, err
	}
	q := r.db.Gen(ctx)
	var rows []gen.BillingSubscription
	switch f.Sort {
	case billing.SubscriptionSortCurrentPeriodEndsAt:
		rows, err = q.ListSubscriptionsPageByPeriodEnd(ctx, gen.ListSubscriptionsPageByPeriodEndParams{
			MerchantID: mid.UUID(), CustomerID: customer, Status: status, PriceID: price, Rail: rail,
			CreatedAfter: f.CreatedAfter, CreatedBefore: f.CreatedBefore, CanceledAfter: f.CanceledAfter,
			CanceledBefore: f.CanceledBefore, ExpiresBefore: f.ExpiresBefore, Dunning: f.Dunning,
			AfterAt: afterAt, AfterID: afterID, RowLimit: fetch,
		})
	case billing.SubscriptionSortCanceledAt:
		rows, err = q.ListSubscriptionsPageByCanceledAt(ctx, gen.ListSubscriptionsPageByCanceledAtParams{
			MerchantID: mid.UUID(), CustomerID: customer, Status: status, PriceID: price, Rail: rail,
			CreatedAfter: f.CreatedAfter, CreatedBefore: f.CreatedBefore, CanceledAfter: f.CanceledAfter,
			CanceledBefore: f.CanceledBefore, ExpiresBefore: f.ExpiresBefore, Dunning: f.Dunning,
			AfterAt: afterAt, AfterID: afterID, RowLimit: fetch,
		})
	default:
		rows, err = q.ListSubscriptionsPage(ctx, gen.ListSubscriptionsPageParams{
			MerchantID: mid.UUID(), CustomerID: customer, Status: status, PriceID: price, Rail: rail,
			CreatedAfter: f.CreatedAfter, CreatedBefore: f.CreatedBefore, CanceledAfter: f.CanceledAfter,
			CanceledBefore: f.CanceledBefore, ExpiresBefore: f.ExpiresBefore, Dunning: f.Dunning,
			AfterAt: afterAt, AfterID: afterID, RowLimit: fetch,
		})
	}
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

func derefSubs(subs []*models.Subscription) []models.Subscription {
	out := make([]models.Subscription, 0, len(subs))
	for _, s := range subs {
		out = append(out, *s)
	}
	return out
}

// DueDunningBatch is one page of a merchant's dunning pass: each row can
// charge a card and terminate a subscription.
const DueDunningBatch = 500

// DueDunningCursor is the (due instant, id) of the last row a pass read.
type DueDunningCursor struct {
	DueAt time.Time
	ID    uuid.UUID
}

// DueDunningCursorOf is the cursor after sub: the instant it came due, as
// ListDueDunningSubscriptions orders it.
func DueDunningCursorOf(sub *models.Subscription) DueDunningCursor {
	due := sub.NextRetryAt
	switch {
	case sub.Status == models.StatusAwaitingMethod, sub.Status == models.StatusPastDue && sub.NextRetryAt == nil:
		due = sub.GraceEndsAt
	case sub.CollectionPolicy == models.CollectionPolicyEngine && sub.Status == models.StatusActive:
		due = sub.CurrentPeriodEndsAt
	}
	var at time.Time
	if due != nil {
		at = *due
	}
	return DueDunningCursor{DueAt: at, ID: sub.ID}
}

// ListDueDunningSubscriptions returns one page of the merchant's due dunning
// work after the cursor (nil: from the start), with Price and PaymentMethod
// attached. includeEngine adds engine memberships whose wait for a card ended;
// admitEngine adds engine renewals due for admission.
func (r *SubscriptionRepo) ListDueDunningSubscriptions(ctx context.Context, rails []string, now time.Time, includeEngine, admitEngine bool, after *DueDunningCursor) ([]models.Subscription, error) {
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}
	params := gen.ListDueDunningSubscriptionsParams{
		IncludeEngine: includeEngine,
		AdmitEngine:   admitEngine,
		MerchantID:    scopeMerchantID.UUID(),
		Rails:         rails,
		Now:           now,
		RowLimit:      DueDunningBatch,
	}
	if after != nil {
		params.AfterDueAt, params.AfterID = &after.DueAt, after.ID
	}
	rows, err := r.db.Gen(ctx).ListDueDunningSubscriptions(ctx, params)
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

// ListOverdueRebills returns a page, after the (period end, id) cursor, of
// NMI schedules whose period ended before cutoff with neither an attempt nor a
// recorded miss for that cycle, and no failed look waiting out its backoff at
// now.
func (r *SubscriptionRepo) ListOverdueRebills(ctx context.Context, cutoff, now time.Time, after DueDunningCursor) ([]*models.Subscription, error) {
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}
	rows, err := r.db.Gen(ctx).ListOverdueRebills(ctx, gen.ListOverdueRebillsParams{
		MerchantID: scopeMerchantID.UUID(), Cutoff: cutoff, Now: now,
		AfterDueAt: after.DueAt, AfterID: after.ID, RowLimit: DueDunningBatch,
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
		ID: p.ID, PriceID: p.PriceID, ProductID: p.ProductID, AccessDurationHoursSnapshot: p.AccessDurationHoursSnapshot, ProviderAccessDefaultSnapshot: p.ProviderAccessDefaultSnapshot, Status: p.Status,
		StartedAt: p.StartedAt, EndedAt: p.EndedAt, CurrentPeriodStartsAt: p.CurrentPeriodStartsAt, CurrentPeriodEndsAt: p.CurrentPeriodEndsAt,
		Rail: p.Rail, RailSubscriptionID: p.RailSubscriptionID, PaymentMethodID: p.PaymentMethodID,
		LastRetryAt: p.LastRetryAt, RetryAttempts: p.RetryAttempts, TransientRetries: p.TransientRetries, NextRetryAt: p.NextRetryAt,
		GraceEndsAt: p.GraceEndsAt, CancelFeedback: p.CancelFeedback, CancelType: p.CancelType, CanceledAt: p.CanceledAt,
		DeletionScheduledAt: p.DeletionScheduledAt, GatewayResponse: p.GatewayResponse, Quantity: p.Quantity,
		UpdatedAt: p.UpdatedAt, MerchantID: p.MerchantID, ExpectedRev: rev, ExpectedVersion: p.ExpectedVersion, HistoryAttribution: p.HistoryAttribution, DunningPolicy: p.DunningPolicy,
	}
}
