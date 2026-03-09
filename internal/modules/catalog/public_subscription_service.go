package catalog

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/internal/db/models"

	log "github.com/sirupsen/logrus"
)

// PublicSubscriptionService handles public-facing subscription operations
type PublicSubscriptionService struct {
	ProductService *ProductService
	PriceService   *PriceService
}

// PublicProductResponse represents a product with pricing for public display
type PublicProductResponse struct {
	*models.Product
	Prices []*models.Price `json:"prices"`
}

// GetAvailableProducts returns all active products with their prices for public consumption
func (s *PublicSubscriptionService) GetAvailableProducts(ctx context.Context) ([]*PublicProductResponse, error) {
	return s.GetProducts(ctx, false)
}

// GetProducts returns products with their prices. If includeInactive is true, returns all products.
func (s *PublicSubscriptionService) GetProducts(ctx context.Context, includeInactive bool) ([]*PublicProductResponse, error) {
	var products []*models.Product
	var err error

	if includeInactive {
		products, err = s.ProductService.GetAll(ctx)
	} else {
		products, err = s.ProductService.GetActive(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get products: %w", err)
	}

	return s.hydrateProductPrices(ctx, products, includeInactive)
}

// PaginatedProductsResult contains products and pagination info
type PaginatedProductsResult struct {
	Products   []*PublicProductResponse
	TotalItems int64
}

// GetProductsPaginated returns products with pagination support.
// If includeInactive is true, returns all products including inactive ones.
func (s *PublicSubscriptionService) GetProductsPaginated(ctx context.Context, includeInactive bool, limit, offset int) (*PaginatedProductsResult, error) {
	var products []*models.Product
	var totalItems int64
	var err error

	if includeInactive {
		products, totalItems, err = s.ProductService.GetAllPaginated(ctx, limit, offset)
	} else {
		products, totalItems, err = s.ProductService.GetActivePaginated(ctx, limit, offset)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get products: %w", err)
	}

	responses, err := s.hydrateProductPrices(ctx, products, includeInactive)
	if err != nil {
		return nil, err
	}

	return &PaginatedProductsResult{
		Products:   responses,
		TotalItems: totalItems,
	}, nil
}

// hydrateProductPrices attaches prices to each product
func (s *PublicSubscriptionService) hydrateProductPrices(ctx context.Context, products []*models.Product, includeInactive bool) ([]*PublicProductResponse, error) {
	responses := make([]*PublicProductResponse, len(products))
	for i, product := range products {
		responses[i] = &PublicProductResponse{
			Product: product,
		}

		// Get prices for this product
		var prices []*models.Price
		var err error
		if includeInactive {
			prices, err = s.PriceService.GetByProductID(ctx, product.ID)
		} else {
			prices, err = s.PriceService.GetActiveByProductID(ctx, product.ID)
		}
		if err != nil {
			log.WithFields(log.Fields{
				"product_id": product.ID,
				"error":      err.Error(),
			}).Warn("Failed to load prices for product in public API")
			prices = []*models.Price{}
		}

		responses[i].Prices = prices
	}

	return responses, nil
}

// NewPublicSubscriptionService creates a new PublicSubscriptionService
func NewPublicSubscriptionService(
	productService *ProductService,
	priceService *PriceService,
) *PublicSubscriptionService {
	return &PublicSubscriptionService{
		ProductService: productService,
		PriceService:   priceService,
	}
}
