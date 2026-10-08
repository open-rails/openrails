package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	catalogwire "github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/catalogpolicy"
	"github.com/open-rails/openrails/internal/catalogrules"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// ApplyCatalog applies one durable local operation. It never invokes provider
// network writes; unsupported provider-link changes fail before local mutation.
func (s *Service) ApplyCatalog(ctx context.Context, params catalogwire.Application) (*billing.CatalogApplicationReceipt, error) {
	return s.applyCatalog(ctx, params, s.verifyCatalogProviderReference)
}

// ErrCatalogProviderUnconfirmed: a provider reference could not be confirmed
// (no answer by the deadline, or its account is not usable yet). Nothing was
// committed; the same application can be retried.
var ErrCatalogProviderUnconfirmed = errors.New("catalog provider reference unconfirmed")

// ApplyDeclaredCatalog applies the host's Config.Catalog startup batch. Provider reference reads end at deadline (zero:
// none); one that fails without a provider refusal is
// ErrCatalogProviderUnconfirmed.
func (s *Service) ApplyDeclaredCatalog(ctx context.Context, params catalogwire.Application, deadline time.Time) (*billing.CatalogApplicationReceipt, error) {
	verify := func(ctx context.Context, provider, rail, account, product string, req billing.CreatePriceParams, link map[string]string) (map[string]string, error) {
		if !deadline.IsZero() {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, deadline)
			defer cancel()
		}
		out, err := s.verifyCatalogProviderReference(ctx, provider, rail, account, product, req, link)
		var refusal *apperr.Error
		if err != nil && !(errors.As(err, &refusal) && refusal.Status < http.StatusInternalServerError) {
			return nil, fmt.Errorf("%w: %w", ErrCatalogProviderUnconfirmed, err)
		}
		return out, err
	}
	return s.applyCatalog(catalogpolicy.OperatorContext(ctx), params, verify)
}

func (s *Service) applyCatalog(ctx context.Context, params catalogwire.Application, verify catalogReferenceVerifier) (*billing.CatalogApplicationReceipt, error) {
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
	for attempt := 1; ; attempt++ {
		prepared, err := s.prepareCatalogApplication(ctx, params, digest, verify)
		if err != nil {
			return nil, err
		}
		if prepared.replay != nil {
			return prepared.replay, nil
		}
		receipt, err := s.commitCatalogApplication(ctx, params, digest, prepared)
		if !errors.Is(err, errCatalogSnapshotMoved) {
			return receipt, err
		}
		if attempt == maxCatalogSnapshotAttempts {
			return nil, apperr.New(409, "catalog_revision_conflict", "catalog kept changing during the declarative application; retry")
		}
	}
}

// A concurrent catalog edit invalidates prepared provider references. Retry
// from a fresh snapshot without changing the batch identity.
var errCatalogSnapshotMoved = errors.New("catalog changed after preparation")

const maxCatalogSnapshotAttempts = 3

func (s *Service) commitCatalogApplication(ctx context.Context, params catalogwire.Application, digest [32]byte, prepared *catalogApplicationPreparation) (*billing.CatalogApplicationReceipt, error) {
	return catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*billing.CatalogApplicationReceipt, error) {
		mid, err := merchant.Require(ctx)
		if err != nil {
			return nil, err
		}
		q := scoped.catalogDatabase().Gen(ctx)
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
		// Scope is resolved only for a new operation. A committed replay does not
		// depend on today's rows or provider state.
		if err := q.SetCatalogBatchMerchant(ctx, mid.String()); err != nil {
			return nil, err
		}
		scoped.localCatalogOnly = true
		scoped.catalogPreparedLinks = prepared.links
		receipt := &billing.CatalogApplicationReceipt{ApplicationID: fmt.Sprintf("sha256:%x", digest), BaseRevision: revision}
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
		if err := scoped.applyCatalogProducts(ctx, params, receipt); err != nil {
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
		err = q.InsertCatalogApplication(ctx, gen.InsertCatalogApplicationParams{MerchantID: mid.UUID(), ApplicationID: receipt.ApplicationID, SchemaVersion: int64(params.SchemaVersion), RequestSha256: digest[:], BaseRevision: revision, AppliedRevision: receipt.AppliedRevision, Result: result})
		if err != nil {
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
	keep := map[string]bool{}
	for _, decl := range params.Products {
		keep[decl.Key] = true
		p := existing[decl.Key]
		if p == nil {
			if decl.Archived.Set && decl.Archived.Value && !decl.DisplayName.Set {
				return apperr.Invalidf("cannot archive unknown product %q", decl.Key)
			}
			if !decl.DisplayName.Set || decl.DisplayName.Null {
				return apperr.Invalidf("new product %q requires display_name", decl.Key)
			}
			req := billing.CreateProductParams{Key: decl.Key, DisplayName: decl.DisplayName.Value, Description: decl.Description.Value, Archived: decl.Archived.Value, TierRank: decl.TierRank.Value, Entitlements: decl.Entitlements.Value}
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
		if err := s.applyCatalogPrices(ctx, p, decl.Prices, params.Prune, receipt); err != nil {
			return err
		}
		if !p.Archived {
			if err := s.validateProductCreditUpdate(ctx, p.ID, p.CreditGrant); err != nil {
				return err
			}
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

func productApplicationChanges(p *billing.Product, r UpdateProductRequest) bool {
	return r.SetCreditGrant && !reflect.DeepEqual(r.CreditGrant, p.CreditGrant) || r.DisplayName != nil && *r.DisplayName != p.DisplayName || r.Description != nil && *r.Description != p.Description || r.TierRank != nil && *r.TierRank != p.TierRank || r.Archived != nil && *r.Archived != p.Archived || r.SetTierGroup && !reflect.DeepEqual(r.TierGroup, p.TierGroup) || r.SetEntitlements && !reflect.DeepEqual(r.Entitlements, p.Entitlements)
}

func (s *Service) applyCatalogPrices(ctx context.Context, product *billing.Product, declarations []catalogwire.ApplyPrice, prune bool, receipt *billing.CatalogApplicationReceipt) error {
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
	keep := map[string]bool{}
	for _, decl := range declarations {
		keep[decl.Key] = true
		current, req, err := catalogApplicationPriceRequest(product, decl, byKey, byID)
		if err != nil {
			return err
		}
		same := current != nil && samePriceTerms(*current, req)
		preparedLinks, ok := s.catalogPreparedLinks[[2]string{product.Key, decl.Key}]
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
		if _, err := s.CreatePrice(ctx, req); err != nil {
			return err
		}
		receipt.PricesChanged++
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
					if err := catalogrules.ValidateRateCard(fmt.Sprintf("product %q rate card #%d", p.Key, i+1), &rc); err != nil {
						return apperr.Invalidf("%s", err.Error())
					}
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
					desired.RateCards = append(desired.RateCards, CatalogRateCardSpec{ProductKey: p.Key, Ordinal: ordinal, MeterKey: rc.Meter, PaymentTerm: string(rc.PaymentTerm), Filter: rc.Filter, Allowance: allowance, Price: price})
				}
			}
		}
		return s.SyncCatalogSidecars(ctx, desired, CatalogMutationOptions{Insert: true, Overwrite: true, Prune: true})
	})
}
func catalogApplicationPriceRequest(product *billing.Product, decl catalogwire.ApplyPrice, byKey map[string][]billing.Price, byID map[string]billing.Price) (current *billing.Price, req billing.CreatePriceParams, err error) {
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
			for _, prior := range byKey[decl.Key] {
				if prior.CustomerAmount != nil && !decl.CustomerAmount.Set {
					return nil, req, apperr.Invalidf("archived deposit key %q is ambiguous; select a price id or include customer_amount in complete terms", decl.Key)
				}
			}
			if !decl.Currency.Set || !decl.UnitAmount.Set || !decl.AccessDurationHours.Set || !decl.BillingIntervalHours.Set || !decl.TrialUnitAmount.Set || !decl.TrialDurationHours.Set {
				return nil, req, apperr.Invalidf("archived price key %q is ambiguous; select a price id or complete terms", decl.Key)
			}
		}
	}
	req = billing.CreatePriceParams{ProductID: product.ID, Key: decl.Key}
	if current != nil {
		req.CustomerAmount = current.CustomerAmount
		req.UnitAmount = current.UnitAmount
		req.Currency = current.Currency
		req.AccessDurationHours = current.AccessDurationHours
		req.BillingIntervalHours = current.BillingIntervalHours
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
	if decl.CustomerAmount.Set {
		req.CustomerAmount = nil
		if !decl.CustomerAmount.Null {
			req.CustomerAmount = &decl.CustomerAmount.Value
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
	if product.Archived && decl.Archived.Set && !decl.Archived.Value {
		return nil, req, apperr.Invalidf("cannot explicitly activate price %q under archived product", decl.Key)
	}
	req.Currency = money.NormalizeCurrency(req.Currency)
	if decl.ID != "" && !samePriceTerms(*current, req) {
		return nil, req, apperr.Invalidf("price id pins immutable terms; omit id to create a new price revision")
	}
	// A rollback or archived declaration may select a historical financial
	// version. Preserve that version's own provider bindings, not the currently
	// active version's links, when links were omitted.
	for _, candidate := range byKey[decl.Key] {
		if samePriceTerms(candidate, req) {
			copy := candidate
			current = &copy
			break
		}
	}
	return current, req, nil
}
