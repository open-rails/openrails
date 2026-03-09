package catalog

import (
	"context"
	"errors"

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

func (s *PriceService) GetByNMIPlan(ctx context.Context, provider, nmiPlanID string) (*models.Price, error) {
	provider = normalize.FirstNonEmpty(normalize.Lower(provider), "mobius")
	return s.repo.GetByNMIPlan(ctx, provider, nmiPlanID)
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
// Use UpdateDisplayName() or UpdateProcessors() for non-financial fields.
func (s *PriceService) Update(ctx context.Context, price *models.Price) error {
	return errors.New("prices are immutable; use UpdateDisplayName(), UpdateProcessors(), or Deactivate() for allowed changes")
}

// Delete is not supported - prices are immutable to preserve historical payment accuracy.
// To retire a price, set is_active = false via Deactivate().
func (s *PriceService) Delete(ctx context.Context, id uuid.UUID) error {
	return errors.New("prices cannot be deleted; use Deactivate() instead to preserve historical data")
}

// Deactivate marks a price as inactive so it won't appear in product listings.
// Existing subscriptions and payments referencing this price are unaffected.
func (s *PriceService) Deactivate(ctx context.Context, id uuid.UUID) error {
	price, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	price.IsActive = false
	return s.repo.Update(ctx, price)
}

// Activate marks a price as active so it appears in product listings.
func (s *PriceService) Activate(ctx context.Context, id uuid.UUID) error {
	price, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	price.IsActive = true
	return s.repo.Update(ctx, price)
}

// UpdateDisplayName updates only the display name (cosmetic, does not affect historical data).
func (s *PriceService) UpdateDisplayName(ctx context.Context, id uuid.UUID, displayName string) error {
	price, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	price.DisplayName = displayName
	return s.repo.Update(ctx, price)
}

// UpdateProcessors updates the processor mappings (external IDs, does not affect historical data).
// This is useful when adding new processors or updating external price/plan IDs.
func (s *PriceService) UpdateProcessors(ctx context.Context, id uuid.UUID, processors map[string]map[string]string) error {
	price, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	price.Processors = processors
	return s.repo.Update(ctx, price)
}
