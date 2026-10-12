package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/catalogrules"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	catalogwire "github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/merchant"
)

type catalogAdmissionKey struct{}
type catalogAdmission struct {
	before     catalogState
	targets    map[catalogObject]bool
	action     *destructive.Action
	rolledBack bool
	finished   bool
	external   bool
}

type catalogClassification func(context.Context, *Service, catalogState) ([]catalogObject, error)

// catalogCommand plans under a read lock, releases it, and admits the whole
// affected set before a provider call or a catalog transaction. The commit
// rechecks the snapshot and its actual changed targets; nested effects reuse
// this admission instead of spending another unit.
func catalogCommand[T any](ctx context.Context, s *Service, target string, terms any, origin string, external bool, plan catalogClassification, write func(context.Context, *Service) (T, error)) (out T, err error) {
	if _, ok := ctx.Value(catalogAdmissionKey{}).(*catalogAdmission); ok {
		return write(ctx, s)
	}
	if s.catalogWriteLocked {
		return out, errors.New("catalog write has no outer command admission")
	}
	scope, err := catalogReadLocked(ctx, s, func(ctx context.Context, read *Service) (*catalogAdmission, error) {
		mid, err := merchant.Require(ctx)
		if err != nil {
			return nil, err
		}
		before, err := readCatalogState(ctx, read.catalogDatabase().Gen(ctx), mid.UUID())
		if err != nil {
			return nil, err
		}
		objects, err := plan(ctx, read, before)
		if err != nil {
			return nil, err
		}
		targets := make(map[catalogObject]bool, len(objects))
		for _, obj := range objects {
			targets[obj] = true
		}
		return &catalogAdmission{before: before, targets: targets, external: external}, nil
	})
	if err != nil {
		return out, err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return out, err
	}
	if len(scope.targets) > 0 {
		admissions := s.rt.DestructiveAdmissions
		if admissions == nil {
			return out, errors.New("catalog destructive admission is not wired")
		}
		digest := destructive.Digest(terms)
		revisions := make([]any, 0, len(scope.targets))
		for _, obj := range slices.SortedFunc(maps.Keys(scope.targets), compareCatalogObjects) {
			var revision int64
			if state := scope.before[obj]; state != nil {
				revision = state.revision
			}
			revisions = append(revisions, struct {
				Target   string
				Revision int64
			}{obj.String(), revision})
		}
		command := destructive.Command{MerchantID: mid.UUID(), Kind: destructive.CatalogChange, Target: target,
			Key: "catalog:" + destructive.Digest([]any{target, digest, revisions}), Digest: digest, Units: int64(len(scope.targets))}
		var action destructive.Action
		if origin != "" {
			action, err = admissions.AdmitAutonomous(ctx, command, origin)
		} else {
			command.Actor, err = destructive.ActorFromContext(ctx)
			if err != nil {
				return out, err
			}
			action, err = admissions.Admit(ctx, command)
		}
		if err != nil {
			return out, err
		}
		scope.action = &action
		if _, err = admissions.Start(ctx, mid.UUID(), action.ID); err != nil {
			return out, err
		}
	}
	ctx = context.WithValue(ctx, catalogAdmissionKey{}, scope)
	out, err = write(ctx, s)
	if scope.action != nil && !scope.finished {
		finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		outcome := destructive.OutcomeUncertain
		if scope.rolledBack && !scope.external {
			outcome = destructive.OutcomeNoEffect
		}
		err = errors.Join(err, s.rt.DestructiveAdmissions.Finish(finish, mid.UUID(), scope.action.ID, outcome, ""))
	}
	return out, err
}

func substantiveCatalogField(kind billing.CatalogObjectKind, field string) bool {
	return kind != billing.CatalogObjectProduct || field != "display_name" && field != "description"
}

func substantiveCatalogTargets(before, after catalogState) []catalogObject {
	changed := map[catalogObject]bool{}
	for obj, old := range before {
		next := after[obj]
		if next == nil {
			changed[obj] = true
			continue
		}
		for _, field := range catalogFields[obj.kind] {
			if substantiveCatalogField(obj.kind, field) && !bytes.Equal(old.fields[field], next.fields[field]) {
				changed[obj] = true
				break
			}
		}
		// A meter's hidden rate-card projection is the product's rate_cards
		// field, already counted once under that owning product.
	}
	for obj := range after {
		if before[obj] == nil {
			changed[obj] = true
		}
	}
	return slices.SortedFunc(maps.Keys(changed), compareCatalogObjects)
}

func catalogObjectWithProductID(state catalogState, id billing.ProductID) (catalogObject, *catalogObjectState, error) {
	for obj, value := range state {
		if obj.kind == billing.CatalogObjectProduct && value.productID == id.UUID() {
			return obj, value, nil
		}
	}
	return catalogObject{}, nil, ErrProductNotFound
}

func catalogFieldsChanged(obj catalogObject, old *catalogObjectState, fields map[string]json.RawMessage) bool {
	if old == nil {
		return true
	}
	for field, value := range fields {
		if substantiveCatalogField(obj.kind, field) && !bytes.Equal(old.fields[field], value) {
			return true
		}
	}
	return false
}

func productEditPlan(id billing.ProductID, req UpdateProductRequest) catalogClassification {
	return func(_ context.Context, _ *Service, state catalogState) ([]catalogObject, error) {
		obj, old, err := catalogObjectWithProductID(state, id)
		if err != nil {
			return nil, err
		}
		if err = checkRevision(obj.String(), req.ExpectedRevision, old.revision); err != nil {
			return nil, err
		}
		fields := map[string]json.RawMessage{}
		if req.SetCreditGrant {
			fields["credit_grant"] = catalogJSON(req.CreditGrant)
		}
		if req.SetEntitlements {
			fields["entitlements"] = catalogJSON(req.Entitlements)
		}
		if req.SetTierGroup {
			fields["tier_group"] = catalogOptionalText(req.TierGroup)
		}
		if req.TierRank != nil {
			fields["tier_rank"] = catalogJSON(*req.TierRank)
		}
		if req.SetOwnership {
			var value *string
			if req.Ownership != "" {
				v := string(req.Ownership)
				value = &v
			}
			fields["ownership"] = catalogOptionalText(value)
		}
		if req.Archived != nil {
			fields["archived"] = catalogJSON(*req.Archived)
		}
		if catalogFieldsChanged(obj, old, fields) {
			return []catalogObject{obj}, nil
		}
		return nil, nil
	}
}

func sameCatalogSnapshot(a, b catalogState) bool { return reflect.DeepEqual(a, b) }

func applicationAdmissionPlan(document catalogwire.Application, prepared *catalogApplicationPreparation, force bool) catalogClassification {
	return func(ctx context.Context, s *Service, state catalogState) ([]catalogObject, error) {
		mid, err := merchant.Require(ctx)
		if err != nil {
			return nil, err
		}
		owners, err := readCatalogOwners(ctx, s.catalogDatabase().Gen(ctx), mid.UUID())
		if err != nil {
			return nil, err
		}
		plan, err := planCatalogApplication(document, state, owners, prepared.links, force)
		if err != nil {
			return nil, err
		}
		targets := map[catalogObject]bool{}
		for key, product := range plan.document.Products {
			fields, err := documentProductFields(key, product)
			if err != nil {
				return nil, err
			}
			obj := productObject(key)
			if len(fields) > 0 && catalogFieldsChanged(obj, state[obj], fields) {
				targets[obj] = true
			}

			for name, price := range product.Prices {
				obj := priceObject(key, name)
				if catalogFieldsChanged(obj, state[obj], documentPriceFields(price, prepared.links[[2]string{key, name}])) {
					targets[obj] = true
				}
			}
		}
		for key, meter := range plan.document.Meters {
			obj := meterObject(key)
			if catalogFieldsChanged(obj, state[obj], documentMeterFields(key, meter)) {
				targets[obj] = true
			}
		}
		for obj := range plan.renamed {
			targets[obj] = true
		}
		for _, obj := range plan.prune {
			targets[obj] = true
		}
		return slices.SortedFunc(maps.Keys(targets), compareCatalogObjects), nil
	}
}

func createProductPlan(req billing.CreateProductParams) catalogClassification {
	return func(_ context.Context, _ *Service, state catalogState) ([]catalogObject, error) {
		key := strings.TrimSpace(req.Key)
		if key == "" || strings.TrimSpace(req.DisplayName) == "" {
			return nil, apperr.Invalidf("key and display_name required")
		}
		if _, err := normalizeCreditGrant(req.CreditGrant); err != nil {
			return nil, err
		}
		if _, err := catalogwire.NormalizeEntitlements(req.Entitlements); err != nil {
			return nil, err
		}
		obj := productObject(key)
		if state[obj] != nil {
			return nil, nil
		}
		return []catalogObject{obj}, nil
	}
}

func deleteProductPlan(id billing.ProductID) catalogClassification {
	return func(ctx context.Context, s *Service, state catalogState) ([]catalogObject, error) {
		obj, _, err := catalogObjectWithProductID(state, id)
		if err != nil {
			return nil, err
		}
		mid, err := merchant.Require(ctx)
		if err != nil {
			return nil, err
		}
		q := s.catalogDatabase().Gen(ctx)
		used, err := q.ProductHoldsReferences(ctx, gen.ProductHoldsReferencesParams{MerchantID: mid.UUID(), ProductID: id.UUID()})
		if err != nil {
			return nil, err
		}
		ids, err := q.ListProductPriceIDs(ctx, gen.ListProductPriceIDsParams{MerchantID: mid.UUID(), ProductID: id.UUID()})
		if err != nil {
			return nil, err
		}
		if !used && len(ids) > 0 {
			used, err = q.PricesHoldReferences(ctx, gen.PricesHoldReferencesParams{MerchantID: mid.UUID(), PriceIds: ids})
			if err != nil {
				return nil, err
			}
		}
		if used {
			return nil, ErrProductInUse
		}
		out := []catalogObject{obj}
		for child := range state {
			if child.kind == billing.CatalogObjectPrice && child.key == obj.key {
				out = append(out, child)
			}
		}
		return out, nil
	}
}

func priceEditPlan(id billing.PriceID, req UpdatePriceRequest) catalogClassification {
	return func(ctx context.Context, s *Service, state catalogState) ([]catalogObject, error) {
		prices, err := s.requirePriceService()
		if err != nil {
			return nil, err
		}
		current, err := prices.GetByPublicID(ctx, id)
		if err != nil {
			return nil, priceLookup(err)
		}
		product, _, err := catalogObjectWithProductID(state, billing.ProductID(current.ProductID))
		if err != nil {
			return nil, err
		}
		obj := priceObject(product.key, current.Key)
		if err = checkRevision(obj.String(), req.ExpectedRevision, current.KeyRevision); err != nil {
			return nil, err
		}
		changed := req.Archived != nil && *req.Archived != current.Archived
		for key, link := range req.PSPLinks {
			key = strings.ToLower(strings.TrimSpace(key))
			link = normalizeLinkMap(link)
			if len(link) == 0 {
				changed = changed || len(current.PSPLinks[key]) > 0
			} else {
				changed = changed || !catalogLinkContains(current.PSPLinks[key], link)
			}
		}
		if changed {
			return []catalogObject{obj}, nil
		}
		return nil, nil
	}
}

func createPricePlan(req billing.CreatePriceParams) catalogClassification {
	return func(ctx context.Context, s *Service, state catalogState) ([]catalogObject, error) {
		req.Currency = money.NormalizeCurrency(req.Currency)
		if err := validateCatalogPriceTerms(req); err != nil {
			return nil, err
		}
		selectors := 0
		if !req.ProductID.IsZero() {
			selectors++
		}
		if req.ProductKey != "" {
			selectors++
		}
		if req.ProductData != nil {
			selectors++
		}
		if selectors != 1 {
			return nil, apperr.Invalidf("exactly one product selector is required")
		}
		productKey := strings.TrimSpace(req.ProductKey)
		targets := []catalogObject{}
		if req.ProductData != nil {
			productKey = strings.TrimSpace(req.ProductData.Key)
			if productKey == "" || strings.TrimSpace(req.ProductData.DisplayName) == "" {
				return nil, apperr.Invalidf("product_data requires key and display_name")
			}
			if state[productObject(productKey)] == nil {
				targets = append(targets, productObject(productKey))
			}
		} else if !req.ProductID.IsZero() {
			obj, _, err := catalogObjectWithProductID(state, req.ProductID)
			if err != nil {
				return nil, err
			}
			productKey = obj.key
		}
		product := state[productObject(productKey)]
		if product == nil && req.ProductData == nil {
			return nil, ErrProductNotFound
		}
		key, _ := resolvePriceKey(&models.Product{Key: productKey}, req)
		obj := priceObject(productKey, key)
		old := state[obj]
		if old != nil {
			if err := checkRevision(obj.String(), req.ExpectedRevision, old.revision); err != nil {
				return nil, err
			}
		} else if req.ExpectedRevision != nil && *req.ExpectedRevision != 0 {
			return nil, ErrCatalogConflict
		}
		fields := map[string]json.RawMessage{
			"currency": catalogJSON(req.Currency), "unit_amount": catalogMoney(req.UnitAmount), "access_duration_hours": catalogJSON(req.AccessDurationHours),
			"billing_interval_hours": catalogJSON(req.BillingIntervalHours), "trial_unit_amount": catalogOptionalMoney(req.TrialUnitAmount),
			"trial_duration_hours": catalogJSON(req.TrialDurationHours), "archived": catalogJSON(req.Archived), "customer_amount": catalogJSON(req.CustomerAmount), "quantity": catalogJSON(req.Quantity),
		}
		changed := catalogFieldsChanged(obj, old, fields)
		if !changed && old != nil {
			var links map[string]map[string]string
			if err := json.Unmarshal(old.fields["psp_links"], &links); err != nil {
				return nil, err
			}
			for _, key := range req.PSPs {
				if _, ok := links[key]; !ok {
					changed = true
				}
			}
			for key, link := range req.PSPLinks {
				if !catalogLinkContains(links[key], normalizeLinkMap(link)) {
					changed = true
				}
			}
		}
		if changed {
			targets = append(targets, obj)
		}
		return targets, nil
	}
}

func sidecarPlan(desired SyncCatalogSidecarsRequest, opts CatalogMutationOptions) catalogClassification {
	return func(ctx context.Context, s *Service, state catalogState) ([]catalogObject, error) {
		if err := normalizeCatalogBilling(&desired); err != nil {
			return nil, err
		}
		mid, err := merchant.Require(ctx)
		if err != nil {
			return nil, err
		}
		current, err := readCatalogBillingQueries(ctx, s.catalogDatabase().Gen(ctx), mid.UUID())
		if err != nil {
			return nil, err
		}
		next := mergeCatalogBilling(current, desired, opts)
		targets := map[catalogObject]bool{}
		beforeMeters := map[string]CatalogMeterSpec{}
		afterMeters := map[string]CatalogMeterSpec{}
		for _, m := range current.Meters {
			beforeMeters[m.Key] = m
		}
		for _, m := range next.Meters {
			afterMeters[m.Key] = m
		}
		for key, m := range beforeMeters {
			if n, ok := afterMeters[key]; !ok || !reflect.DeepEqual(m, n) {
				targets[meterObject(key)] = true
			}
		}
		for key, m := range afterMeters {
			if o, ok := beforeMeters[key]; !ok || !reflect.DeepEqual(o, m) {
				targets[meterObject(key)] = true
			}
		}
		beforeCards := map[string][]CatalogRateCardSpec{}
		afterCards := map[string][]CatalogRateCardSpec{}
		for _, c := range current.RateCards {
			beforeCards[c.ProductKey] = append(beforeCards[c.ProductKey], c)
		}
		for _, c := range next.RateCards {
			if state[productObject(c.ProductKey)] != nil {
				afterCards[c.ProductKey] = append(afterCards[c.ProductKey], c)
			}
		}
		for key, cards := range beforeCards {
			if !sameCatalogCards(cards, afterCards[key]) {
				targets[productObject(key)] = true
			}
		}
		for key, cards := range afterCards {
			if !sameCatalogCards(cards, beforeCards[key]) {
				targets[productObject(key)] = true
			}
		}
		return slices.SortedFunc(maps.Keys(targets), compareCatalogObjects), nil
	}
}

func meterEditPlan(spec catalogrules.Meter, card *UsageRateCardInput, remove bool, expected *int64) catalogClassification {
	return func(ctx context.Context, s *Service, state catalogState) ([]catalogObject, error) {
		if err := catalogrules.ValidateMeter("meter", &spec); err != nil {
			return nil, err
		}
		obj := meterObject(spec.Key)
		old := state[obj]
		revision := int64(0)
		if old != nil {
			revision = old.revision
		}
		if err := checkRevision(obj.String(), expected, revision); err != nil {
			return nil, err
		}
		fields := catalogMeterValues(CatalogMeterSpec{Key: spec.Key, EventType: spec.EventType, ValueProperty: spec.ValueProperty, Aggregation: spec.Aggregation, Unit: spec.Unit, GroupBy: spec.GroupBy})
		targets := map[catalogObject]bool{}
		if catalogFieldsChanged(obj, old, fields) {
			targets[obj] = true
		}
		if card != nil || remove {
			mid, err := merchant.Require(ctx)
			if err != nil {
				return nil, err
			}
			current, err := readCatalogBillingQueries(ctx, s.catalogDatabase().Gen(ctx), mid.UUID())
			if err != nil {
				return nil, err
			}
			var existing *CatalogRateCardSpec
			for i := range current.RateCards {
				if current.RateCards[i].MeterKey == spec.Key {
					existing = &current.RateCards[i]
					break
				}
			}
			if remove {
				if existing != nil {
					targets[productObject(existing.ProductKey)] = true
				}
			} else {
				if card.ProductID == nil || *card.ProductID == uuid.Nil {
					return nil, ErrRateCardProductNotFound
				}
				product, _, err := catalogObjectWithProductID(state, billing.ProductID(*card.ProductID))
				if err != nil {
					return nil, err
				}
				next := CatalogRateCardSpec{ProductKey: product.key, MeterKey: spec.Key, Filter: card.Filter, Price: catalogJSON(card.Price)}
				if card.Allowance != nil {
					next.Allowance = catalogJSON(card.Allowance)
				}
				if existing != nil {
					next.Ordinal = existing.Ordinal
					next.PaymentTerm = existing.PaymentTerm
				}
				if existing == nil || !sameCatalogCards([]CatalogRateCardSpec{*existing}, []CatalogRateCardSpec{next}) {
					targets[product] = true
					if existing != nil {
						targets[productObject(existing.ProductKey)] = true
					}
				}
			}
		}
		return slices.SortedFunc(maps.Keys(targets), compareCatalogObjects), nil
	}
}
