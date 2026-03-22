package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/models"
)

const (
	devMobiusProductSlug       = "e2e_mobius"
	devMobiusProductName       = "E2E Mobius Plan"
	devMobiusProductDesc       = "Local E2E product for Mobius/NMI sandbox"
	devMobiusPriceName         = "E2E Mobius Monthly (1 day cadence recommended for rebill tests)"
	devMobiusPlanID            = "nmi_premium_monthly"
	devCCBillProductSlug       = "basic"
	devCCBillProductName       = "Basic"
	devCCBillProductDesc       = "Local dev basic monthly subscription"
	devCCBillPriceName         = "basic-monthly"
	devCCBillFlexID            = "681cb38f-afb9-4665-931f-2b896072178a"
	devCCBillAmountCents int64 = 999
	devMobiusAmountCents int64 = 999
	devCurrency                = "usd"
	devCCBillCycleDays         = 30
	devMobiusCycleDays         = 1
)

func seedDevCatalog(cmd *cobra.Command, _ []string) error {
	cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	application, err := app.Bootstrap(cfg)
	if err != nil {
		return fmt.Errorf("bootstrap application: %w", err)
	}
	defer func() {
		if closeErr := application.Close(context.Background()); closeErr != nil {
			log.WithError(closeErr).Error("Application cleanup failed")
		}
	}()

	ctx := cmd.Context()
	runtime := application.Runtime

	mobiusProduct, err := ensureProduct(ctx, runtime.ProductService, devMobiusProductSlug, devMobiusProductName, devMobiusProductDesc, "e2e", 1)
	if err != nil {
		return fmt.Errorf("ensure mobius product: %w", err)
	}
	mobiusPrice, err := ensureRecurringPrice(ctx, runtime.PriceService, runtime.ProductService, recurringPriceSpec{
		ProductID:   mobiusProduct.ID,
		DisplayName: devMobiusPriceName,
		Amount:      devMobiusAmountCents,
		Currency:    devCurrency,
		CycleDays:   devMobiusCycleDays,
		Processors: map[string]map[string]string{
			"mobius": {
				models.ProcessorKeyPlanID:   devMobiusPlanID,
				models.ProcessorKeyProvider: "mobius",
			},
		},
		Lookup: func(ctx context.Context) (*models.Price, error) {
			return runtime.PriceService.GetByNMIPlan(ctx, "mobius", devMobiusPlanID)
		},
	})
	if err != nil {
		return fmt.Errorf("ensure mobius price: %w", err)
	}

	ccbillProduct, err := ensureProduct(ctx, runtime.ProductService, devCCBillProductSlug, devCCBillProductName, devCCBillProductDesc, "basic", 1)
	if err != nil {
		return fmt.Errorf("ensure ccbill product: %w", err)
	}
	ccbillPrice, err := ensureRecurringPrice(ctx, runtime.PriceService, runtime.ProductService, recurringPriceSpec{
		ProductID:   ccbillProduct.ID,
		DisplayName: strings.TrimSpace(devCCBillPriceName),
		Amount:      devCCBillAmountCents,
		Currency:    devCurrency,
		CycleDays:   devCCBillCycleDays,
		Processors: map[string]map[string]string{
			"ccbill": {
				models.ProcessorKeyCCBillFlexID:  devCCBillFlexID,
				models.ProcessorKeyStripePriceID: devCCBillFlexID,
			},
		},
		Lookup: func(ctx context.Context) (*models.Price, error) {
			return runtime.PriceService.GetByCCBillPriceID(ctx, devCCBillFlexID)
		},
	})
	if err != nil {
		return fmt.Errorf("ensure ccbill price: %w", err)
	}

	fmt.Printf("seeded dev catalog\n")
	fmt.Printf("mobius product_id=%s price_id=%s plan_id=%s\n", mobiusProduct.ID, mobiusPrice.ID, devMobiusPlanID)
	fmt.Printf("ccbill product_id=%s price_id=%s flex_id=%s\n", ccbillProduct.ID, ccbillPrice.ID, devCCBillFlexID)
	return nil
}

type recurringPriceSpec struct {
	ProductID   uuid.UUID
	DisplayName string
	Amount      int64
	Currency    string
	CycleDays   int
	Processors  map[string]map[string]string
	Lookup      func(context.Context) (*models.Price, error)
}

func ensureProduct(ctx context.Context, svc interface {
	Create(context.Context, *models.Product) error
	GetBySlug(context.Context, string) (*models.Product, error)
	Activate(context.Context, uuid.UUID) error
	UpdateDisplayName(context.Context, uuid.UUID, string) error
	UpdateDescription(context.Context, uuid.UUID, string) error
}, slug, displayName, description, tierGroup string, tierRank int) (*models.Product, error) {
	product, err := svc.GetBySlug(ctx, slug)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		product = &models.Product{
			Slug:        slug,
			DisplayName: displayName,
			Description: description,
			TierGroup:   &tierGroup,
			TierRank:    tierRank,
			IsActive:    true,
		}
		if err := svc.Create(ctx, product); err != nil {
			return nil, err
		}
		return svc.GetBySlug(ctx, slug)
	}

	if !product.IsActive {
		if err := svc.Activate(ctx, product.ID); err != nil {
			return nil, err
		}
	}
	if product.DisplayName != displayName {
		if err := svc.UpdateDisplayName(ctx, product.ID, displayName); err != nil {
			return nil, err
		}
	}
	if product.Description != description {
		if err := svc.UpdateDescription(ctx, product.ID, description); err != nil {
			return nil, err
		}
	}
	return svc.GetBySlug(ctx, slug)
}

func ensureRecurringPrice(ctx context.Context, priceSvc interface {
	Create(context.Context, *models.Price) error
	Activate(context.Context, uuid.UUID) error
	UpdateDisplayName(context.Context, uuid.UUID, string) error
	UpdateProcessors(context.Context, uuid.UUID, map[string]map[string]string) error
}, productSvc interface {
	GetByID(context.Context, uuid.UUID) (*models.Product, error)
}, spec recurringPriceSpec) (*models.Price, error) {
	price, err := spec.Lookup(ctx)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		cycleDays := spec.CycleDays
		price = &models.Price{
			ProductID:        spec.ProductID,
			DisplayName:      spec.DisplayName,
			IsActive:         true,
			Amount:           spec.Amount,
			Currency:         spec.Currency,
			BillingCycleDays: &cycleDays,
			Processors:       spec.Processors,
		}
		if err := priceSvc.Create(ctx, price); err != nil {
			return nil, err
		}
		return spec.Lookup(ctx)
	}

	if !price.IsActive {
		if err := priceSvc.Activate(ctx, price.ID); err != nil {
			return nil, err
		}
	}
	if price.ProductID != spec.ProductID {
		product, err := productSvc.GetByID(ctx, price.ProductID)
		if err != nil {
			return nil, fmt.Errorf("existing price belongs to different product (%s): %w", price.ProductID, err)
		}
		return nil, fmt.Errorf("existing price %s is attached to unexpected product slug %s", price.ID, product.Slug)
	}
	if price.Amount != spec.Amount || !strings.EqualFold(price.Currency, spec.Currency) || price.BillingCycleDays == nil || *price.BillingCycleDays != spec.CycleDays {
		return nil, fmt.Errorf("existing price %s has immutable values that do not match requested seed", price.ID)
	}
	if price.DisplayName != spec.DisplayName {
		if err := priceSvc.UpdateDisplayName(ctx, price.ID, spec.DisplayName); err != nil {
			return nil, err
		}
	}
	if err := priceSvc.UpdateProcessors(ctx, price.ID, spec.Processors); err != nil {
		return nil, err
	}
	return spec.Lookup(ctx)
}
