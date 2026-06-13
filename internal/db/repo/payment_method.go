package repo

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
	"github.com/open-rails/openrails/internal/modules/payments/processors"
	"github.com/open-rails/openrails/pkg/tenant"
)

type PaymentMethodRepo struct {
	db *db.DB
}

func NewPaymentMethodRepo(d *db.DB) *PaymentMethodRepo { return &PaymentMethodRepo{db: d} }

var (
	ErrPaymentMethodNotFound = errors.New("payment method not found")
)

func (r *PaymentMethodRepo) Create(ctx context.Context, m *models.PaymentMethod) error {
	if err := ensureTenantSubjectRow(ctx, r.db.Qx(ctx), uuid.Nil, m.TenantSubjectID); err != nil {
		return err
	}
	meta, err := toJSONB(m.Metadata)
	if err != nil {
		return err
	}
	tid, err := tenant.Require(ctx)
	if err != nil {
		return err
	}
	rows, err := r.db.Gen(ctx).CreatePaymentMethod(ctx, gen.CreatePaymentMethodParams{
		ID:                   m.ID,
		TenantID:             tid.UUID(),
		TenantSubjectID:      m.TenantSubjectID,
		Processor:            string(m.Processor),
		VaultID:              m.VaultID,
		BillingID:            m.BillingID,
		InitialTransactionID: m.InitialTransactionID,
		LastFour:             m.LastFour,
		CardType:             m.CardType,
		ExpiryDate:           m.ExpiryDate,
		FailureReason:        m.FailureReason,
		Metadata:             meta,
		CreatedAt:            m.CreatedAt,
		UpdatedAt:            m.UpdatedAt,
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
	subs, err := subscriptionsFromGen(subRows)
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
			p, err := productFromGen(row)
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
	pm, err := paymentMethodFromGen(row)
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
	tsid, err := ResolveTenantSubjectID(userID)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListPaymentMethodsByTenantSubject(ctx, tsid)
	if err != nil {
		return nil, err
	}
	return paymentMethodsFromGen(rows)
}

func (r *PaymentMethodRepo) ListByUserID(ctx context.Context, userID string, limit, offset int) ([]*models.PaymentMethod, int64, error) {
	tsid, err := ResolveTenantSubjectID(userID)
	if err != nil {
		return nil, 0, err
	}
	q := r.db.Gen(ctx)
	total, err := q.CountPaymentMethodsByTenantSubject(ctx, tsid)
	if err != nil {
		return nil, 0, err
	}
	limit32, _ := safecast.Convert[int32](limit)
	offset32, _ := safecast.Convert[int32](offset)
	rows, err := q.ListPaymentMethodsByTenantSubjectPaged(ctx, gen.ListPaymentMethodsByTenantSubjectPagedParams{
		TenantSubjectID: tsid,
		PageLimit:       limit32,
		PageOffset:      offset32,
	})
	if err != nil {
		return nil, 0, err
	}
	methods, err := paymentMethodsFromGen(rows)
	if err != nil {
		return nil, 0, err
	}
	if err := r.attachPaymentMethodSubscriptions(ctx, methods); err != nil {
		return nil, 0, err
	}
	return methods, total, nil
}

func (r *PaymentMethodRepo) GetByVaultID(ctx context.Context, processor, vaultID string) (*models.PaymentMethod, error) {
	row, err := r.db.Gen(ctx).GetPaymentMethodByVaultID(ctx, gen.GetPaymentMethodByVaultIDParams{
		Processor: processor,
		VaultID:   vaultID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPaymentMethodNotFound
		}
		return nil, err
	}
	return paymentMethodFromGen(row)
}

func (r *PaymentMethodRepo) GetByInitialTransactionID(ctx context.Context, processor, initialTransactionID string) (*models.PaymentMethod, error) {
	row, err := r.db.Gen(ctx).GetPaymentMethodByInitialTransactionID(ctx, gen.GetPaymentMethodByInitialTransactionIDParams{
		Processor:            processor,
		InitialTransactionID: initialTransactionID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPaymentMethodNotFound
		}
		return nil, err
	}
	return paymentMethodFromGen(row)
}

func (r *PaymentMethodRepo) Update(ctx context.Context, method *models.PaymentMethod) error {
	meta, err := toJSONB(method.Metadata)
	if err != nil {
		return err
	}
	rows, err := r.db.Gen(ctx).UpdatePaymentMethod(ctx, gen.UpdatePaymentMethodParams{
		ID:                   method.ID,
		TenantSubjectID:      method.TenantSubjectID,
		Processor:            string(method.Processor),
		VaultID:              method.VaultID,
		BillingID:            method.BillingID,
		InitialTransactionID: method.InitialTransactionID,
		LastFour:             method.LastFour,
		CardType:             method.CardType,
		ExpiryDate:           method.ExpiryDate,
		FailureReason:        method.FailureReason,
		Metadata:             meta,
		UpdatedAt:            updateTimestamp(method.UpdatedAt),
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return ErrPaymentMethodNotFound
	}
	return nil
}

// GetAllNMIBacked returns all payment methods for NMI-backed processors
func (r *PaymentMethodRepo) GetAllNMIBacked(ctx context.Context) ([]*models.PaymentMethod, error) {
	rows, err := r.db.Gen(ctx).ListPaymentMethodsByProcessors(ctx, processors.GetNMIBackedProcessorsList())
	if err != nil {
		return nil, err
	}
	return paymentMethodsFromGen(rows)
}

// GetNMIBackedByUserID returns all payment methods for NMI-backed processors for a user
func (r *PaymentMethodRepo) GetNMIBackedByUserID(ctx context.Context, userID string) ([]*models.PaymentMethod, error) {
	tsid, err := ResolveTenantSubjectID(userID)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListPaymentMethodsByTenantSubjectProcessors(ctx, gen.ListPaymentMethodsByTenantSubjectProcessorsParams{
		TenantSubjectID: tsid,
		Processors:      processors.GetNMIBackedProcessorsList(),
	})
	if err != nil {
		return nil, err
	}
	return paymentMethodsFromGen(rows)
}

func (r *PaymentMethodRepo) ExistsForUser(ctx context.Context, id uuid.UUID, userID string) (bool, error) {
	tsid, err := ResolveTenantSubjectID(userID)
	if err != nil {
		return false, err
	}
	count, err := r.db.Gen(ctx).CountPaymentMethodForUser(ctx, gen.CountPaymentMethodForUserParams{
		ID:              id,
		TenantSubjectID: tsid,
	})
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (r *PaymentMethodRepo) WithTx(txdb *db.DB) *PaymentMethodRepo {
	return NewPaymentMethodRepo(txdb)
}

func (r *PaymentMethodRepo) GetByProcessor(ctx context.Context, processor models.Processor) ([]*models.PaymentMethod, error) {
	rows, err := r.db.Gen(ctx).ListPaymentMethodsByProcessor(ctx, string(processor))
	if err != nil {
		return nil, err
	}
	return paymentMethodsFromGen(rows)
}

func (r *PaymentMethodRepo) RequireByID(ctx context.Context, id uuid.UUID) (*models.PaymentMethod, error) {
	pm, err := r.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return pm, nil
}
