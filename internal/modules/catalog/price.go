package catalog

import (
	"context"
	"errors"
	"fmt"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/normalize"
)

type PriceService struct {
	db *db.DB
}

func NewPriceService(db *db.DB) *PriceService {
	return &PriceService{db: db}
}

func (s *PriceService) Create(ctx context.Context, price *models.Price) error {
	if _, _, err := queryCatalogScope(ctx); err != nil {
		return err
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		scoped := NewPriceService(s.db.NewWithPgxTx(tx))
		if err := scoped.createRow(ctx, price); err != nil {
			return err
		}
		return scoped.UpdatePSPLinks(ctx, price.ID, price.PSPLinks)
	})
}

func (s *PriceService) createRow(ctx context.Context, price *models.Price) error {
	mid, catalogID, err := queryCatalogScope(ctx)
	if err != nil {
		return err
	}
	if price.MerchantID != uuid.Nil && price.MerchantID != mid.UUID() {
		return fmt.Errorf("price merchant does not match the authorized merchant")
	}
	price.MerchantID = mid.UUID()
	// CUR-6: the single price-INSERT chokepoint, so every price row is
	// canonical whatever minted it (service API, catalog manifest apply,
	// importer).
	price.Currency = moneyutil.NormalizeCurrency(price.Currency)
	rows, err := s.db.Gen(ctx).CreatePrice(ctx, gen.CreatePriceParams{
		ID:                  price.ID,
		MerchantID:          price.MerchantID,
		CatalogID:           catalogID,
		ProductID:           price.ProductID,
		Archived:            price.Archived,
		Amount:              price.Amount,
		Currency:            price.Currency,
		AccessDurationHours: models.IntPtrTo32(price.AccessDurationHours),
		AutoRenew:           price.AutoRenew,
		TrialUnitAmount:     price.TrialUnitAmount,
		TrialDurationHours:  models.IntPtrTo32(price.TrialDurationHours),
		Key:                 price.Key,
		CreatedAt:           price.CreatedAt,
		UpdatedAt:           price.UpdatedAt,
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return pgx.ErrNoRows
	}
	return nil
}

func (s *PriceService) GetByID(ctx context.Context, id uuid.UUID) (*models.Price, error) {
	queryMerchant, catalogID, queryScopeErr := queryCatalogScope(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}

	row, err := s.db.Gen(ctx).GetPriceByID(ctx, gen.GetPriceByIDParams{MerchantID: queryMerchant.UUID(), CatalogID: catalogID, ID: id})
	if err != nil {
		return nil, err
	}
	return s.db.PriceFromGen(ctx, row)
}

func (s *PriceService) pricesFromGen(ctx context.Context, rows []gen.OpenrailsPrice) ([]*models.Price, error) {
	out := make([]*models.Price, 0, len(rows))
	for _, r := range rows {
		p, err := models.PriceFromGen(r)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, s.db.LoadPricePSPBindings(ctx, out, nil)
}

func (s *PriceService) GetByProductID(ctx context.Context, productID uuid.UUID) ([]*models.Price, error) {
	queryMerchant, catalogID, queryScopeErr := queryCatalogScope(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}

	// Archived included. The catalog converge relies on this to reconcile
	// already-archived historical prices instead of re-creating them;
	// GetActiveByProductID is the non-archived variant.
	rows, err := s.db.Gen(ctx).ListPricesByProduct(ctx, gen.ListPricesByProductParams{MerchantID: queryMerchant.UUID(), CatalogID: catalogID, ProductID: productID})
	if err != nil {
		return nil, err
	}
	return s.pricesFromGen(ctx, rows)
}

func (s *PriceService) GetActiveByProductID(ctx context.Context, productID uuid.UUID) ([]*models.Price, error) {
	queryMerchant, catalogID, queryScopeErr := queryCatalogScope(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}

	rows, err := s.db.Gen(ctx).ListActivePricesByProductOrdered(ctx, gen.ListActivePricesByProductOrderedParams{MerchantID: queryMerchant.UUID(), CatalogID: catalogID, ProductID: productID})
	if err != nil {
		return nil, err
	}
	return s.pricesFromGen(ctx, rows)
}

func (s *PriceService) GetAllActive(ctx context.Context) ([]*models.Price, error) {
	queryMerchant, catalogID, queryScopeErr := queryCatalogScope(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}

	rows, err := s.db.Gen(ctx).ListAllActivePricesWithProduct(ctx, gen.ListAllActivePricesWithProductParams{MerchantID: queryMerchant.UUID(), CatalogID: catalogID})
	if err != nil {
		return nil, err
	}
	out := make([]*models.Price, 0, len(rows))
	for _, row := range rows {
		price, err := s.priceWithProduct(ctx, row.OpenrailsPrice, row.OpenrailsProduct)
		if err != nil {
			return nil, err
		}
		out = append(out, price)
	}
	return out, s.db.LoadPricePSPBindings(ctx, out, nil)
}

func (s *PriceService) GetAll(ctx context.Context) ([]*models.Price, error) {
	queryMerchant, catalogID, queryScopeErr := queryCatalogScope(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}

	rows, err := s.db.Gen(ctx).ListAllPricesWithProduct(ctx, gen.ListAllPricesWithProductParams{MerchantID: queryMerchant.UUID(), CatalogID: catalogID})
	if err != nil {
		return nil, err
	}
	out := make([]*models.Price, 0, len(rows))
	for _, row := range rows {
		price, err := s.priceWithProduct(ctx, row.OpenrailsPrice, row.OpenrailsProduct)
		if err != nil {
			return nil, err
		}
		out = append(out, price)
	}
	return out, s.db.LoadPricePSPBindings(ctx, out, nil)
}

func (s *PriceService) priceWithProduct(ctx context.Context, p gen.OpenrailsPrice, prod gen.OpenrailsProduct) (*models.Price, error) {
	price, err := models.PriceFromGen(p)
	if err != nil {
		return nil, err
	}
	product, err := models.ProductFromGen(prod)
	if err != nil {
		return nil, err
	}
	price.Product = product
	return price, nil
}

// PriceFilter contains optional filters for listing prices
type PriceFilter struct {
	CatalogID *uuid.UUID // Optional administrator selector; owner scope always wins.
	Archived  *bool      // Filter by archived flag (nil = all)
	Currency  string     // Filter by currency (e.g., "usd")
	ProductID *uuid.UUID // Filter by product ID
	Type      string     // Filter by "recurring" or "one_time"
}

// ListPaginated returns prices with pagination and optional filters
func (s *PriceService) ListPaginated(ctx context.Context, filter PriceFilter, limit, offset int) ([]*models.Price, int64, error) {
	queryMerchant, _, queryScopeErr := queryCatalogScope(ctx)
	if queryScopeErr != nil {
		return nil, 0, queryScopeErr
	}
	catalogID, err := selectCatalogFilter(ctx, s.db, filter.CatalogID)
	if err != nil {
		return nil, 0, err
	}

	var currency *string
	if filter.Currency != "" {
		currency = &filter.Currency
	}
	onlyRecurring := filter.Type == "recurring"
	onlyOneTime := filter.Type == "one_time"

	q := s.db.Gen(ctx)
	total, err := q.CountPricesFiltered(ctx, gen.CountPricesFilteredParams{MerchantID: queryMerchant.UUID(), CatalogID: catalogID,
		Archived:      filter.Archived,
		Currency:      currency,
		ProductID:     filter.ProductID,
		OnlyRecurring: onlyRecurring,
		OnlyOneTime:   onlyOneTime,
	})
	if err != nil {
		return nil, 0, err
	}
	limit32, _ := safecast.Convert[int32](limit)
	offset32, _ := safecast.Convert[int32](offset)
	rows, err := q.ListPricesFiltered(ctx, gen.ListPricesFilteredParams{MerchantID: queryMerchant.UUID(), CatalogID: catalogID,
		Archived:      filter.Archived,
		Currency:      currency,
		ProductID:     filter.ProductID,
		OnlyRecurring: onlyRecurring,
		OnlyOneTime:   onlyOneTime,
		PageLimit:     limit32,
		PageOffset:    offset32,
	})
	if err != nil {
		return nil, 0, err
	}
	out := make([]*models.Price, 0, len(rows))
	for _, row := range rows {
		price, err := s.priceWithProduct(ctx, row.OpenrailsPrice, row.OpenrailsProduct)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, price)
	}
	return out, total, s.db.LoadPricePSPBindings(ctx, out, nil)
}

func (s *PriceService) GetByNMIPlan(ctx context.Context, rail, nmiPlanID string) (*models.Price, error) {
	mid, catalogID, err := queryCatalogScope(ctx)
	if err != nil {
		return nil, err
	}
	pspID, err := db.RequirePSPID(ctx)
	if err != nil {
		return nil, err
	}
	rail = normalize.Lower(rail)
	if rail == "" {
		return nil, fmt.Errorf("nmi rail is required for plan %q", normalize.Trim(nmiPlanID))
	}
	// Archived prices must still resolve here so grandfathered subscriptions
	// keep billing.
	row, err := s.db.Gen(ctx).GetPriceByNMIPlan(ctx, gen.GetPriceByNMIPlanParams{
		MerchantID: mid.UUID(), CatalogID: catalogID, PspID: pspID,
		Rail:   rail,
		PlanID: nmiPlanID,
	})
	if err != nil {
		return nil, err
	}
	price, err := s.db.PriceFromGen(ctx, row)
	return price.ForPSP(pspID), err
}

func (s *PriceService) GetByCCBillPriceID(ctx context.Context, recurringBillingOptionID, flexID string) (*models.Price, error) {
	objectKind, ccbillPriceID := "recurring_billing_option", normalize.Trim(recurringBillingOptionID)
	if ccbillPriceID == "" {
		objectKind, ccbillPriceID = "flex", normalize.Trim(flexID)
	}
	mid, catalogID, err := queryCatalogScope(ctx)
	if err != nil {
		return nil, err
	}
	pspID, err := db.RequirePSPID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Gen(ctx).GetPriceWithProductByCCBillPriceID(ctx, gen.GetPriceWithProductByCCBillPriceIDParams{ObjectKind: objectKind, MerchantID: mid.UUID(), CatalogID: catalogID, PspID: pspID, CcbillPriceID: ccbillPriceID})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, pgx.ErrNoRows
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("ambiguous CCBill %s %q for PSP %s; supply recurring billing option identity", objectKind, ccbillPriceID, pspID)
	}
	row := rows[0]
	price, err := s.priceWithProduct(ctx, row.OpenrailsPrice, row.OpenrailsProduct)
	if err != nil {
		return nil, err
	}
	if err = s.db.LoadPricePSPBindings(ctx, []*models.Price{price}, &pspID); err != nil {
		return nil, err
	}
	return price, nil
}

func (s *PriceService) GetByStripePriceID(ctx context.Context, stripePriceID string) (*models.Price, error) {
	mid, catalogID, err := queryCatalogScope(ctx)
	if err != nil {
		return nil, err
	}
	pspID, err := db.RequirePSPID(ctx)
	if err != nil {
		return nil, err
	}
	row, err := s.db.Gen(ctx).GetPriceWithProductByStripePriceID(ctx, gen.GetPriceWithProductByStripePriceIDParams{MerchantID: mid.UUID(), CatalogID: catalogID, PspID: pspID, StripePriceID: stripePriceID})
	if err != nil {
		return nil, err
	}
	price, err := s.priceWithProduct(ctx, row.OpenrailsPrice, row.OpenrailsProduct)
	if err != nil {
		return nil, err
	}
	if err = s.db.LoadPricePSPBindings(ctx, []*models.Price{price}, &pspID); err != nil {
		return nil, err
	}
	return price, nil
}

// Update is not supported - prices are immutable to preserve historical payment accuracy.
// To change pricing, create a new price and deactivate the old one.
// Use UpdatePSPLinks() for non-financial fields.
func (s *PriceService) Update(ctx context.Context, price *models.Price) error {
	return errors.New("prices are immutable; use UpdatePSPLinks() or Deactivate() for allowed changes")
}

// Delete is not supported - prices are immutable to preserve historical payment accuracy.
// To retire a price, archive it via Deactivate() (sets archived).
func (s *PriceService) Delete(ctx context.Context, id uuid.UUID) error {
	return errors.New("prices cannot be deleted; use Deactivate() instead to preserve historical data")
}

// #662: there is no full-row price update. A price's money/identity columns are
// immutable (a reprice creates a new row and archives the old); the only allowed
// mutations are archived (SetArchived) and rails (UpdateRails), each writing
// exactly its own column via a narrow query, so the immutable columns cannot be
// SET at the DB layer.

// Deactivate archives a price so it won't appear in product listings and
// cannot be purchased by new customers. Existing subscriptions and payments
// referencing this price are grandfathered and keep billing.
func (s *PriceService) Deactivate(ctx context.Context, id uuid.UUID) error {
	return s.SetArchived(ctx, id, true)
}

// Activate un-archives a price so it appears in product listings.
func (s *PriceService) Activate(ctx context.Context, id uuid.UUID) error {
	return s.SetArchived(ctx, id, false)
}

// SetArchived sets the archived lifecycle flag on a price.
func (s *PriceService) SetArchived(ctx context.Context, id uuid.UUID, archived bool) error {
	queryMerchant, catalogID, queryScopeErr := queryCatalogScope(ctx)
	if queryScopeErr != nil {
		return queryScopeErr
	}

	rows, err := s.db.Gen(ctx).UpdatePriceStatus(ctx, gen.UpdatePriceStatusParams{MerchantID: queryMerchant.UUID(), CatalogID: catalogID,
		ID:       id,
		Archived: archived,
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return pgx.ErrNoRows
	}
	return nil
}

// #774: price keys — a durable, per-merchant-unique handle that is a MOVABLE
// POINTER to the current row of a substance-version chain. Row identity stays
// the #662 substance UUID; these methods manage the key label + the
// pointer-movement history log, never the immutable financial columns.

// SetKey relabels a price row's key in place. This is a pure label mutation
// (like UpdateRails) — it never changes row identity. Used both when a
// version bump repoints a NEW/reactivated row to a key, and when MODE 1's
// YAML-is-truth converge detects a plain key rename on an otherwise-unchanged
// price (the same substance, matched by matchPrice, now declared under a
// different key string).
func (s *PriceService) SetKey(ctx context.Context, id uuid.UUID, key string) error {
	queryMerchant, catalogID, queryScopeErr := queryCatalogScope(ctx)
	if queryScopeErr != nil {
		return queryScopeErr
	}

	rows, err := s.db.Gen(ctx).UpdatePriceKey(ctx, gen.UpdatePriceKeyParams{MerchantID: queryMerchant.UUID(), CatalogID: catalogID,
		ID:  id,
		Key: key,
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return pgx.ErrNoRows
	}
	return nil
}

// GetCurrentByKey returns the CURRENT (non-archived) row for a key, or
// pgx.ErrNoRows if the key names no live price. At most one such row can
// exist per (merchant, key) — enforced by uq_prices_merchant_key_current.
func (s *PriceService) GetCurrentByKey(ctx context.Context, merchantID uuid.UUID, key string) (*models.Price, error) {
	catalogID, err := queryCatalogMerchant(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	row, err := s.db.Gen(ctx).GetCurrentPriceByKey(ctx, gen.GetCurrentPriceByKeyParams{
		MerchantID: merchantID,
		CatalogID:  catalogID,
		Key:        key,
	})
	if err != nil {
		return nil, err
	}
	return s.db.PriceFromGen(ctx, row)
}

// ListChainByKey returns every row (archived + current) that has ever been
// named by this key — the version chain.
func (s *PriceService) ListChainByKey(ctx context.Context, merchantID uuid.UUID, key string) ([]*models.Price, error) {
	catalogID, err := queryCatalogMerchant(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Gen(ctx).ListPriceChainByKey(ctx, gen.ListPriceChainByKeyParams{
		MerchantID: merchantID,
		CatalogID:  catalogID,
		Key:        key,
	})
	if err != nil {
		return nil, err
	}
	return s.pricesFromGen(ctx, rows)
}

// ListPriorVersionsByKey returns the archived members of a key's chain —
// #773's "all prior versions of key K", the reprice_all_prior_versions bulk
// target set.
func (s *PriceService) ListPriorVersionsByKey(ctx context.Context, merchantID uuid.UUID, key string) ([]*models.Price, error) {
	catalogID, err := queryCatalogMerchant(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Gen(ctx).ListPriorVersionsByKey(ctx, gen.ListPriorVersionsByKeyParams{
		MerchantID: merchantID,
		CatalogID:  catalogID,
		Key:        key,
	})
	if err != nil {
		return nil, err
	}
	return s.pricesFromGen(ctx, rows)
}

// RecordKeyMovement appends one entry to the pointer-movement history log:
// key's current pointer moved to priceID at effectiveAt. Append-only — call
// exactly once per genuine movement (new row, reactivation, or repoint), never
// on a true no-op (same key, same already-current substance).
func (s *PriceService) RecordKeyMovement(ctx context.Context, merchantID, priceID uuid.UUID, key string, effectiveAt time.Time) error {
	catalogID, err := queryCatalogMerchant(ctx, merchantID)
	if err != nil {
		return err
	}
	rows, err := s.db.Gen(ctx).InsertPriceKeyMovement(ctx, gen.InsertPriceKeyMovementParams{
		MerchantID:  merchantID,
		CatalogID:   catalogID,
		Key:         key,
		PriceID:     priceID,
		EffectiveAt: effectiveAt,
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return pgx.ErrNoRows
	}
	return nil
}

// ListKeyMovements returns the full movement history for a key, most-recent
// first.
func (s *PriceService) ListKeyMovements(ctx context.Context, merchantID uuid.UUID, key string) ([]*models.PriceKeyMovement, error) {
	catalogID, err := queryCatalogMerchant(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Gen(ctx).ListPriceKeyMovements(ctx, gen.ListPriceKeyMovementsParams{
		MerchantID: merchantID,
		CatalogID:  catalogID,
		Key:        key,
	})
	if err != nil {
		return nil, err
	}
	return models.PriceKeyMovementsFromGen(rows), nil
}
