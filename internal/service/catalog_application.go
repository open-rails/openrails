package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/catalogscope"
	"github.com/open-rails/openrails/internal/db/gen"
	catalogmodule "github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/apperr"
	catalogwire "github.com/open-rails/openrails/pkg/catalog"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ApplyCatalog applies one durable local operation. It never invokes provider
// network writes; unsupported provider-link changes fail before local mutation.
func (s *Service) ApplyCatalog(ctx context.Context, params openrails.CatalogApplyParams) (*openrails.CatalogApplicationReceipt, error) {
	return s.applyCatalog(ctx, params, s.verifyCatalogProviderReference)
}

func (s *Service) applyCatalog(ctx context.Context, params openrails.CatalogApplyParams, verify catalogReferenceVerifier) (*openrails.CatalogApplicationReceipt, error) {
	if err := params.Validate(); err != nil {
		return nil, apperr.Invalidf("%s", err)
	}
	// The operator path does not cross HTTP. Freeze the same bounded wire
	// representation so nested billing normalization cannot mutate its caller,
	// and explicit nil collections have exactly the remote transport semantics.
	wire, err := json.Marshal(params)
	if err != nil {
		return nil, apperr.Invalidf("%s", err)
	}
	normalized, err := catalogwire.ParseApplicationJSON(wire)
	if err != nil {
		return nil, apperr.Invalidf("%s", err)
	}
	params = *normalized
	if _, owned := catalogscope.FromContext(ctx); owned {
		return nil, catalogmodule.ErrOwnerOperation
	}
	digest, err := params.CanonicalDigest()
	if err != nil {
		return nil, err
	}
	prepared, err := s.prepareCatalogApplication(ctx, params, digest, verify)
	if err != nil {
		return nil, err
	}
	if prepared.replay != nil {
		return prepared.replay, nil
	}
	return catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*openrails.CatalogApplicationReceipt, error) {
		mid, err := merchant.Require(ctx)
		if err != nil {
			return nil, err
		}
		q := scoped.catalogDatabase().Gen(ctx)
		if replay, err := scoped.catalogApplicationReplay(ctx, params, digest); err != nil {
			return nil, err
		} else if replay != nil {
			return replay, nil
		}
		revision, err := q.GetCatalogRevision(ctx, mid.UUID())
		if err != nil {
			return nil, err
		}
		if revision != *params.ExpectedRevision {
			return nil, apperr.New(409, "catalog_revision_conflict", fmt.Sprintf("catalog revision is %d; expected %d", revision, *params.ExpectedRevision))
		}
		if err := scoped.revalidateCatalogApplicationProviders(ctx, prepared); err != nil {
			return nil, err
		}
		// Scope is resolved only for a new operation. A committed replay does not
		// depend on today's rows or provider state.
		if err := q.SetCatalogBatchMerchant(ctx, mid.String()); err != nil {
			return nil, err
		}
		repo := catalogmodule.NewCatalogRepo(scoped.catalogDatabase())
		var target gen.OpenrailsCatalog
		if params.CatalogID == "" {
			target, err = repo.Ensure(ctx, nil)
		} else {
			id, parseErr := openrails.ParseCatalogID(params.CatalogID)
			if parseErr != nil {
				return nil, apperr.Invalidf("invalid catalog_id")
			}
			target, err = repo.Get(ctx, id.UUID())
		}
		if err != nil {
			return nil, productLookup(err)
		}
		scoped.localCatalogOnly = true
		scoped.catalogPreparedLinks = prepared.links
		receipt := &openrails.CatalogApplicationReceipt{ApplicationID: params.ApplicationID, CatalogID: openrails.CatalogID(target.ID).String(), BaseRevision: revision}
		for _, product := range params.Products {
			for _, price := range product.Prices {
				if price.PSPLinks.Set {
					for _, link := range price.PSPLinks.Value {
						if len(link) == 0 {
							return nil, apperr.Invalidf("empty provider links request removal; catalog applications cannot remove bindings")
						}
					}
				}
			}
		}
		if err := scoped.applyCatalogProducts(ctx, target.ID, params, receipt); err != nil {
			return nil, err
		}
		if err := scoped.applyCatalogBilling(ctx, params); err != nil {
			return nil, err
		}
		receipt.AppliedRevision, err = q.AdvanceCatalogRevision(ctx, mid.UUID())
		if err != nil {
			return nil, err
		}
		result, err := json.Marshal(receipt)
		if err != nil {
			return nil, err
		}
		err = q.InsertCatalogApplication(ctx, gen.InsertCatalogApplicationParams{MerchantID: mid.UUID(), ApplicationID: params.ApplicationID, CatalogID: target.ID, SchemaVersion: int64(params.SchemaVersion), RequestSha256: digest[:], BaseRevision: revision, AppliedRevision: receipt.AppliedRevision, Result: result})
		if err != nil {
			return nil, err
		}
		if err := q.SetCatalogBatchMerchant(ctx, ""); err != nil {
			return nil, err
		}
		return receipt, nil
	})
}

func (s *Service) applyCatalogProducts(ctx context.Context, target uuid.UUID, params openrails.CatalogApplyParams, receipt *openrails.CatalogApplicationReceipt) error {
	// Enumerate all pages without public active/tier filtering. The merchant lock
	// makes the stable pagination snapshot safe while the eventual apply mutates it.
	existing := map[string]*CatalogProduct{}
	for offset := 0; ; {
		page, err := s.ListProducts(ctx, ListProductsOptions{CatalogID: &target, Limit: maxCatalogPageSize, Offset: offset})
		if err != nil {
			return err
		}
		for i := range page.Items {
			p := page.Items[i]
			existing[p.Key] = &p
		}
		offset += len(page.Items)
		if int64(offset) >= page.Total {
			break
		}
		if len(page.Items) == 0 {
			return fmt.Errorf("catalog pagination made no progress")
		}
	}
	keep := map[string]bool{}
	for _, decl := range params.Products {
		keep[decl.Key] = true
		p := existing[decl.Key]
		if p == nil {
			// A key may already belong to another catalog in this same merchant.
			foreign, e := s.GetProductByKey(ctx, decl.Key)
			if e == nil && foreign.CatalogID.UUID() != target {
				return ErrCatalogConflict
			}
			if e != nil && !errors.Is(e, openrails.ErrNotFound) {
				return e
			}
			if decl.Archived.Set && decl.Archived.Value && !decl.DisplayName.Set {
				return apperr.Invalidf("cannot archive unknown product %q", decl.Key)
			}
			if !decl.DisplayName.Set || decl.DisplayName.Null {
				return apperr.Invalidf("new product %q requires display_name", decl.Key)
			}
			req := CreateProductRequest{CatalogID: openrails.CatalogID(target), Key: decl.Key, DisplayName: decl.DisplayName.Value, Description: decl.Description.Value, Archived: decl.Archived.Value, TierRank: decl.TierRank.Value, EntitlementsSpec: decl.EntitlementsSpec.Value}
			if decl.TierGroup.Set && !decl.TierGroup.Null {
				req.TierGroup = &decl.TierGroup.Value
			}
			var err error
			p, err = s.CreateProduct(ctx, req)
			if err != nil {
				return err
			}
			receipt.ProductsChanged++
		} else {
			req := UpdateProductRequest{SkipRailSync: true}
			if decl.DisplayName.Set {
				req.DisplayName = &decl.DisplayName.Value
			}
			if decl.Description.Set {
				req.Description = &decl.Description.Value
			}
			if decl.TierRank.Set {
				req.TierRank = &decl.TierRank.Value
			}
			if decl.Archived.Set {
				req.Archived = &decl.Archived.Value
			}
			if decl.EntitlementsSpec.Set {
				req.SetEntitlements = true
				req.EntitlementsSpec = decl.EntitlementsSpec.Value
			}
			if decl.TierGroup.Set {
				req.SetTierGroup = true
				if !decl.TierGroup.Null {
					req.TierGroup = &decl.TierGroup.Value
				}
			}
			if productApplicationChanges(p, req) {
				var err error
				p, err = s.UpdateProduct(ctx, p.ID, req)
				if err != nil {
					return err
				}
				receipt.ProductsChanged++
			}
		}
		if err := s.applyCatalogPrices(ctx, p, decl.Prices, params.Prune, receipt); err != nil {
			return err
		}
	}
	if params.Prune {
		for key, p := range existing {
			if !keep[key] {
				if !p.Archived {
					if _, err := s.DeactivateProduct(ctx, p.ID); err != nil {
						return err
					}
					receipt.ProductsChanged++
				}
				if err := s.applyCatalogPrices(ctx, p, nil, true, receipt); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func productApplicationChanges(p *CatalogProduct, r UpdateProductRequest) bool {
	return r.DisplayName != nil && *r.DisplayName != p.DisplayName || r.Description != nil && *r.Description != p.Description || r.TierRank != nil && *r.TierRank != p.TierRank || r.Archived != nil && *r.Archived != p.Archived || r.SetTierGroup && !reflect.DeepEqual(r.TierGroup, p.TierGroup) || r.SetEntitlements && !reflect.DeepEqual(r.EntitlementsSpec, p.EntitlementsSpec)
}

func (s *Service) applyCatalogPrices(ctx context.Context, product *CatalogProduct, declarations []openrails.CatalogApplyPrice, prune bool, receipt *openrails.CatalogApplicationReceipt) error {
	prices, err := s.ListPricesByProduct(ctx, product.ID, false)
	if err != nil {
		return err
	}
	byKey := map[string][]CatalogPrice{}
	byID := map[string]CatalogPrice{}
	for _, p := range prices {
		byKey[p.Key] = append(byKey[p.Key], p)
		byID[p.ID.String()] = p
	}
	keep := map[string]bool{}
	for _, decl := range declarations {
		keep[decl.Key] = true
		current, req, err := catalogApplicationPriceRequest(product, decl, byKey, byID)
		if err != nil {
			return err
		}
		same := current != nil && current.ID.UUID() == priceDeterministicID(product.ID.UUID(), req.UnitAmount, req.Currency, req.AccessDurationHours, req.AutoRenew, req.TrialUnitAmount, req.TrialDurationHours)
		preparedLinks, ok := s.catalogPreparedLinks[decl.Key]
		if !ok {
			return fmt.Errorf("price %q has no prepared application state", decl.Key)
		}
		if same {
			changed := false
			if !sameCatalogLinks(catalogPriceLinks(current), preparedLinks) {
				prices, err := s.requirePriceService()
				if err != nil {
					return err
				}
				if err := prices.UpdatePSPLinks(ctx, current.ID.UUID(), preparedLinks); err != nil {
					return err
				}
				changed = true
			}
			if current.Archived != req.Archived {
				if req.Archived {
					_, err = s.DeactivatePrice(ctx, current.ID)
				} else {
					_, err = s.ActivatePrice(ctx, current.ID)
				}
				if err != nil {
					return err
				}
				changed = true
			}
			if changed {
				receipt.PricesChanged++
			}
			continue
		}
		expectedID := openrails.PriceID(priceDeterministicID(product.ID.UUID(), req.UnitAmount, req.Currency, req.AccessDurationHours, req.AutoRenew, req.TrialUnitAmount, req.TrialDurationHours))
		if prior, lookupErr := s.GetPrice(ctx, expectedID); lookupErr == nil && prior.Key != decl.Key {
			return ErrCatalogConflict
		} else if lookupErr != nil && !errors.Is(lookupErr, openrails.ErrNotFound) {
			return lookupErr
		}
		out, e := s.CreatePrice(ctx, req)
		if e != nil {
			return e
		}
		receipt.PricesChanged++
		// Subsequent declarations cannot steal an immutable financial row's key.
		if out.Key != decl.Key {
			return ErrCatalogConflict
		}
	}
	if prune {
		for _, p := range prices {
			if !p.Archived && !keep[p.Key] {
				if _, err := s.DeactivatePrice(ctx, p.ID); err != nil {
					return err
				}
				receipt.PricesChanged++
			}
		}
	}
	return nil
}

func (s *Service) applyCatalogBilling(ctx context.Context, params openrails.CatalogApplyParams) error {
	hasCards := false
	for _, p := range params.Products {
		hasCards = hasCards || p.RateCards.Set
	}
	if len(params.Meters) == 0 && !hasCards {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.catalogDatabase().MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		current, err := readCatalogBilling(ctx, tx, mid.UUID())
		if err != nil {
			return err
		}
		desired := current
		meters := map[string]int{}
		for i, m := range desired.Meters {
			meters[m.Key] = i
		}
		for _, m := range params.Meters {
			next := CatalogMeterSpec{Key: m.Key}
			i, exists := meters[m.Key]
			if exists {
				next = desired.Meters[i]
			}
			if m.EventType.Set {
				next.EventType = m.EventType.Value
			}
			if m.ValueProperty.Set {
				next.ValueProperty = m.ValueProperty.Value
			}
			if m.Aggregation.Set {
				next.Aggregation = m.Aggregation.Value
			}
			if m.Unit.Set {
				next.Unit = m.Unit.Value
			}
			if m.GroupBy.Set {
				next.GroupBy = m.GroupBy.Value
			}
			if exists {
				desired.Meters[i] = next
			} else {
				desired.Meters = append(desired.Meters, next)
			}
		}
		for _, p := range params.Products {
			if p.RateCards.Set {
				// Preserve all other products and payer overrides; replace exactly
				// this named product's explicit list after existing dependency checks.
				kept := make([]CatalogRateCardSpec, 0, len(desired.RateCards))
				for _, card := range desired.RateCards {
					if card.ProductKey != p.Key {
						kept = append(kept, card)
					}
				}
				desired.RateCards = kept
				ordinals := map[int]bool{}
				for i, rc := range p.RateCards.Value {
					ordinal := rc.Ordinal
					if ordinal == 0 {
						ordinal = i + 1
					}
					if ordinal < 1 || ordinals[ordinal] {
						return apperr.Invalidf("product %q has invalid or duplicate rate-card ordinal %d", p.Key, ordinal)
					}
					ordinals[ordinal] = true
					price, err := json.Marshal(rc.Price)
					if err != nil {
						return err
					}
					var allowance json.RawMessage
					if rc.Allowance != nil {
						allowance, err = json.Marshal(rc.Allowance)
						if err != nil {
							return err
						}
					}
					desired.RateCards = append(desired.RateCards, CatalogRateCardSpec{ProductKey: p.Key, Ordinal: ordinal, MeterKey: rc.Meter, PaymentTerm: rc.PaymentTerm, Filter: rc.Filter, Allowance: allowance, Price: price})
				}
			}
		}
		return s.SyncCatalogSidecars(ctx, desired, CatalogMutationOptions{Insert: true, Overwrite: true, Prune: true})
	})
}
func catalogApplicationPriceRequest(product *CatalogProduct, decl openrails.CatalogApplyPrice, byKey map[string][]CatalogPrice, byID map[string]CatalogPrice) (current *CatalogPrice, req CreatePriceRequest, err error) {
	if decl.ID != "" {
		p, ok := byID[decl.ID]
		if !ok || p.Key != decl.Key {
			return nil, req, apperr.Invalidf("price id does not select this product/key")
		}
		current = &p
	} else {
		for _, p := range byKey[decl.Key] {
			if !p.Archived {
				copy := p
				current = &copy
				break
			}
		}
		if current == nil && len(byKey[decl.Key]) == 1 {
			p := byKey[decl.Key][0]
			current = &p
		}
		if current == nil && len(byKey[decl.Key]) > 1 {
			if !decl.Currency.Set || !decl.UnitAmount.Set || !decl.AccessDurationHours.Set || !decl.AutoRenew.Set || !decl.TrialUnitAmount.Set || !decl.TrialDurationHours.Set {
				return nil, req, apperr.Invalidf("archived price key %q is ambiguous; select a price id or complete terms", decl.Key)
			}
		}
	}
	req = CreatePriceRequest{ProductID: product.ID, Key: decl.Key}
	if current != nil {
		req.UnitAmount = current.UnitAmount
		req.Currency = current.Currency
		req.AccessDurationHours = current.AccessDurationHours
		req.AutoRenew = current.AutoRenew
		req.TrialUnitAmount = current.TrialUnitAmount
		req.TrialDurationHours = current.TrialDurationHours
		req.Archived = current.Archived
	}
	if current == nil {
		if decl.Archived.Set && decl.Archived.Value && (!decl.Currency.Set || !decl.UnitAmount.Set) {
			return nil, req, apperr.Invalidf("cannot archive unknown price %q", decl.Key)
		}
		if !decl.Currency.Set || !decl.UnitAmount.Set {
			return nil, req, apperr.Invalidf("new price %q requires currency and unit_amount", decl.Key)
		}
		if len(byKey[decl.Key]) > 0 {
			req.Archived = true
		}
	}
	if decl.Currency.Set {
		req.Currency = decl.Currency.Value
	}
	if decl.UnitAmount.Set {
		req.UnitAmount = decl.UnitAmount.Value
	}
	if decl.AccessDurationHours.Set {
		req.AccessDurationHours = nil
		if !decl.AccessDurationHours.Null {
			req.AccessDurationHours = &decl.AccessDurationHours.Value
		}
	}
	if decl.AutoRenew.Set {
		req.AutoRenew = decl.AutoRenew.Value
	}
	if decl.TrialUnitAmount.Set {
		req.TrialUnitAmount = nil
		if !decl.TrialUnitAmount.Null {
			req.TrialUnitAmount = &decl.TrialUnitAmount.Value
		}
	}
	if decl.TrialDurationHours.Set {
		req.TrialDurationHours = nil
		if !decl.TrialDurationHours.Null {
			req.TrialDurationHours = &decl.TrialDurationHours.Value
		}
	}
	if decl.Archived.Set {
		req.Archived = decl.Archived.Value
	}
	if product.Archived && decl.Archived.Set && !decl.Archived.Value {
		return nil, req, apperr.Invalidf("cannot explicitly activate price %q under archived product", decl.Key)
	}
	req.Currency = money.NormalizeCurrency(req.Currency)
	return current, req, nil
}
