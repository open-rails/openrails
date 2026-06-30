package catalog

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/shared/normalize"
)

type PriceService struct {
	repo *repo.PriceRepo
}

func NewPriceService(db *db.DB) *PriceService {
	return &PriceService{repo: repo.NewPriceRepo(db)}
}

func (s *PriceService) Create(ctx context.Context, price *models.Price) error {
	return s.repo.Create(ctx, price)
}

func (s *PriceService) GetByID(ctx context.Context, id uuid.UUID) (*models.Price, error) {
	return s.repo.GetByID(ctx, id)
}

func (s *PriceService) GetByProductID(ctx context.Context, productID uuid.UUID) ([]*models.Price, error) {
	return s.repo.GetByProductID(ctx, productID)
}

func (s *PriceService) GetByNMIPlan(ctx context.Context, rail, nmiPlanID string) (*models.Price, error) {
	rail = normalize.Lower(rail)
	if rail == "" {
		return nil, fmt.Errorf("nmi rail is required for plan %q", normalize.Trim(nmiPlanID))
	}
	return s.repo.GetByNMIPlan(ctx, rail, nmiPlanID)
}

func (s *PriceService) GetByCCBillPriceID(ctx context.Context, ccbillPriceID string) (*models.Price, error) {
	return s.repo.GetByCCBillPriceID(ctx, ccbillPriceID)
}

func (s *PriceService) GetByStripePriceID(ctx context.Context, stripePriceID string) (*models.Price, error) {
	return s.repo.GetByStripePriceID(ctx, stripePriceID)
}

func (s *PriceService) GetActiveByProductID(ctx context.Context, productID uuid.UUID) ([]*models.Price, error) {
	return s.repo.GetActiveByProductID(ctx, productID)
}

func (s *PriceService) GetAllActive(ctx context.Context) ([]*models.Price, error) {
	return s.repo.GetAllActive(ctx)
}

func (s *PriceService) GetAll(ctx context.Context) ([]*models.Price, error) {
	return s.repo.GetAll(ctx)
}

// PriceFilter contains optional filters for listing prices
type PriceFilter = repo.PriceFilter

// ListPaginated returns prices with pagination and optional filters
func (s *PriceService) ListPaginated(ctx context.Context, filter PriceFilter, limit, offset int) ([]*models.Price, int64, error) {
	return s.repo.ListPaginated(ctx, filter, limit, offset)
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
	price, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	price.Status = status
	return s.repo.Update(ctx, price)
}

// UpdateRails updates the rail mappings (external IDs, does not affect historical data).
// This is useful when adding new rails or updating external price/plan IDs.
func (s *PriceService) UpdateRails(ctx context.Context, id uuid.UUID, rails map[string]map[string]string) error {
	price, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	price.Rails = rails
	return s.repo.Update(ctx, price)
}
