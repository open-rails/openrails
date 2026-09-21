package catalog

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

// UpdatePSPLinks resolves input labels once and atomically replaces normalized
// bindings. A captured psp_id is authoritative through a label rename/archive.
func (s *PriceService) UpdatePSPLinks(ctx context.Context, priceID uuid.UUID, links map[string]map[string]string) error {
	mid, catalogID, err := queryCatalogScope(ctx)
	if err != nil {
		return err
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.LockPriceForBindingUpdate(ctx, gen.LockPriceForBindingUpdateParams{MerchantID: mid.UUID(), CatalogID: catalogID, PriceID: priceID}); err != nil {
			return err
		}
		if err := q.DeletePricePSPBindings(ctx, gen.DeletePricePSPBindingsParams{MerchantID: mid.UUID(), CatalogID: catalogID, PriceID: priceID}); err != nil {
			return err
		}
		seen := map[uuid.UUID]bool{}
		for key, values := range links {
			cfg := make(map[string]string, len(values))
			for k, value := range values {
				cfg[k] = value
			}
			var pspID *uuid.UUID
			if raw := strings.TrimSpace(cfg[models.RailKeyPSPID]); raw != "" {
				id, err := uuid.Parse(raw)
				if err != nil || id == uuid.Nil {
					return fmt.Errorf("invalid PSP identity for price binding %q", key)
				}
				pspID = &id
			} else if id, err := uuid.Parse(key); err == nil {
				pspID = &id
			}
			rail := strings.TrimSpace(cfg[models.RailKeyRail])
			if rail == "" {
				return fmt.Errorf("price binding %q requires rail", key)
			}
			accounts, err := q.ResolvePriceBindingPSP(ctx, gen.ResolvePriceBindingPSPParams{MerchantID: mid.UUID(), Rail: rail, PspKey: key, PspID: pspID})
			if err != nil {
				return fmt.Errorf("resolve price binding %q: %w", key, err)
			}
			if len(accounts) != 1 {
				return fmt.Errorf("price binding %q must resolve exactly one PSP account, got %d; provide psp_id", key, len(accounts))
			}
			account := accounts[0]
			if seen[account.ID] {
				return fmt.Errorf("duplicate price binding for PSP %s", account.ID)
			}
			seen[account.ID] = true
			pop := func(key string) *string {
				value := strings.TrimSpace(cfg[key])
				delete(cfg, key)
				if value == "" {
					return nil
				}
				return &value
			}
			params := gen.InsertPricePSPBindingParams{MerchantID: mid.UUID(), CatalogID: catalogID, PriceID: priceID, PspID: account.ID,
				PlanID: pop(models.RailKeyPlanID), PriceRef: pop(models.RailKeyStripePriceID),
				RecurringBillingOptionID: pop(models.RailKeyCCBillRecurringBillingOption), PlanPda: pop("plan_pda"), FlexID: pop(models.RailKeyCCBillFlexID)}
			delete(cfg, models.RailKeyPSPID)
			delete(cfg, models.RailKeyRail)
			params.Configuration, err = models.ToJSONB(cfg)
			if err != nil {
				return err
			}
			if err := q.InsertPricePSPBinding(ctx, params); err != nil {
				return fmt.Errorf("store price binding %q: %w", key, err)
			}
		}
		return nil
	})
}
