package paymentmethods

import (
	"context"
	"errors"
	"fmt"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/merchant"
)

type PaymentMethodRepo struct {
	db *db.DB
}

func NewPaymentMethodRepo(d *db.DB) *PaymentMethodRepo { return &PaymentMethodRepo{db: d} }

// ErrPaymentMethodNotFound is declared in payment_method.go (the service
// file); the repo and service sentinels were unified when this moved into the
// module (#688) — same message, same errors.Is matching.

func (r *PaymentMethodRepo) Create(ctx context.Context, m *models.PaymentMethod) error {
	if err := db.EnsureCustomerRow(ctx, r.db.Qx(ctx), uuid.Nil, m.CustomerID); err != nil {
		return err
	}
	meta, err := models.ToJSONB(m.Metadata)
	if err != nil {
		return err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	pspID := m.PspID
	if pspID == nil {
		pspID = db.ResolveRailMerchantAccountIDForStamp(ctx)
	}
	rows, err := r.db.Gen(ctx).CreatePaymentMethod(ctx, gen.CreatePaymentMethodParams{
		ID:                   m.ID,
		MerchantID:           tid.UUID(),
		CustomerID:           m.CustomerID,
		Rail:                 string(m.Rail),
		PspID:                pspID,
		RailCustomerRef:      m.RailCustomerRef,
		RailMethodRef:        m.RailMethodRef,
		RebillDriver:         m.RebillDriver, // "" -> DB default 'provider'
		InitialTransactionID: m.InitialTransactionID,
		LastFour:             m.LastFour,
		CardType:             m.CardType,
		ExpiryDate:           m.ExpiryDate,
		Metadata:             meta,
		CreatedAt:            m.CreatedAt,
		UpdatedAt:            m.UpdatedAt,
		Custodian:            m.Custodian, // "" -> DB default 'psp'
		VaultFingerprint:     m.VaultFingerprint,
		NetworkTokenID:       m.NetworkTokenID,
		NetworkTokenStatus:   m.NetworkTokenStatus,
		NetworkTokenPar:      m.NetworkTokenPAR,
		ChargeVia:            m.ChargeVia, // "" -> DB default 'pan_proxy'
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

// attachPaymentMethodSubscriptions loads the Subscriptions (+ their Product)
// relation for the supplied payment methods (bun-era
// Relation("Subscriptions").Relation("Subscriptions.Product")).
func (r *PaymentMethodRepo) attachPaymentMethodSubscriptions(ctx context.Context, methods []*models.PaymentMethod) error {
	if len(methods) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(methods))
	for _, m := range methods {
		ids = append(ids, m.ID)
	}
	q := r.db.Gen(ctx)
	subRows, err := q.ListSubscriptionsByPaymentMethodIDs(ctx, ids)
	if err != nil {
		return err
	}
	subs, err := models.SubscriptionsFromGen(subRows)
	if err != nil {
		return err
	}

	productIDs := make([]uuid.UUID, 0, len(subs))
	seen := map[uuid.UUID]bool{}
	for _, s := range subs {
		if !seen[s.ProductID] {
			seen[s.ProductID] = true
			productIDs = append(productIDs, s.ProductID)
		}
	}
	products := map[uuid.UUID]*models.Product{}
	if len(productIDs) > 0 {
		rows, err := q.ListProductsByIDs(ctx, productIDs)
		if err != nil {
			return err
		}
		for _, row := range rows {
			p, err := models.ProductFromGen(row)
			if err != nil {
				return err
			}
			products[p.ID] = p
		}
	}

	byPM := map[uuid.UUID][]*models.Subscription{}
	for _, s := range subs {
		s.Product = products[s.ProductID]
		if s.PaymentMethodID != nil {
			byPM[*s.PaymentMethodID] = append(byPM[*s.PaymentMethodID], s)
		}
	}
	for _, m := range methods {
		m.Subscriptions = byPM[m.ID]
	}
	return nil
}

func (r *PaymentMethodRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.PaymentMethod, error) {
	row, err := r.db.Gen(ctx).GetPaymentMethodByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("payment method %s: %w", id, ErrPaymentMethodNotFound)
		}
		return nil, err
	}
	pm, err := models.PaymentMethodFromGen(row)
	if err != nil {
		return nil, err
	}
	if err := r.attachPaymentMethodSubscriptions(ctx, []*models.PaymentMethod{pm}); err != nil {
		return nil, err
	}
	return pm, nil
}

func (r *PaymentMethodRepo) Delete(ctx context.Context, id uuid.UUID) error {
	rows, err := r.db.Gen(ctx).DeletePaymentMethod(ctx, id)
	if err != nil {
		return err
	}
	if rows < 1 {
		return ErrPaymentMethodNotFound
	}
	return nil
}

func (r *PaymentMethodRepo) GetByUserID(ctx context.Context, userID string) ([]*models.PaymentMethod, error) {
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListPaymentMethodsByCustomer(ctx, tsid)
	if err != nil {
		return nil, err
	}
	return models.PaymentMethodsFromGen(rows)
}

func (r *PaymentMethodRepo) ListByUserID(ctx context.Context, userID string, limit, offset int) ([]*models.PaymentMethod, int64, error) {
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, 0, err
	}
	q := r.db.Gen(ctx)
	total, err := q.CountPaymentMethodsByCustomer(ctx, tsid)
	if err != nil {
		return nil, 0, err
	}
	limit32, _ := safecast.Convert[int32](limit)
	offset32, _ := safecast.Convert[int32](offset)
	rows, err := q.ListPaymentMethodsByCustomerPaged(ctx, gen.ListPaymentMethodsByCustomerPagedParams{
		CustomerID: tsid,
		PageLimit:  limit32,
		PageOffset: offset32,
	})
	if err != nil {
		return nil, 0, err
	}
	methods, err := models.PaymentMethodsFromGen(rows)
	if err != nil {
		return nil, 0, err
	}
	if err := r.attachPaymentMethodSubscriptions(ctx, methods); err != nil {
		return nil, 0, err
	}
	return methods, total, nil
}

// CountSharingCustomerRef reports how many OTHER payment methods share this
// rail customer-scope handle (#682 shared-vault guard — e.g. an imported
// multi-card NMI vault whose sibling cards a whole-vault delete would destroy).
func (r *PaymentMethodRepo) CountSharingCustomerRef(ctx context.Context, rail, customerRef string, excludeID uuid.UUID) (int64, error) {
	return r.db.Gen(ctx).CountPaymentMethodsSharingCustomerRef(ctx, gen.CountPaymentMethodsSharingCustomerRefParams{
		Rail:            rail,
		RailCustomerRef: customerRef,
		ExcludeID:       excludeID,
	})
}

func (r *PaymentMethodRepo) GetByRailMethodRef(ctx context.Context, rail, methodRef string) (*models.PaymentMethod, error) {
	row, err := r.db.Gen(ctx).GetPaymentMethodByRailMethodRef(ctx, gen.GetPaymentMethodByRailMethodRefParams{
		Rail:          rail,
		RailMethodRef: methodRef,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPaymentMethodNotFound
		}
		return nil, err
	}
	return models.PaymentMethodFromGen(row)
}

func (r *PaymentMethodRepo) Update(ctx context.Context, method *models.PaymentMethod) error {
	meta, err := models.ToJSONB(method.Metadata)
	if err != nil {
		return err
	}
	rows, err := r.db.Gen(ctx).UpdatePaymentMethod(ctx, gen.UpdatePaymentMethodParams{
		ID:                   method.ID,
		CustomerID:           method.CustomerID,
		Rail:                 string(method.Rail),
		RailCustomerRef:      method.RailCustomerRef,
		RailMethodRef:        method.RailMethodRef,
		InitialTransactionID: method.InitialTransactionID,
		LastFour:             method.LastFour,
		CardType:             method.CardType,
		ExpiryDate:           method.ExpiryDate,
		Metadata:             meta,
		UpdatedAt:            models.UpdateTimestamp(method.UpdatedAt),
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return ErrPaymentMethodNotFound
	}
	return nil
}

// GetAllNMIBacked returns all payment methods for NMI-backed rails
func (r *PaymentMethodRepo) GetAllNMIBacked(ctx context.Context) ([]*models.PaymentMethod, error) {
	rows, err := r.db.Gen(ctx).ListPaymentMethodsByRails(ctx, []string{string(models.RailNMI)})
	if err != nil {
		return nil, err
	}
	return models.PaymentMethodsFromGen(rows)
}

// GetNMIBackedByUserID returns all payment methods for NMI-backed rails for a user
func (r *PaymentMethodRepo) GetNMIBackedByUserID(ctx context.Context, userID string) ([]*models.PaymentMethod, error) {
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListPaymentMethodsByCustomerRails(ctx, gen.ListPaymentMethodsByCustomerRailsParams{
		CustomerID: tsid,
		Rails:      []string{string(models.RailNMI)},
	})
	if err != nil {
		return nil, err
	}
	return models.PaymentMethodsFromGen(rows)
}

func (r *PaymentMethodRepo) ExistsForUser(ctx context.Context, id uuid.UUID, userID string) (bool, error) {
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return false, err
	}
	count, err := r.db.Gen(ctx).CountPaymentMethodForUser(ctx, gen.CountPaymentMethodForUserParams{
		ID:         id,
		CustomerID: tsid,
	})
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (r *PaymentMethodRepo) WithTx(txdb *db.DB) *PaymentMethodRepo {
	return NewPaymentMethodRepo(txdb)
}

func (r *PaymentMethodRepo) GetByRail(ctx context.Context, rail models.Rail) ([]*models.PaymentMethod, error) {
	rows, err := r.db.Gen(ctx).ListPaymentMethodsByRail(ctx, string(rail))
	if err != nil {
		return nil, err
	}
	return models.PaymentMethodsFromGen(rows)
}

func (r *PaymentMethodRepo) RequireByID(ctx context.Context, id uuid.UUID) (*models.PaymentMethod, error) {
	pm, err := r.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return pm, nil
}

// LatestChargeByMethodIDs returns the most recent charge (time + raw status) per
// payment-method id, derived via the subscription link (#589). Methods with no
// charge history are simply absent from the map.
func (r *PaymentMethodRepo) LatestChargeByMethodIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]models.PaymentMethodCharge, error) {
	out := make(map[uuid.UUID]models.PaymentMethodCharge, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.db.Gen(ctx).ListLatestChargeByPaymentMethodIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.PaymentMethodID == nil {
			continue
		}
		out[*row.PaymentMethodID] = models.PaymentMethodCharge{
			LastChargedAt: row.PurchasedAt,
			Status:        string(row.Status),
		}
	}
	return out, nil
}
