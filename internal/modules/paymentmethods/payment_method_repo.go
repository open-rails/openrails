package paymentmethods

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/pagination"
)

type PaymentMethodRepo struct {
	db *db.DB
}

func NewPaymentMethodRepo(d *db.DB) *PaymentMethodRepo { return &PaymentMethodRepo{db: d} }

func (r *PaymentMethodRepo) Create(ctx context.Context, m *models.PaymentMethod) error {
	if (m.Custodian != "" && m.Custodian != models.CustodianPSP) || !strings.EqualFold(strings.TrimSpace(string(m.Rail)), "nmi") {
		return r.create(ctx, m)
	}
	return r.createNMI(ctx, m, nil)
}

// CreateStored saves an NMI card its storing verification approved, with the
// card_on_file mandate that verification established, in one transaction.
func (r *PaymentMethodRepo) CreateStored(ctx context.Context, m *models.PaymentMethod, storing charge.Mandate, at time.Time) error {
	return r.createNMI(ctx, m, func(ctx context.Context, q *gen.Queries, merchantID uuid.UUID) error {
		_, err := mandates.Create(ctx, q, mandates.Agreement{MerchantID: merchantID, CustomerID: m.CustomerID, PaymentMethodID: m.ID, PSPID: *m.PspID,
			Rail: string(m.Rail), Kind: charge.AgreementCardOnFile, Lineage: &storing, AcceptedAt: at})
		return err
	})
}

func (r *PaymentMethodRepo) createNMI(ctx context.Context, m *models.PaymentMethod, stored func(context.Context, *gen.Queries, uuid.UUID) error) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	psp, err := heldBy(ctx, m)
	if err != nil {
		return err
	}
	return r.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := r.db.NewWithPgxTx(tx)
		q := d.Gen(ctx)
		if err := db.EnsureCustomerRow(ctx, d.Qx(ctx), mid.UUID(), m.CustomerID); err != nil {
			return err
		}
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: m.CustomerID}); err != nil {
			return err
		}
		if err := LockNativeVault(ctx, q, mid.UUID(), psp, m.RailCustomerRef); err != nil {
			return err
		}
		if err := RequireNativeVaultAvailable(ctx, q, mid.UUID(), psp, m.RailCustomerRef, m.RailMethodRef); err != nil {
			return err
		}
		if err := NewPaymentMethodRepo(d).create(ctx, m); err != nil || stored == nil {
			return err
		}
		return stored(ctx, q, mid.UUID())
	})
}

// heldBy is the PSP holding a PSP-held card: the method's own, else the
// request's.
func heldBy(ctx context.Context, m *models.PaymentMethod) (uuid.UUID, error) {
	if m.PspID != nil && *m.PspID != uuid.Nil {
		return *m.PspID, nil
	}
	psp, err := db.RequirePSPID(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("create payment method %s/%s: %w", m.Rail, m.RailCustomerRef, err)
	}
	return psp, nil
}

func (r *PaymentMethodRepo) create(ctx context.Context, m *models.PaymentMethod) error {
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
	if m.Custodian == "" {
		m.Custodian = models.CustodianPSP
	}
	if m.Custodian == models.CustodianPSP {
		psp, err := heldBy(ctx, m)
		if err != nil {
			return err
		}
		m.PspID = &psp
	} else {
		m.PspID = nil
		if m.CustodianID == nil {
			if id := db.CustodianIDFromContext(ctx); id != uuid.Nil {
				m.CustodianID = &id
			}
		}
	}
	brand, last4, month, year := m.Card.Columns()
	rows, err := r.db.Gen(ctx).CreatePaymentMethod(ctx, gen.CreatePaymentMethodParams{
		ID:              m.ID,
		MerchantID:      tid.UUID(),
		CustomerID:      m.CustomerID,
		Rail:            string(m.Rail),
		PspID:           m.PspID,
		RailCustomerRef: m.RailCustomerRef,
		RailMethodRef:   m.RailMethodRef,

		CardBrand:          brand,
		CardLast4:          last4,
		CardExpMonth:       month,
		CardExpYear:        year,
		Metadata:           meta,
		CreatedAt:          m.CreatedAt,
		UpdatedAt:          m.UpdatedAt,
		CustodianID:        m.CustodianID,
		Custodian:          m.Custodian,
		Fingerprint:        m.Fingerprint,
		NetworkTokenID:     m.NetworkTokenID,
		NetworkTokenStatus: m.NetworkTokenStatus,
		NetworkTokenPar:    m.NetworkTokenPAR,
		ChargeVia:          m.ChargeVia, // "" -> 'pan_proxy'
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

// attachPaymentMethodSubscriptions loads the subscriptions, with their
// products, of the supplied payment methods.
func (r *PaymentMethodRepo) attachPaymentMethodSubscriptions(ctx context.Context, methods []*models.PaymentMethod) error {
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return queryScopeErr
	}

	if len(methods) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(methods))
	for _, m := range methods {
		ids = append(ids, m.ID)
	}
	q := r.db.Gen(ctx)
	paid, err := q.ListSubscriptionsPaidByMethods(ctx, gen.ListSubscriptionsPaidByMethodsParams{MerchantID: queryMerchant.UUID(), PaymentMethodIds: ids})
	if err != nil {
		return err
	}
	subs := make([]*models.Subscription, 0, len(paid))
	paidBy := make(map[uuid.UUID]uuid.UUID, len(paid))
	for _, row := range paid {
		sub, err := models.SubscriptionFromGen(row.BillingSubscription)
		if err != nil {
			return err
		}
		subs = append(subs, sub)
		paidBy[sub.ID] = row.PaidBy
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
		rows, err := q.ListProductsByIDs(ctx, gen.ListProductsByIDsParams{MerchantID: queryMerchant.UUID(), Ids: productIDs})
		if err != nil {
			return err
		}
		loaded, err := r.db.ProductsFromGen(ctx, rows)
		if err != nil {
			return err
		}
		for _, p := range loaded {
			products[p.ID] = p
		}
	}

	byPM := map[uuid.UUID][]*models.Subscription{}
	for _, s := range subs {
		s.Product = products[s.ProductID]
		byPM[paidBy[s.ID]] = append(byPM[paidBy[s.ID]], s)
	}
	for _, m := range methods {
		m.Subscriptions = byPM[m.ID]
	}
	return nil
}

func (r *PaymentMethodRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.PaymentMethod, error) {
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}

	row, err := r.db.Gen(ctx).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: queryMerchant.UUID(), ID: id})
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
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return queryScopeErr
	}

	// The card's agreements end with it; the mandates stay as evidence.
	return r.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := mandates.EndForPaymentMethod(ctx, q, queryMerchant.UUID(), id, mandates.EndPaymentMethodRemoved, time.Time{}); err != nil {
			return err
		}
		rows, err := q.DeletePaymentMethod(ctx, gen.DeletePaymentMethodParams{MerchantID: queryMerchant.UUID(), ID: id})
		if err != nil {
			return err
		}
		if rows < 1 {
			return ErrPaymentMethodNotFound
		}
		return nil
	})
}

func (r *PaymentMethodRepo) GetByUserID(ctx context.Context, userID string) ([]*models.PaymentMethod, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListPaymentMethodsByCustomer(ctx, gen.ListPaymentMethodsByCustomerParams{MerchantID: tid.UUID(), CustomerID: tsid})
	if err != nil {
		return nil, err
	}
	return models.PaymentMethodsFromGen(rows)
}

// ListPage is one page of a customer's methods, newest first.
func (r *PaymentMethodRepo) ListPage(ctx context.Context, customerID uuid.UUID, page billing.PageRequest) (billing.ListPage[*models.PaymentMethod], error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return billing.ListPage[*models.PaymentMethod]{}, err
	}
	limit, err := pagination.Limit(page)
	if err != nil {
		return billing.ListPage[*models.PaymentMethod]{}, err
	}
	afterAt, afterID, err := pagination.After(page.Cursor)
	if err != nil {
		return billing.ListPage[*models.PaymentMethod]{}, err
	}
	rows, err := r.db.Gen(ctx).ListPaymentMethodsByCustomerPage(ctx, gen.ListPaymentMethodsByCustomerPageParams{
		MerchantID: mid.UUID(), CustomerID: customerID, AfterAt: afterAt, AfterID: afterID, RowLimit: pagination.Fetch(limit),
	})
	if err != nil {
		return billing.ListPage[*models.PaymentMethod]{}, err
	}
	methods, err := models.PaymentMethodsFromGen(rows)
	if err != nil {
		return billing.ListPage[*models.PaymentMethod]{}, err
	}
	out := pagination.Cut(methods, limit, func(m *models.PaymentMethod) any { return pagination.TimeID{At: m.CreatedAt, ID: m.ID} })
	if err := r.attachPaymentMethodSubscriptions(ctx, out.Items); err != nil {
		return billing.ListPage[*models.PaymentMethod]{}, err
	}
	return out, nil
}

// ListByIDs reads a customer's named methods, newest first.
func (r *PaymentMethodRepo) ListByIDs(ctx context.Context, customerID uuid.UUID, ids []uuid.UUID) ([]*models.PaymentMethod, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListCustomerPaymentMethodsByIDs(ctx, gen.ListCustomerPaymentMethodsByIDsParams{MerchantID: mid.UUID(), CustomerID: customerID, Ids: ids})
	if err != nil {
		return nil, err
	}
	methods, err := models.PaymentMethodsFromGen(rows)
	if err != nil {
		return nil, err
	}
	return methods, r.attachPaymentMethodSubscriptions(ctx, methods)
}

// CountSharingCustomerRef reports how many other payment methods share this
// customer-scope handle, e.g. the sibling cards of an imported multi-card NMI
// vault that a whole-vault delete would destroy.
func (r *PaymentMethodRepo) CountSharingCustomerRef(ctx context.Context, rail string, pspID uuid.UUID, customerRef string, excludeID uuid.UUID) (int64, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	if pspID == uuid.Nil {
		return 0, db.ErrNoPSPInContext
	}
	return r.db.Gen(ctx).CountPaymentMethodsSharingCustomerRef(ctx, gen.CountPaymentMethodsSharingCustomerRefParams{MerchantID: mid.UUID(), PspID: pspID,
		Rail:            rail,
		RailCustomerRef: customerRef,
		ExcludeID:       excludeID,
	})
}

func (r *PaymentMethodRepo) GetByPSPMethodRef(ctx context.Context, rail, methodRef string) (*models.PaymentMethod, error) {
	pspID, err := db.RequirePSPID(ctx)
	if err != nil {
		return nil, err
	}
	return r.GetByRailMethodRefForPSP(ctx, rail, pspID, methodRef)
}

func (r *PaymentMethodRepo) GetByRailMethodRefForPSP(ctx context.Context, rail string, pspID uuid.UUID, methodRef string) (*models.PaymentMethod, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if pspID == uuid.Nil {
		return nil, db.ErrNoPSPInContext
	}
	var cid *uuid.UUID
	if id := db.CustodianIDFromContext(ctx); id != uuid.Nil {
		cid = &id
	}
	row, err := r.db.Gen(ctx).GetPaymentMethodByRailMethodRefForPSP(ctx, gen.GetPaymentMethodByRailMethodRefForPSPParams{CustodianID: cid, MerchantID: mid.UUID(),
		Rail:          rail,
		PspID:         pspID,
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

// GetByCustodianRef finds a customer's custodian-held card by its custodian
// token.
func (r *PaymentMethodRepo) GetByCustodianRef(ctx context.Context, custodianID, customerID uuid.UUID, methodRef string) (*models.PaymentMethod, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).GetPaymentMethodByCustodianRef(ctx, gen.GetPaymentMethodByCustodianRefParams{MerchantID: mid.UUID(), CustomerID: customerID, CustodianID: custodianID, RailMethodRef: methodRef})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPaymentMethodNotFound
	}
	if err != nil {
		return nil, err
	}
	return models.PaymentMethodFromGen(row)
}

func (r *PaymentMethodRepo) Update(ctx context.Context, method *models.PaymentMethod) error {
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return queryScopeErr
	}

	meta, err := models.ToJSONB(method.Metadata)
	if err != nil {
		return err
	}
	brand, last4, month, year := method.Card.Columns()
	rows, err := r.db.Gen(ctx).UpdatePaymentMethod(ctx, gen.UpdatePaymentMethodParams{MerchantID: queryMerchant.UUID(),
		ID:              method.ID,
		CustomerID:      method.CustomerID,
		Rail:            string(method.Rail),
		RailCustomerRef: method.RailCustomerRef,
		RailMethodRef:   method.RailMethodRef,
		CardBrand:       brand,
		CardLast4:       last4,
		CardExpMonth:    month,
		CardExpYear:     year,
		Metadata:        meta,
		UpdatedAt:       models.UpdateTimestamp(method.UpdatedAt),
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return ErrPaymentMethodNotFound
	}
	return nil
}

// GetAllNMIBacked returns the merchant's NMI payment methods.
func (r *PaymentMethodRepo) GetAllNMIBacked(ctx context.Context) ([]*models.PaymentMethod, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListPaymentMethodsByRails(ctx, gen.ListPaymentMethodsByRailsParams{MerchantID: mid.UUID(), Rails: []string{string(models.RailNMI)}})
	if err != nil {
		return nil, err
	}
	return models.PaymentMethodsFromGen(rows)
}

// GetNMIBackedByUserID returns a customer's NMI payment methods.
func (r *PaymentMethodRepo) GetNMIBackedByUserID(ctx context.Context, userID string) ([]*models.PaymentMethod, error) {
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}

	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListPaymentMethodsByCustomerRails(ctx, gen.ListPaymentMethodsByCustomerRailsParams{MerchantID: queryMerchant.UUID(),
		CustomerID: tsid,
		Rails:      []string{string(models.RailNMI)},
	})
	if err != nil {
		return nil, err
	}
	return models.PaymentMethodsFromGen(rows)
}

func (r *PaymentMethodRepo) ExistsForUser(ctx context.Context, id uuid.UUID, userID string) (bool, error) {
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return false, queryScopeErr
	}

	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return false, err
	}
	count, err := r.db.Gen(ctx).CountPaymentMethodForUser(ctx, gen.CountPaymentMethodForUserParams{MerchantID: queryMerchant.UUID(),
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
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}

	rows, err := r.db.Gen(ctx).ListPaymentMethodsByRail(ctx, gen.ListPaymentMethodsByRailParams{MerchantID: queryMerchant.UUID(), Rail: string(rail)})
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

// LatestChargeByMethodIDs returns the most recent charge (time and raw status)
// per payment method id, derived via the subscription link. Methods with no
// charge history are absent.
func (r *PaymentMethodRepo) LatestChargeByMethodIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]models.PaymentMethodCharge, error) {
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}

	out := make(map[uuid.UUID]models.PaymentMethodCharge, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.db.Gen(ctx).ListLatestChargeByPaymentMethodIDs(ctx, gen.ListLatestChargeByPaymentMethodIDsParams{MerchantID: queryMerchant.UUID(), Ids: ids})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.PaymentMethodID == nil {
			continue
		}
		status := "failed"
		if row.Category == "approved" {
			status = "succeeded"
		}
		out[*row.PaymentMethodID] = models.PaymentMethodCharge{LastChargedAt: row.AttemptedAt, Status: status}
	}
	return out, nil
}
