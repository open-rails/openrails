package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	catalogwire "github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/catalogrules"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// ApplyCatalog applies a catalog document as the apply manager. Nothing is
// written to a provider's catalog: provider prices are read from their
// provider, and the only provider write publishes the Solana plan a recurring
// price of ours sells on, before local mutation. An object whose named field
// an edit set differently is skipped and reported unless params.Force.
func (s *Service) ApplyCatalog(ctx context.Context, document catalogwire.Application, params billing.ApplyCatalogParams) (*billing.CatalogApplicationReceipt, error) {
	return s.applyCatalog(ctx, document, params.Force, "")
}

// ApplyOperatorCatalog applies a catalog file for trusted operator tooling,
// which carries no verified identity. Its substantive changes are admitted on
// the merchant's autonomous budget.
func (s *Service) ApplyOperatorCatalog(ctx context.Context, document catalogwire.Application, params billing.ApplyCatalogParams) (*billing.CatalogApplicationReceipt, error) {
	return s.applyCatalog(ctx, document, params.Force, "operator_catalog")
}

func (s *Service) applyCatalog(ctx context.Context, params catalogwire.Application, force bool, origin string) (*billing.CatalogApplicationReceipt, error) {
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
	digest, err := params.CanonicalDigest()
	if err != nil {
		return nil, err
	}
	if digest, err = s.treasuryDigest(ctx, digest); err != nil {
		return nil, err
	}
	for attempt := 1; ; attempt++ {
		prepared, err := s.prepareCatalogApplication(ctx, params, digest)
		if err != nil {
			return nil, err
		}
		if prepared.replay != nil {
			return prepared.replay, nil
		}
		receipt, err := catalogCommandOnce(ctx, s, "catalog/application", params, origin, applicationAdmissionPlan(params, prepared, force), func(ctx context.Context, s *Service) (*billing.CatalogApplicationReceipt, error) {
			return s.commitCatalogApplication(ctx, params, digest, prepared, force)
		})
		if !errors.Is(err, errCatalogSnapshotMoved) {
			return receipt, err
		}
		if attempt == maxCatalogSnapshotAttempts {
			return nil, apperr.New(409, "catalog_revision_conflict", "catalog kept changing during the declarative application; retry")
		}
	}
}

// treasuryDigest folds in the merchant's declared Solana PSPs: a plan pays the
// treasury its PSP names, so the same document under another treasury is
// another application, which binds its recurring prices to new plans.
func (s *Service) treasuryDigest(ctx context.Context, digest [32]byte) ([32]byte, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return digest, err
	}
	accounts, err := merchants.Of(s.catalogDatabase()).PSPScopes(ctx, mid)
	if err != nil {
		return digest, err
	}
	var ids []string
	for _, a := range accounts {
		if a.Rail == string(models.RailSolana) && a.Declared {
			ids = append(ids, a.ID.String())
		}
	}
	if len(ids) == 0 {
		return digest, nil
	}
	slices.Sort(ids)
	return sha256.Sum256(append(digest[:], strings.Join(ids, ",")...)), nil
}

// A concurrent catalog edit invalidates prepared provider references. Retry
// from a fresh snapshot without changing the batch identity.
var errCatalogSnapshotMoved = errors.New("catalog changed after preparation")

const maxCatalogSnapshotAttempts = 3

// commitCatalogApplication applies every object of the document that no edit
// contests, skips the rest whole, and records the document as applied only
// when it skipped nothing.
func (s *Service) commitCatalogApplication(ctx context.Context, params catalogwire.Application, digest [32]byte, prepared *catalogApplicationPreparation, force bool) (*billing.CatalogApplicationReceipt, error) {
	return catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*billing.CatalogApplicationReceipt, error) {
		scoped.catalogApplying = true
		mid, err := merchant.Require(ctx)
		if err != nil {
			return nil, err
		}
		tx := scoped.catalogDatabase()
		q := tx.Gen(ctx)
		revision, replay, err := scoped.catalogApplicationGate(ctx, digest)
		if err != nil || replay != nil {
			return replay, err
		}
		// Prepared links and accounts describe the snapshot at that revision.
		if revision != prepared.revision {
			return nil, errCatalogSnapshotMoved
		}
		if err := scoped.revalidateCatalogApplicationProviders(ctx, prepared); err != nil {
			return nil, err
		}
		owners, err := readCatalogOwners(ctx, q, mid.UUID())
		if err != nil {
			return nil, err
		}
		plan, err := planCatalogApplication(withoutRefused(params, prepared.refused), scoped.catalogBefore, owners, prepared.providers, force)
		if err != nil {
			return nil, err
		}
		// Scope is resolved only for a new operation. A committed replay does not
		// depend on today's rows or provider state.
		if err := q.SetCatalogBatchMerchant(ctx, mid.String()); err != nil {
			return nil, err
		}
		scoped.localCatalogOnly = true
		scoped.catalogPreparedLinks = prepared.links
		scoped.catalogPreparedProviders = prepared.providers
		receipt := &billing.CatalogApplicationReceipt{ApplicationID: fmt.Sprintf("sha256:%x", digest), BaseRevision: revision,
			EntitlementChanges: []billing.EntitlementChange{}, Changes: []billing.CatalogChange{}, Conflicts: plan.conflicts, Warnings: prepared.warnings}
		scoped.catalogKeyActor = receipt.ApplicationID
		scoped.catalogKeyChanges = &receipt.EntitlementChanges
		if err := scoped.replaceEntitlements(ctx, plan.document.EntitlementReplacements, receipt); err != nil {
			return nil, err
		}
		if err := scoped.applyCatalogProducts(ctx, plan.document, receipt); err != nil {
			return nil, err
		}
		if err := scoped.applyCatalogBilling(ctx, plan.document); err != nil {
			return nil, err
		}
		if err := scoped.pruneCatalogObjects(ctx, plan.prune); err != nil {
			return nil, err
		}
		after, err := readCatalogState(ctx, q, mid.UUID())
		if err != nil {
			return nil, err
		}
		changes, err := scoped.recordCatalogApply(ctx, q, mid.UUID(), scoped.catalogBefore, after, plan, receipt.ApplicationID)
		if err != nil {
			return nil, err
		}
		receipt.Changes = changes
		receipt.ProductsChanged, receipt.PricesChanged = 0, 0
		for _, change := range changes {
			switch change.Object {
			case billing.CatalogObjectProduct:
				receipt.ProductsChanged++
			case billing.CatalogObjectPrice:
				receipt.PricesChanged++
			}
		}
		recorded := len(plan.conflicts) == 0 && len(prepared.warnings) == 0
		receipt.AppliedRevision = revision
		if recorded || len(changes) > 0 {
			if receipt.AppliedRevision, err = q.AdvanceCatalogRevision(ctx, mid.UUID()); err != nil {
				return nil, err
			}
		}
		if recorded {
			if err := scoped.recordCatalogReceipt(ctx, receipt, params.SchemaVersion, digest); err != nil {
				return nil, err
			}
		}
		if err := scoped.reportCatalogConflicts(ctx, receipt); err != nil {
			return nil, err
		}
		if err := q.SetCatalogBatchMerchant(ctx, ""); err != nil {
			return nil, err
		}
		return receipt, nil
	})
}

func (s *Service) applyCatalogProducts(ctx context.Context, params catalogwire.Application, receipt *billing.CatalogApplicationReceipt) error {
	// Enumerate all pages without public active/tier filtering. The merchant lock
	// makes the stable pagination snapshot safe while the eventual apply mutates it.
	existing := map[string]*billing.Product{}
	for cursor := ""; ; {
		page, err := s.ListProducts(ctx, billing.ProductListParams{PageRequest: billing.PageRequest{Limit: billing.MaxPageLimit, Cursor: cursor}})
		if err != nil {
			return err
		}
		for i := range page.Items {
			p := page.Items[i]
			existing[p.Key] = &p
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	for _, key := range slices.Sorted(maps.Keys(params.Products)) {
		decl := params.Products[key]
		p := existing[key]
		if p == nil {
			if decl.Archived.Set && decl.Archived.Value && !decl.DisplayName.Set {
				return apperr.Invalidf("cannot archive unknown product %q", key)
			}
			if !decl.DisplayName.Set || decl.DisplayName.Null {
				return apperr.Invalidf("new product %q requires display_name", key)
			}
			req := billing.CreateProductParams{Key: key, DisplayName: decl.DisplayName.Value, Description: decl.Description.Value, Archived: decl.Archived.Value, TierRank: decl.TierRank.Value, Ownership: decl.Ownership.Value, Entitlements: decl.Entitlements.Value, RequiredEntitlements: decl.RequiredEntitlements.Value}
			if decl.CreditGrant.Set && !decl.CreditGrant.Null {
				req.CreditGrant = &decl.CreditGrant.Value
			}
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
			req := UpdateProductRequest{SkipRailSync: true, DeferCreditPriceValidation: true}
			if decl.DisplayName.Set {
				req.DisplayName = &decl.DisplayName.Value
			}
			if decl.Description.Set {
				req.Description = &decl.Description.Value
			}
			if decl.TierRank.Set {
				req.TierRank = &decl.TierRank.Value
			}
			if decl.Ownership.Set {
				req.SetOwnership, req.Ownership = true, decl.Ownership.Value
			}
			if decl.Archived.Set {
				req.Archived = &decl.Archived.Value
			}
			if decl.Entitlements.Set {
				req.SetEntitlements = true
				var err error
				req.Entitlements, err = catalogwire.NormalizeEntitlements(decl.Entitlements.Value)
				if err != nil {
					return err
				}
			}
			if decl.RequiredEntitlements.Set {
				req.SetRequiredEntitlements = true
				var err error
				req.RequiredEntitlements, err = catalogwire.NormalizeRequiredEntitlements(decl.RequiredEntitlements.Value)
				if err != nil {
					return err
				}
			}
			if decl.CreditGrant.Set {
				req.SetCreditGrant = true
				if !decl.CreditGrant.Null {
					req.CreditGrant = &decl.CreditGrant.Value
				}
			}
			if decl.TierGroup.Set {
				req.SetTierGroup = true
				if !decl.TierGroup.Null {
					req.TierGroup = &decl.TierGroup.Value
				}
			}
			if productApplicationChanges(p, req) {
				var err error
				p, err = s.patchProduct(ctx, p.ID, req)
				if err != nil {
					return err
				}
				receipt.ProductsChanged++
			}
		}
		if err := s.applyCatalogPrices(ctx, p, decl.Prices, receipt); err != nil {
			return err
		}
		if err := s.applyCatalogProviderPrices(ctx, p, decl.ProviderPrices, receipt); err != nil {
			return err
		}
		if !p.Archived {
			if err := s.validateProductCreditUpdate(ctx, p.ID, p.CreditGrant); err != nil {
				return err
			}
		}
	}
	return nil
}

func declaredOwnership(p *billing.Product) catalogwire.Ownership {
	if p.Ownership == nil {
		return ""
	}
	return *p.Ownership
}

func productApplicationChanges(p *billing.Product, r UpdateProductRequest) bool {
	return r.SetCreditGrant && !reflect.DeepEqual(r.CreditGrant, p.CreditGrant) || r.DisplayName != nil && *r.DisplayName != p.DisplayName || r.Description != nil && *r.Description != p.Description || r.TierRank != nil && *r.TierRank != p.TierRank || r.SetOwnership && r.Ownership != declaredOwnership(p) || r.Archived != nil && *r.Archived != p.Archived || r.SetTierGroup && !reflect.DeepEqual(r.TierGroup, p.TierGroup) || r.SetEntitlements && !reflect.DeepEqual(r.Entitlements, p.Entitlements) || r.SetRequiredEntitlements && !slices.Equal(r.RequiredEntitlements, p.RequiredEntitlements)
}

func (s *Service) applyCatalogPrices(ctx context.Context, product *billing.Product, declarations map[string]catalogwire.ApplyPrice, receipt *billing.CatalogApplicationReceipt) error {
	prices, err := s.ListPricesByProduct(ctx, product.ID, false)
	if err != nil {
		return err
	}
	byKey := map[string][]billing.Price{}
	byID := map[string]billing.Price{}
	for _, p := range prices {
		byKey[p.Key] = append(byKey[p.Key], p)
		byID[p.ID.String()] = p
	}
	for _, key := range slices.Sorted(maps.Keys(declarations)) {
		current, req, err := catalogApplicationPriceRequest(product, key, declarations[key], byKey, byID)
		if err != nil {
			return err
		}
		same := current != nil && samePriceTerms(*current, req)
		preparedLinks, ok := s.catalogPreparedLinks[[2]string{product.Key, key}]
		if !ok {
			return fmt.Errorf("price %q has no prepared application state", key)
		}
		if same {
			prices, err := s.requirePriceService()
			if err != nil {
				return err
			}
			stored, err := prices.GetByPublicID(ctx, current.ID)
			if err != nil {
				return err
			}
			changed := false
			if !sameCatalogLinks(stored.PSPLinks, preparedLinks) {
				if err := prices.UpdatePSPLinks(ctx, stored.ID, preparedLinks); err != nil {
					return err
				}
				changed = true
			}
			if !slices.Equal(stored.SellVia, req.SellVia) {
				if err := prices.SetSellVia(ctx, stored.ID, req.SellVia); err != nil {
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
		if _, err := s.CreatePrice(ctx, req); err != nil {
			return err
		}
		receipt.PricesChanged++
	}
	return nil
}

// applyCatalogProviderPrices writes the prepared provider prices of a product.
func (s *Service) applyCatalogProviderPrices(ctx context.Context, product *billing.Product, declarations map[string]catalogwire.ApplyProviderPrice, receipt *billing.CatalogApplicationReceipt) error {
	if len(declarations) == 0 {
		return nil
	}
	products, err := s.requireProductService()
	if err != nil {
		return err
	}
	stored, err := products.GetByID(ctx, product.ID.UUID())
	if err != nil {
		return productLookup(err)
	}
	for _, key := range slices.Sorted(maps.Keys(declarations)) {
		prepared, ok := s.catalogPreparedProviders[[2]string{product.Key, key}]
		if !ok {
			return fmt.Errorf("provider price %q has no prepared application state", key)
		}
		_, changed, err := s.writeProviderPrice(ctx, stored, prepared)
		if err != nil {
			return err
		}
		if changed {
			receipt.PricesChanged++
		}
	}
	return nil
}

// withoutRefused is the document without the prices preparation refused.
func withoutRefused(params catalogwire.Application, refused map[[2]string]bool) catalogwire.Application {
	if len(refused) == 0 {
		return params
	}
	products := make(map[string]catalogwire.ApplyProduct, len(params.Products))
	for key, product := range params.Products {
		prices, providers := maps.Clone(product.Prices), maps.Clone(product.ProviderPrices)
		for ref := range refused {
			if ref[0] == key {
				delete(prices, ref[1])
				delete(providers, ref[1])
			}
		}
		product.Prices, product.ProviderPrices = prices, providers
		products[key] = product
	}
	params.Products = products
	return params
}

func (s *Service) applyCatalogBilling(ctx context.Context, params catalogwire.Application) error {
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
		for _, key := range slices.Sorted(maps.Keys(params.Meters)) {
			m := params.Meters[key]
			next := CatalogMeterSpec{Key: key}
			i, exists := meters[key]
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
		for _, key := range slices.Sorted(maps.Keys(params.Products)) {
			if p := params.Products[key]; p.RateCards.Set {
				// Preserve all other products and payer overrides; replace exactly
				// this named product's explicit list after existing dependency checks.
				kept := make([]CatalogRateCardSpec, 0, len(desired.RateCards))
				for _, card := range desired.RateCards {
					if card.ProductKey != key {
						kept = append(kept, card)
					}
				}
				desired.RateCards = kept
				ordinals := map[int]bool{}
				for i, rc := range p.RateCards.Value {
					if err := catalogrules.ValidateRateCard(fmt.Sprintf("product %q rate card #%d", key, i+1), &rc); err != nil {
						return apperr.Invalidf("%s", err.Error())
					}
					ordinal := rc.Ordinal
					if ordinal == 0 {
						ordinal = i + 1
					}
					if ordinal < 1 || ordinals[ordinal] {
						return apperr.Invalidf("product %q has invalid or duplicate rate-card ordinal %d", key, ordinal)
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
					desired.RateCards = append(desired.RateCards, CatalogRateCardSpec{ProductKey: key, Ordinal: ordinal, MeterKey: rc.Meter, PaymentTerm: string(rc.PaymentTerm), Filter: rc.Filter, Allowance: allowance, Price: price})
				}
			}
		}
		return s.SyncCatalogSidecars(ctx, desired, CatalogMutationOptions{Insert: true, Overwrite: true, Prune: true})
	})
}
func catalogApplicationPriceRequest(product *billing.Product, key string, decl catalogwire.ApplyPrice, byKey map[string][]billing.Price, byID map[string]billing.Price) (current *billing.Price, req billing.CreatePriceParams, err error) {
	if decl.ID != "" {
		p, ok := byID[decl.ID]
		if !ok || p.Key != key {
			return nil, req, apperr.Invalidf("price id does not select this product/key")
		}
		current = &p
	} else {
		for _, p := range byKey[key] {
			if !p.Archived {
				copy := p
				current = &copy
				break
			}
		}
		if current == nil && len(byKey[key]) == 1 {
			p := byKey[key][0]
			current = &p
		}
		if current == nil && len(byKey[key]) > 1 {
			for _, prior := range byKey[key] {
				if prior.CustomerAmount != nil && !decl.CustomerAmount.Set {
					return nil, req, apperr.Invalidf("archived deposit key %q is ambiguous; select a price id or include customer_amount in complete terms", key)
				}
			}
			if !decl.Currency.Set || !decl.UnitAmount.Set || !decl.AccessDurationHours.Set || !decl.BillingIntervalHours.Set || !decl.TrialUnitAmount.Set || !decl.TrialDurationHours.Set {
				return nil, req, apperr.Invalidf("archived price key %q is ambiguous; select a price id or complete terms", key)
			}
		}
	}
	req = billing.CreatePriceParams{ProductID: product.ID, Key: key}
	if current != nil {
		req.CustomerAmount = current.CustomerAmount
		req.Quantity = current.Quantity
		req.UnitAmount = current.UnitAmount
		req.Currency = current.Currency
		req.AccessDurationHours = current.AccessDurationHours
		req.BillingIntervalHours = current.BillingIntervalHours
		req.TrialUnitAmount = current.TrialUnitAmount
		req.TrialDurationHours = current.TrialDurationHours
		req.Archived = current.Archived
		req.SellVia = current.SellVia
	}
	if current == nil {
		if decl.Archived.Set && decl.Archived.Value && (!decl.Currency.Set || !decl.UnitAmount.Set) {
			return nil, req, apperr.Invalidf("cannot archive unknown price %q", key)
		}
		if !decl.Currency.Set || !decl.UnitAmount.Set {
			return nil, req, apperr.Invalidf("new price %q requires currency and unit_amount", key)
		}
		if len(byKey[key]) > 0 {
			req.Archived = true
		}
	}
	if decl.CustomerAmount.Set {
		req.CustomerAmount = nil
		if !decl.CustomerAmount.Null {
			req.CustomerAmount = &decl.CustomerAmount.Value
		}
	}
	if decl.Quantity.Set {
		req.Quantity = nil
		if !decl.Quantity.Null {
			req.Quantity = &decl.Quantity.Value
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
	if decl.BillingIntervalHours.Set {
		req.BillingIntervalHours = nil
		if !decl.BillingIntervalHours.Null {
			req.BillingIntervalHours = &decl.BillingIntervalHours.Value
		}
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
	if decl.SellVia.Set {
		req.SellVia = decl.SellVia.Value
	}
	if product.Archived && decl.Archived.Set && !decl.Archived.Value {
		return nil, req, apperr.Invalidf("cannot explicitly activate price %q under archived product", key)
	}
	req.Currency = money.NormalizeCurrency(req.Currency)
	if decl.ID != "" && !samePriceTerms(*current, req) {
		return nil, req, apperr.Invalidf("price id pins immutable terms; omit id to create a new price revision")
	}
	// A rollback or archived declaration may select a historical financial
	// version: it keeps that version's own Solana plan.
	for _, candidate := range byKey[key] {
		if samePriceTerms(candidate, req) {
			copy := candidate
			current = &copy
			break
		}
	}
	return current, req, nil
}
