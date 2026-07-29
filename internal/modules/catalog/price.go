package catalog

import (
	"context"
	"errors"
	"fmt"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
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

func pricePSPLinksJSONB(p *models.Price) ([]byte, error) {
	return models.ToJSONB(p.PSPLinks)
}

func (s *PriceService) Create(ctx context.Context, price *models.Price) error {
	pspLinks, err := pricePSPLinksJSONB(price)
	if err != nil {
		return err
	}
	// CUR-6: the single price-INSERT chokepoint, so every price row is
	// canonical whatever minted it (service API, catalog manifest apply,
	// importer).
	price.Currency = moneyutil.NormalizeCurrency(price.Currency)
	rows, err := s.db.Gen(ctx).CreatePrice(ctx, gen.CreatePriceParams{
		ID:                  price.ID,
		MerchantID:          price.MerchantID,
		ProductID:           price.ProductID,
		Archived:            price.Archived,
		Amount:              price.Amount,
		Currency:            price.Currency,
		AccessDurationHours: models.IntPtrTo32(price.AccessDurationHours),
		AutoRenew:           price.AutoRenew,
		TrialUnitAmount:     price.TrialUnitAmount,
		TrialDurationHours:  models.IntPtrTo32(price.TrialDurationHours),
		PspLinks:            pspLinks,
		Key:                 price.Key,
		CreatedAt:           price.CreatedAt,
		UpdatedAt:           price.UpdatedAt,
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (s *PriceService) GetByID(ctx context.Context, id uuid.UUID) (*models.Price, error) {
	row, err := s.db.Gen(ctx).GetPriceByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return models.PriceFromGen(row)
}

func pricesFromGen(rows []gen.OpenrailsPrice) ([]*models.Price, error) {
	out := make([]*models.Price, 0, len(rows))
	for _, r := range rows {
		p, err := models.PriceFromGen(r)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func (s *PriceService) GetByProductID(ctx context.Context, productID uuid.UUID) ([]*models.Price, error) {
	// Archived included. The catalog converge relies on this to reconcile
	// already-archived historical prices instead of re-creating them;
	// GetActiveByProductID is the non-archived variant.
	rows, err := s.db.Gen(ctx).ListPricesByProduct(ctx, productID)
	if err != nil {
		return nil, err
	}
	return pricesFromGen(rows)
}

func (s *PriceService) GetActiveByProductID(ctx context.Context, productID uuid.UUID) ([]*models.Price, error) {
	rows, err := s.db.Gen(ctx).ListActivePricesByProductOrdered(ctx, productID)
	if err != nil {
		return nil, err
	}
	return pricesFromGen(rows)
}

func (s *PriceService) GetAllActive(ctx context.Context) ([]*models.Price, error) {
	rows, err := s.db.Gen(ctx).ListAllActivePricesWithProduct(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*models.Price, 0, len(rows))
	for _, row := range rows {
		price, err := priceWithProduct(row.OpenrailsPrice, row.OpenrailsProduct)
		if err != nil {
			return nil, err
		}
		out = append(out, price)
	}
	return out, nil
}

func (s *PriceService) GetAll(ctx context.Context) ([]*models.Price, error) {
	rows, err := s.db.Gen(ctx).ListAllPricesWithProduct(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*models.Price, 0, len(rows))
	for _, row := range rows {
		price, err := priceWithProduct(row.OpenrailsPrice, row.OpenrailsProduct)
		if err != nil {
			return nil, err
		}
		out = append(out, price)
	}
	return out, nil
}

func priceWithProduct(p gen.OpenrailsPrice, prod gen.OpenrailsProduct) (*models.Price, error) {
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
	Archived  *bool      // Filter by archived flag (nil = all)
	Currency  string     // Filter by currency (e.g., "usd")
	ProductID *uuid.UUID // Filter by product ID
	Type      string     // Filter by "recurring" or "one_time"
}

// ListPaginated returns prices with pagination and optional filters
func (s *PriceService) ListPaginated(ctx context.Context, filter PriceFilter, limit, offset int) ([]*models.Price, int64, error) {
	var currency *string
	if filter.Currency != "" {
		currency = &filter.Currency
	}
	onlyRecurring := filter.Type == "recurring"
	onlyOneTime := filter.Type == "one_time"

	q := s.db.Gen(ctx)
	total, err := q.CountPricesFiltered(ctx, gen.CountPricesFilteredParams{
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
	rows, err := q.ListPricesFiltered(ctx, gen.ListPricesFilteredParams{
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
		price, err := priceWithProduct(row.OpenrailsPrice, row.OpenrailsProduct)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, price)
	}
	return out, total, nil
}

func (s *PriceService) GetByNMIPlan(ctx context.Context, rail, nmiPlanID string) (*models.Price, error) {
	rail = normalize.Lower(rail)
	if rail == "" {
		return nil, fmt.Errorf("nmi rail is required for plan %q", normalize.Trim(nmiPlanID))
	}
	// Archived prices must still resolve here so grandfathered subscriptions
	// keep billing.
	row, err := s.db.Gen(ctx).GetPriceByNMIPlan(ctx, gen.GetPriceByNMIPlanParams{
		Rail:   rail,
		PlanID: nmiPlanID,
	})
	if err != nil {
		return nil, err
	}
	return models.PriceFromGen(row)
}

func (s *PriceService) GetByCCBillPriceID(ctx context.Context, ccbillPriceID string) (*models.Price, error) {
	row, err := s.db.Gen(ctx).GetPriceWithProductByCCBillPriceID(ctx, ccbillPriceID)
	if err != nil {
		return nil, err
	}
	return priceWithProduct(row.OpenrailsPrice, row.OpenrailsProduct)
}

func (s *PriceService) GetByStripePriceID(ctx context.Context, stripePriceID string) (*models.Price, error) {
	row, err := s.db.Gen(ctx).GetPriceWithProductByStripePriceID(ctx, stripePriceID)
	if err != nil {
		return nil, err
	}
	return priceWithProduct(row.OpenrailsPrice, row.OpenrailsProduct)
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
	rows, err := s.db.Gen(ctx).UpdatePriceStatus(ctx, gen.UpdatePriceStatusParams{
		ID:       id,
		Archived: archived,
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

// UpdatePSPLinks updates the PSP link entries (external IDs, does not affect
// historical data).
func (s *PriceService) UpdatePSPLinks(ctx context.Context, id uuid.UUID, links map[string]map[string]string) error {
	linksJSONB, err := models.ToJSONB(links)
	if err != nil {
		return err
	}
	rows, err := s.db.Gen(ctx).UpdatePricePSPLinks(ctx, gen.UpdatePricePSPLinksParams{
		ID:       id,
		PspLinks: linksJSONB,
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
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
	rows, err := s.db.Gen(ctx).UpdatePriceKey(ctx, gen.UpdatePriceKeyParams{
		ID:  id,
		Key: key,
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

// GetCurrentByKey returns the CURRENT (non-archived) row for a key, or
// pgx.ErrNoRows if the key names no live price. At most one such row can
// exist per (merchant, key) — enforced by uq_prices_merchant_key_current.
func (s *PriceService) GetCurrentByKey(ctx context.Context, merchantID uuid.UUID, key string) (*models.Price, error) {
	row, err := s.db.Gen(ctx).GetCurrentPriceByKey(ctx, gen.GetCurrentPriceByKeyParams{
		MerchantID: merchantID,
		Key:        key,
	})
	if err != nil {
		return nil, err
	}
	return models.PriceFromGen(row)
}

// ListChainByKey returns every row (archived + current) that has ever been
// named by this key — the version chain.
func (s *PriceService) ListChainByKey(ctx context.Context, merchantID uuid.UUID, key string) ([]*models.Price, error) {
	rows, err := s.db.Gen(ctx).ListPriceChainByKey(ctx, gen.ListPriceChainByKeyParams{
		MerchantID: merchantID,
		Key:        key,
	})
	if err != nil {
		return nil, err
	}
	return pricesFromGen(rows)
}

// ListPriorVersionsByKey returns the archived members of a key's chain —
// #773's "all prior versions of key K", the reprice_all_prior_versions bulk
// target set.
func (s *PriceService) ListPriorVersionsByKey(ctx context.Context, merchantID uuid.UUID, key string) ([]*models.Price, error) {
	rows, err := s.db.Gen(ctx).ListPriorVersionsByKey(ctx, gen.ListPriorVersionsByKeyParams{
		MerchantID: merchantID,
		Key:        key,
	})
	if err != nil {
		return nil, err
	}
	return pricesFromGen(rows)
}

// RecordKeyMovement appends one entry to the pointer-movement history log:
// key's current pointer moved to priceID at effectiveAt. Append-only — call
// exactly once per genuine movement (new row, reactivation, or repoint), never
// on a true no-op (same key, same already-current substance).
func (s *PriceService) RecordKeyMovement(ctx context.Context, merchantID, priceID uuid.UUID, key string, effectiveAt time.Time) error {
	rows, err := s.db.Gen(ctx).InsertPriceKeyMovement(ctx, gen.InsertPriceKeyMovementParams{
		MerchantID:  merchantID,
		Key:         key,
		PriceID:     priceID,
		EffectiveAt: effectiveAt,
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

// ListKeyMovements returns the full movement history for a key, most-recent
// first.
func (s *PriceService) ListKeyMovements(ctx context.Context, merchantID uuid.UUID, key string) ([]*models.PriceKeyMovement, error) {
	rows, err := s.db.Gen(ctx).ListPriceKeyMovements(ctx, gen.ListPriceKeyMovementsParams{
		MerchantID: merchantID,
		Key:        key,
	})
	if err != nil {
		return nil, err
	}
	return models.PriceKeyMovementsFromGen(rows), nil
}
