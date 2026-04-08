package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

type CreditGrantCadence string

const (
	CreditGrantCadenceOnce       CreditGrantCadence = "once"
	CreditGrantCadencePerRenewal CreditGrantCadence = "per_renewal"
)

type CreditGrantSpec struct {
	Amount      int64              `json:"amount"`
	ExpiresDays *int               `json:"expires_days,omitempty"`
	Cadence     CreditGrantCadence `json:"cadence,omitempty"`
}

type CreditsSpec map[string]CreditGrantSpec

func toModelCreditsSpec(in CreditsSpec) models.CreditsSpec {
	if in == nil {
		return nil
	}
	out := make(models.CreditsSpec, len(in))
	for k, v := range in {
		cadence := models.CreditGrantCadence(v.Cadence)
		out[k] = models.CreditGrantSpec{
			Amount:      v.Amount,
			ExpiresDays: v.ExpiresDays,
			Cadence:     cadence,
		}
	}
	return out
}

type CatalogProduct struct {
	ID               uuid.UUID       `json:"id"`
	Slug             string          `json:"slug"`
	DisplayName      string          `json:"display_name"`
	Description      string          `json:"description"`
	EntitlementsSpec map[string]*int `json:"entitlements_spec,omitempty"`
	CreditsSpec      CreditsSpec     `json:"credits_spec,omitempty"`
	TierGroup        *string         `json:"tier_group,omitempty"`
	TierRank         int             `json:"tier_rank"`
	IsActive         bool            `json:"is_active"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

type CreateProductRequest struct {
	Slug             string          `json:"slug"`
	DisplayName      string          `json:"display_name"`
	Description      string          `json:"description"`
	EntitlementsSpec map[string]*int `json:"entitlements_spec,omitempty"`
	CreditsSpec      CreditsSpec     `json:"credits_spec,omitempty"`
	TierGroup        *string         `json:"tier_group,omitempty"`
	TierRank         int             `json:"tier_rank,omitempty"`
	IsActive         *bool           `json:"is_active,omitempty"`
}

func (s *Service) CreateProduct(ctx context.Context, req CreateProductRequest) (*CatalogProduct, error) {
	products, err := s.requireProductService()
	if err != nil {
		return nil, err
	}
	req.Slug = strings.TrimSpace(req.Slug)
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	req.Description = strings.TrimSpace(req.Description)
	if req.Slug == "" {
		return nil, fmt.Errorf("slug required")
	}
	if req.DisplayName == "" {
		return nil, fmt.Errorf("display_name required")
	}

	now := time.Now().UTC()
	active := true
	if req.IsActive != nil {
		active = *req.IsActive
	}
	p := &models.Product{
		ID:               uuid.New(),
		Slug:             req.Slug,
		DisplayName:      req.DisplayName,
		Description:      req.Description,
		EntitlementsSpec: req.EntitlementsSpec,
		CreditsSpec:      toModelCreditsSpec(req.CreditsSpec),
		TierGroup:        req.TierGroup,
		TierRank:         req.TierRank,
		IsActive:         active,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := products.Create(ctx, p); err != nil {
		return nil, err
	}
	return productToCatalogProduct(p), nil
}

type UpdateProductRequest struct {
	DisplayName      *string         `json:"display_name,omitempty"`
	Description      *string         `json:"description,omitempty"`
	EntitlementsSpec map[string]*int `json:"entitlements_spec,omitempty"`
	SetEntitlements  bool            `json:"set_entitlements,omitempty"`
	CreditsSpec      CreditsSpec     `json:"credits_spec,omitempty"`
	SetCredits       bool            `json:"set_credits,omitempty"`
	TierGroup        *string         `json:"tier_group,omitempty"`
	SetTierGroup     bool            `json:"set_tier_group,omitempty"`
	TierRank         *int            `json:"tier_rank,omitempty"`
	IsActive         *bool           `json:"is_active,omitempty"`
}

func (s *Service) UpdateProduct(ctx context.Context, productID uuid.UUID, req UpdateProductRequest) (*CatalogProduct, error) {
	products, err := s.requireProductService()
	if err != nil {
		return nil, err
	}
	if productID == uuid.Nil {
		return nil, fmt.Errorf("product_id required")
	}
	p, err := products.UpdateDefinition(ctx, productID, catalog.ProductDefinitionUpdateParams{
		DisplayName:      req.DisplayName,
		Description:      req.Description,
		EntitlementsSpec: req.EntitlementsSpec,
		SetEntitlements:  req.SetEntitlements,
		CreditsSpec:      toModelCreditsSpec(req.CreditsSpec),
		SetCredits:       req.SetCredits,
		TierGroup:        req.TierGroup,
		SetTierGroup:     req.SetTierGroup,
		TierRank:         req.TierRank,
		IsActive:         req.IsActive,
	})
	if err != nil {
		return nil, err
	}
	return productToCatalogProduct(p), nil
}

func productToCatalogProduct(p *models.Product) *CatalogProduct {
	var credits CreditsSpec
	if len(p.CreditsSpec) > 0 {
		credits = make(CreditsSpec, len(p.CreditsSpec))
		for k, v := range p.CreditsSpec {
			credits[k] = CreditGrantSpec{
				Amount:      v.Amount,
				ExpiresDays: v.ExpiresDays,
				Cadence:     CreditGrantCadence(v.Cadence),
			}
		}
	}
	return &CatalogProduct{
		ID:               p.ID,
		Slug:             p.Slug,
		DisplayName:      p.DisplayName,
		Description:      p.Description,
		EntitlementsSpec: p.EntitlementsSpec,
		CreditsSpec:      credits,
		TierGroup:        p.TierGroup,
		TierRank:         p.TierRank,
		IsActive:         p.IsActive,
		CreatedAt:        p.CreatedAt,
		UpdatedAt:        p.UpdatedAt,
	}
}

type CatalogPrice struct {
	ID               uuid.UUID                    `json:"id"`
	ProductID        uuid.UUID                    `json:"product_id"`
	DisplayName      string                       `json:"display_name"`
	IsActive         bool                         `json:"is_active"`
	Amount           int64                        `json:"amount"`
	Currency         string                       `json:"currency"`
	BillingCycleDays *int                         `json:"billing_cycle_days,omitempty"`
	Processors       map[string]map[string]string `json:"processors,omitempty"`
	CreatedAt        time.Time                    `json:"created_at"`
	UpdatedAt        time.Time                    `json:"updated_at"`
}

type ProcessorMappingMode struct {
	Link   map[string]string `json:"link,omitempty"`
	Create map[string]string `json:"create,omitempty"`
}

type CreatePriceRequest struct {
	ProductID        uuid.UUID                       `json:"product_id"`
	DisplayName      string                          `json:"display_name"`
	Amount           int64                           `json:"amount"`
	Currency         string                          `json:"currency"`
	BillingCycleDays *int                            `json:"billing_cycle_days,omitempty"`
	Processors       map[string]ProcessorMappingMode `json:"processors,omitempty"`
	IsActive         *bool                           `json:"is_active,omitempty"`
}

func (s *Service) CreatePrice(ctx context.Context, req CreatePriceRequest) (*CatalogPrice, error) {
	products, prices, err := s.requireCatalogServices()
	if err != nil {
		return nil, err
	}
	if req.ProductID == uuid.Nil {
		return nil, fmt.Errorf("product_id required")
	}
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	req.Currency = strings.ToLower(strings.TrimSpace(req.Currency))
	if req.DisplayName == "" {
		return nil, fmt.Errorf("display_name required")
	}
	if req.Amount <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}
	if req.Currency == "" {
		return nil, fmt.Errorf("currency required")
	}

	// Validate product exists.
	product, err := products.GetByID(ctx, req.ProductID)
	if err != nil {
		return nil, fmt.Errorf("product not found")
	}

	priceID := uuid.New()
	processors, err := s.resolveProcessorMappings(ctx, product, req, priceID)
	if err != nil {
		return nil, err
	}

	active := true
	if req.IsActive != nil {
		active = *req.IsActive
	}
	now := time.Now().UTC()
	price := &models.Price{
		ID:               priceID,
		ProductID:        req.ProductID,
		DisplayName:      req.DisplayName,
		IsActive:         active,
		Amount:           req.Amount,
		Currency:         req.Currency,
		BillingCycleDays: req.BillingCycleDays,
		Processors:       processors,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := prices.Create(ctx, price); err != nil {
		return nil, err
	}
	return priceToCatalogPrice(price), nil
}

func (s *Service) resolveProcessorMappings(ctx context.Context, product *models.Product, req CreatePriceRequest, priceID uuid.UUID) (map[string]map[string]string, error) {
	if len(req.Processors) == 0 {
		return nil, nil
	}
	out := make(map[string]map[string]string, len(req.Processors))
	for name, mode := range req.Processors {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		if (mode.Link == nil) == (mode.Create == nil) {
			return nil, fmt.Errorf("processor '%s' must specify exactly one of link or create", name)
		}

		if mode.Link != nil {
			link := mode.Link
			switch name {
			case "stripe":
				id := strings.TrimSpace(link[models.ProcessorKeyStripePriceID])
				if id == "" {
					return nil, fmt.Errorf("stripe link requires processors['stripe'].link.price_id")
				}
				if s.rt.Config != nil && s.rt.Config.FeatureFlags != nil && s.rt.Config.FeatureFlags.VerifyProcessorMappings {
					stripeSvc := &catalog.StripeCatalogService{Config: s.rt.Config}
					if err := stripeSvc.VerifyPriceExists(ctx, id); err != nil {
						return nil, err
					}
				}
			case "ccbill":
				formName := strings.TrimSpace(link[models.ProcessorKeyCCBillFormName])
				flexID := strings.TrimSpace(link[models.ProcessorKeyCCBillFlexID])
				if formName == "" || flexID == "" {
					return nil, fmt.Errorf("ccbill link requires processors['ccbill'].link.form_name and flex_id")
				}
			case "mobius":
				planID := strings.TrimSpace(link[models.ProcessorKeyPlanID])
				if planID == "" {
					return nil, fmt.Errorf("%s link requires processors['%s'].link.plan_id", name, name)
				}
			}
			out[name] = link
			continue
		}

		// create mode (processor-dependent)
		switch name {
		case "stripe":
			if s.rt.Config == nil {
				return nil, fmt.Errorf("stripe create requested but config is unavailable")
			}
			stripeSvc := &catalog.StripeCatalogService{Config: s.rt.Config}
			idKeyProduct := "openrails-product-" + product.ID.String()
			stripeProductID, err := stripeSvc.CreateProduct(ctx, product.DisplayName, product.Description, idKeyProduct)
			if err != nil {
				return nil, err
			}
			idKeyPrice := "openrails-price-" + priceID.String()
			stripePriceID, err := stripeSvc.CreatePrice(ctx, stripeProductID, req.Amount, req.Currency, req.BillingCycleDays, idKeyPrice)
			if err != nil {
				return nil, err
			}
			out[name] = map[string]string{
				models.ProcessorKeyStripePriceID:   stripePriceID,
				models.ProcessorKeyStripeProductID: stripeProductID,
			}
		case "ccbill":
			return nil, fmt.Errorf("processor '%s' does not support auto-create; create products/prices in CCBill manually and use link mode", name)
		default:
			return nil, fmt.Errorf("processor '%s' does not support auto-create; use link mode", name)
		}
	}
	return out, nil
}

type UpdatePriceRequest struct {
	DisplayName   *string                      `json:"display_name,omitempty"`
	Processors    map[string]map[string]string `json:"processors,omitempty"`
	SetProcessors bool                         `json:"set_processors,omitempty"`
	IsActive      *bool                        `json:"is_active,omitempty"`
}

func (s *Service) UpdatePrice(ctx context.Context, priceID uuid.UUID, req UpdatePriceRequest) (*CatalogPrice, error) {
	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	if priceID == uuid.Nil {
		return nil, fmt.Errorf("price_id required")
	}
	if req.DisplayName != nil {
		if err := prices.UpdateDisplayName(ctx, priceID, strings.TrimSpace(*req.DisplayName)); err != nil {
			return nil, err
		}
	}
	if req.SetProcessors {
		if err := prices.UpdateProcessors(ctx, priceID, req.Processors); err != nil {
			return nil, err
		}
	}
	if req.IsActive != nil {
		if *req.IsActive {
			if err := prices.Activate(ctx, priceID); err != nil {
				return nil, err
			}
		} else {
			if err := prices.Deactivate(ctx, priceID); err != nil {
				return nil, err
			}
		}
	}
	updated, err := prices.GetByID(ctx, priceID)
	if err != nil {
		return nil, err
	}
	return priceToCatalogPrice(updated), nil
}

func priceToCatalogPrice(p *models.Price) *CatalogPrice {
	return &CatalogPrice{
		ID:               p.ID,
		ProductID:        p.ProductID,
		DisplayName:      p.DisplayName,
		IsActive:         p.IsActive,
		Amount:           p.Amount,
		Currency:         p.Currency,
		BillingCycleDays: p.BillingCycleDays,
		Processors:       p.Processors,
		CreatedAt:        p.CreatedAt,
		UpdatedAt:        p.UpdatedAt,
	}
}
