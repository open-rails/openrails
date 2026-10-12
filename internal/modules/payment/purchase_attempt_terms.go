package payment

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
)

func permanentPurchase(price *models.Price) bool {
	return price != nil && !price.IsRecurring() && price.AccessDurationHours == nil
}

func (s *FulfillmentService) permanentCoverage(ctx context.Context, user string, product *models.Product) (*CoverageInfo, error) {
	coverage := &CoverageInfo{}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	customer, err := customerIDFromUser(user)
	if err != nil {
		return nil, err
	}
	covered, err := s.database.Gen(ctx).PermanentBenefitsCovered(ctx, gen.PermanentBenefitsCoveredParams{MerchantID: mid.UUID(), CustomerID: customer, ProductID: product.ID, AtTime: s.now()})
	if err != nil {
		return nil, err
	}
	if covered != nil && *covered {
		coverage.HasCoverage = true
		coverage.IsIndefinite = true
		coverage.SourceType = "product"
	}
	return coverage, nil
}

func (s *FulfillmentService) purchaseCoverage(ctx context.Context, user string, price *models.Price, product *models.Product) (*CoverageInfo, error) {
	if product.CreditGrant != nil {
		return &CoverageInfo{}, nil
	}
	if permanentPurchase(price) && s.database != nil {
		return s.permanentCoverage(ctx, user, product)
	}
	return s.GetUserProductCoverage(ctx, user, product)
}

func (s *FulfillmentService) checkPermanentOwnership(ctx context.Context, user string, price *models.Price, product *models.Product) error {
	if !permanentPurchase(price) || product.CreditGrant != nil || s.database == nil {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	customer, err := customerIDFromUser(user)
	if err != nil {
		return err
	}
	owned, err := s.database.Gen(ctx).HasPermanentProductAccess(ctx, gen.HasPermanentProductAccessParams{MerchantID: mid.UUID(), CustomerID: customer, ProductID: product.ID, AtTime: s.now()})
	if err != nil {
		return err
	}
	if owned {
		return fmt.Errorf("%w: customer already owns this product", ErrOrderAttemptConflict)
	}
	return nil
}
