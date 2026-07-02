package catalog

import (
	"context"
	"errors"
	"fmt"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/normalize"
)

type PriceService struct {
	db *db.DB
}

func NewPriceService(db *db.DB) *PriceService {
	return &PriceService{db: db}
}

func priceRailsJSONB(p *models.Price) ([]byte, error) {
	return models.ToJSONB(p.Rails)
}

func (s *PriceService) Create(ctx context.Context, price *models.Price) error {
	rails, err := priceRailsJSONB(price)
	if err != nil {
		return err
	}
	rows, err := s.db.Gen(ctx).CreatePrice(ctx, gen.CreatePriceParams{
		ID:                  price.ID,
		MerchantID:          price.MerchantID,
		ProductID:           price.ProductID,
		Status:              string(price.Status),
		Amount:              price.Amount,
		Currency:            price.Currency,
		AccessDurationHours: models.IntPtrTo32(price.AccessDurationHours),
		AutoRenew:           price.AutoRenew,
		TrialUnitAmount:     price.TrialUnitAmount,
		TrialDurationHours:  models.IntPtrTo32(price.TrialDurationHours),
		Rails:               rails,
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
	// All statuses (active + archived/draft). The catalog converge relies on this
	// to reconcile already-archived historical prices instead of re-creating
	// them; GetActiveByProductID is the active-only variant.
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
	Active    *bool                 // Filter by active status (true=status='active', false=status<>'active')
	Status    *models.CatalogStatus // Filter by exact lifecycle status (admin)
	Currency  string                // Filter by currency (e.g., "usd")
	ProductID *uuid.UUID            // Filter by product ID
	Type      string                // Filter by "recurring" or "one_time"
}

// ListPaginated returns prices with pagination and optional filters
func (s *PriceService) ListPaginated(ctx context.Context, filter PriceFilter, limit, offset int) ([]*models.Price, int64, error) {
	onlyActive := filter.Active != nil && *filter.Active
	onlyInactive := filter.Active != nil && !*filter.Active
	var status *string
	if filter.Status != nil {
		s := string(*filter.Status)
		status = &s
	}
	var currency *string
	if filter.Currency != "" {
		currency = &filter.Currency
	}
	onlyRecurring := filter.Type == "recurring"
	onlyOneTime := filter.Type == "one_time"

	q := s.db.Gen(ctx)
	total, err := q.CountPricesFiltered(ctx, gen.CountPricesFilteredParams{
		OnlyActive:    onlyActive,
		OnlyInactive:  onlyInactive,
		Status:        status,
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
		OnlyActive:    onlyActive,
		OnlyInactive:  onlyInactive,
		Status:        status,
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
	// Resolve any non-draft price (active or archived). Archived prices must
	// still resolve here so grandfathered subscriptions keep openrails.
	row, err := s.db.Gen(ctx).GetPriceByNMIPlan(ctx, gen.GetPriceByNMIPlanParams{
		Rail:               rail,
		PlanID:             nmiPlanID,
		IncludeNmiFallback: rail != string(models.RailNMI),
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
// Use UpdateRails() for non-financial fields.
func (s *PriceService) Update(ctx context.Context, price *models.Price) error {
	return errors.New("prices are immutable; use UpdateRails() or Deactivate() for allowed changes")
}

// Delete is not supported - prices are immutable to preserve historical payment accuracy.
// To retire a price, archive it via Deactivate() (sets status=archived).
func (s *PriceService) Delete(ctx context.Context, id uuid.UUID) error {
	return errors.New("prices cannot be deleted; use Deactivate() instead to preserve historical data")
}

// updateRow writes the full price row (the raw persistence Update, formerly
// repo.PriceRepo.Update). Unexported on purpose: the public Update() forbids
// arbitrary changes; the allowed mutations (SetStatus, UpdateRails) funnel here.
func (s *PriceService) updateRow(ctx context.Context, price *models.Price) error {
	rails, err := priceRailsJSONB(price)
	if err != nil {
		return err
	}
	rows, err := s.db.Gen(ctx).UpdatePrice(ctx, gen.UpdatePriceParams{
		ID:                  price.ID,
		ProductID:           price.ProductID,
		Status:              string(price.Status),
		Amount:              price.Amount,
		Currency:            price.Currency,
		AccessDurationHours: models.IntPtrTo32(price.AccessDurationHours),
		AutoRenew:           price.AutoRenew,
		TrialUnitAmount:     price.TrialUnitAmount,
		TrialDurationHours:  models.IntPtrTo32(price.TrialDurationHours),
		Rails:               rails,
		UpdatedAt:           models.UpdateTimestamp(price.UpdatedAt),
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

// Deactivate archives a price (status=archived) so it won't appear in product
// listings and cannot be purchased by new customers. Existing subscriptions and
// payments referencing this price are grandfathered and keep openrails.
func (s *PriceService) Deactivate(ctx context.Context, id uuid.UUID) error {
	return s.SetStatus(ctx, id, models.CatalogStatusArchived)
}

// Activate marks a price as active so it appears in product listings.
func (s *PriceService) Activate(ctx context.Context, id uuid.UUID) error {
	return s.SetStatus(ctx, id, models.CatalogStatusActive)
}

// SetStatus sets the lifecycle status (draft|active|archived) on a price.
func (s *PriceService) SetStatus(ctx context.Context, id uuid.UUID, status models.CatalogStatus) error {
	if !status.Valid() {
		return fmt.Errorf("invalid catalog status %q", status)
	}
	price, err := s.GetByID(ctx, id)
	if err != nil {
		return err
	}
	price.Status = status
	return s.updateRow(ctx, price)
}

// UpdateRails updates the rail mappings (external IDs, does not affect historical data).
// This is useful when adding new rails or updating external price/plan IDs.
func (s *PriceService) UpdateRails(ctx context.Context, id uuid.UUID, rails map[string]map[string]string) error {
	price, err := s.GetByID(ctx, id)
	if err != nil {
		return err
	}
	price.Rails = rails
	return s.updateRow(ctx, price)
}
