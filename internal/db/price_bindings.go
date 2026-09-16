package db

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/merchant"
)

// PriceFromGen loads the normalized account bindings with their current display
// keys. PSP identity is carried in each entry and never derived from its label.
func (d *DB) PriceFromGen(ctx context.Context, row gen.OpenrailsPrice) (*models.Price, error) {
	price, err := models.PriceFromGen(row)
	if err != nil {
		return nil, err
	}
	if err := d.LoadPricePSPBindings(ctx, []*models.Price{price}, nil); err != nil {
		return nil, err
	}
	return price, nil
}

// LoadPricePSPBindings includes archived accounts: their references still own
// historical money and subscriptions. New checkout admission applies separately.
func (d *DB) LoadPricePSPBindings(ctx context.Context, prices []*models.Price, pspID *uuid.UUID) error {
	if len(prices) == 0 {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, 0, len(prices))
	byID := make(map[uuid.UUID]*models.Price, len(prices))
	for _, price := range prices {
		if price.MerchantID != mid.UUID() {
			return fmt.Errorf("price %s does not belong to scoped merchant", price.ID)
		}
		price.PSPLinks = nil
		ids = append(ids, price.ID)
		byID[price.ID] = price
	}
	rows, err := d.Gen(ctx).ListPricePSPBindings(ctx, gen.ListPricePSPBindingsParams{MerchantID: mid.UUID(), PriceIds: ids, PspID: pspID})
	if err != nil {
		return err
	}
	ambiguousKeys := map[uuid.UUID]map[string]bool{}
	for _, row := range rows {
		var cfg map[string]string
		if err := json.Unmarshal(row.Configuration, &cfg); err != nil {
			return fmt.Errorf("price binding %s/%s: %w", row.PriceID, row.PspID, err)
		}
		if cfg == nil {
			cfg = map[string]string{}
		}
		cfg[models.RailKeyRail] = row.Rail
		cfg[models.RailKeyPSPID] = row.PspID.String()
		for key, value := range map[string]*string{
			models.RailKeyPlanID: row.PlanID, models.RailKeyStripePriceID: row.PriceRef,
			models.RailKeyCCBillRecurringBillingOption: row.RecurringBillingOptionID,
			"plan_pda": row.PlanPda, models.RailKeyCCBillFlexID: row.FlexID,
		} {
			if value != nil {
				cfg[key] = *value
			}
		}
		price := byID[row.PriceID]
		if price.PSPLinks == nil {
			price.PSPLinks = map[string]map[string]string{}
		}
		key := row.PspKey
		if ambiguousKeys[price.ID] == nil {
			ambiguousKeys[price.ID] = map[string]bool{}
		}
		if prior, ok := price.PSPLinks[key]; ok && prior[models.RailKeyPSPID] != row.PspID.String() {
			delete(price.PSPLinks, key)
			price.PSPLinks[prior[models.RailKeyPSPID]] = prior
			ambiguousKeys[price.ID][key] = true
		}
		if ambiguousKeys[price.ID][key] {
			key = row.PspID.String()
		}
		price.PSPLinks[key] = cfg
	}
	return nil
}
