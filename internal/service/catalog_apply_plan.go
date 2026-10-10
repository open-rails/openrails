package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	catalogwire "github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/cadence"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// catalogPlan is a document reduced to the objects it may apply: an object
// with a named field an edit set differently is skipped whole.
type catalogPlan struct {
	document  catalogwire.Application
	named     map[catalogObject][]string // applied objects' named fields
	declared  map[catalogObject]bool     // objects the document itself declares
	conflicts []billing.CatalogConflict
	prune     []catalogObject
	release   []catalogObject // omitted by a whole catalog, not archived
	// renamed are the products whose keys only the entitlement replacements
	// change: an operator's rename, which applies to every product, edited or
	// not, and leaves the keys' owners as they were.
	renamed map[catalogObject]bool
}

func planCatalogApplication(params catalogwire.Application, live catalogState, owners catalogOwners, links map[[2]string]map[string]map[string]string, force bool) (catalogPlan, error) {
	plan := catalogPlan{named: map[catalogObject][]string{}, declared: map[catalogObject]bool{}, conflicts: []billing.CatalogConflict{}, renamed: map[catalogObject]bool{}}
	plan.document = catalogwire.Application{SchemaVersion: params.SchemaVersion, EntitlementReplacements: params.EntitlementReplacements}
	renamed, err := renamedProducts(live, params.EntitlementReplacements)
	if err != nil {
		return plan, err
	}
	for _, key := range renamed {
		if !params.Products[key].Entitlements.Set {
			plan.renamed[productObject(key)] = true
		}
	}
	consider := func(obj catalogObject, fields map[string]json.RawMessage) bool {
		if len(fields) == 0 {
			return true
		}
		if conflict, ok := fieldConflicts(obj, fields, live[obj], owners, force); ok {
			plan.conflicts = append(plan.conflicts, conflict)
			return false
		}
		plan.named[obj] = slices.Sorted(maps.Keys(fields))
		return true
	}
	if len(params.Products) > 0 {
		plan.document.Products = map[string]catalogwire.ApplyProduct{}
	}
	for _, key := range slices.Sorted(maps.Keys(params.Products)) {
		decl := params.Products[key]
		fields, err := documentProductFields(key, decl)
		if err != nil {
			return plan, err
		}
		plan.declared[productObject(key)] = true
		if !consider(productObject(key), fields) {
			// Its prices are their own objects and may still apply.
			decl = catalogwire.ApplyProduct{Prices: decl.Prices}
		}
		prices := map[string]catalogwire.ApplyPrice{}
		for _, priceKey := range slices.Sorted(maps.Keys(decl.Prices)) {
			price := decl.Prices[priceKey]
			plan.declared[priceObject(key, priceKey)] = true
			if consider(priceObject(key, priceKey), documentPriceFields(price, links[[2]string{key, priceKey}])) {
				prices[priceKey] = price
			}
		}
		decl.Prices = nil
		if len(prices) > 0 {
			decl.Prices = prices
		}
		plan.document.Products[key] = decl
	}
	for _, key := range slices.Sorted(maps.Keys(params.Meters)) {
		plan.declared[meterObject(key)] = true
		if consider(meterObject(key), documentMeterFields(key, params.Meters[key])) {
			if plan.document.Meters == nil {
				plan.document.Meters = map[string]catalogwire.ApplyMeter{}
			}
			plan.document.Meters[key] = params.Meters[key]
		}
	}
	if params.Prune {
		// A whole catalog: an omitted object only documents set is archived;
		// one an edit touched or created is released to the edit.
		for _, obj := range slices.SortedFunc(maps.Keys(live), compareCatalogObjects) {
			product, declared := params.Products[obj.key]
			_, kept := product.Prices[obj.priceKey]
			switch {
			case obj.kind == billing.CatalogObjectMeter,
				obj.kind == billing.CatalogObjectProduct && declared,
				obj.kind == billing.CatalogObjectPrice && kept:
			case owners.soleFileOwned(obj):
				if string(live[obj].fields["archived"]) != "true" {
					plan.prune = append(plan.prune, obj)
				}
			default:
				plan.release = append(plan.release, obj)
			}
		}
	}
	return plan, nil
}

// renamedProducts are the products whose keys the replacements change.
func renamedProducts(live catalogState, pairs []catalogwire.EntitlementReplacement) ([]string, error) {
	var out []string
	if len(pairs) == 0 {
		return out, nil
	}
	for obj, state := range live {
		if obj.kind != billing.CatalogObjectProduct {
			continue
		}
		var keys []string
		if err := json.Unmarshal(state.fields["entitlements"], &keys); err != nil {
			return nil, err
		}
		next := slices.Clone(keys)
		for _, pair := range pairs {
			if i := slices.Index(next, pair.From); i >= 0 {
				next = slices.Delete(next, i, i+1)
				if pair.To != "" && !slices.Contains(next, pair.To) {
					next = append(next, pair.To)
				}
			}
		}
		slices.Sort(next)
		if !slices.Equal(next, keys) {
			out = append(out, obj.key)
		}
	}
	return out, nil
}

// fieldConflicts lists the fields of an existing object the document names
// whose live values an edit set differently.
func fieldConflicts(obj catalogObject, fields map[string]json.RawMessage, live *catalogObjectState, owners catalogOwners, force bool) (billing.CatalogConflict, bool) {
	conflict := billing.CatalogConflict{Object: obj.kind, Key: obj.ownKey(), ProductKey: obj.productKey()}
	if live == nil || force {
		return conflict, false
	}
	if obj.kind == billing.CatalogObjectPrice {
		var currency string
		if json.Unmarshal(live.fields["currency"], &currency) == nil {
			conflict.Currency = &currency
		}
	}
	for _, field := range catalogFields[obj.kind] {
		value, named := fields[field]
		if !named || bytes.Equal(value, live.fields[field]) {
			continue
		}
		edit, edited := owners[obj][field][catalogManagerEdit]
		if !edited {
			continue
		}
		conflict.Fields = append(conflict.Fields, billing.CatalogFieldConflict{Field: field, FileValue: value, LiveValue: live.fields[field], SetBy: edit.actor, SetAt: edit.setAt.UTC()})
	}
	return conflict, len(conflict.Fields) > 0
}

// pruneCatalogObjects archives omitted objects only the document held.
func (s *Service) pruneCatalogObjects(ctx context.Context, objects []catalogObject) error {
	for _, obj := range objects {
		state := s.catalogBefore[obj]
		var err error
		switch obj.kind {
		case billing.CatalogObjectProduct:
			_, err = s.DeactivateProduct(ctx, billing.ProductID(state.productID))
		case billing.CatalogObjectPrice:
			_, err = s.DeactivatePrice(ctx, billing.PriceID(state.priceID))
		}
		if err != nil {
			return fmt.Errorf("prune %s: %w", obj, err)
		}
	}
	return nil
}

// recordCatalogApply makes the document the owner of every field it set or
// named equal; an edit keeps an equal field too, and loses one the document
// changed (only force changes an edited field). A field an applied object
// no longer names is relinquished. It returns the objects changed.
func (s *Service) recordCatalogApply(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, before, after catalogState, plan catalogPlan, actor string) ([]billing.CatalogChange, error) {
	d := diffCatalogState(before, after)
	var w catalogOwnerWrites
	owners, err := readCatalogOwners(ctx, q, merchantID)
	if err != nil {
		return nil, err
	}
	owned := func(obj catalogObject, field string) bool {
		return !(field == "entitlements" && plan.renamed[obj])
	}
	for _, obj := range slices.SortedFunc(maps.Keys(d.changed), compareCatalogObjects) {
		for _, field := range d.changed[obj] {
			if owned(obj, field) {
				w.release = append(w.release, catalogFieldRef{obj, field})
			}
		}
	}
	for _, obj := range slices.SortedFunc(maps.Keys(plan.named), compareCatalogObjects) {
		named := plan.named[obj]
		for _, field := range named {
			w.take = append(w.take, catalogFieldRef{obj, field})
		}
		if !plan.declared[obj] {
			continue // touched only by a replacement: it relinquishes nothing
		}
		for field, managers := range owners[obj] {
			if _, held := managers[catalogManagerApply]; held && !slices.Contains(named, field) && !slices.Contains(d.changed[obj], field) {
				w.drop = append(w.drop, catalogFieldRef{obj, field})
			}
		}
	}
	for obj, fields := range d.changed {
		if _, named := plan.named[obj]; named {
			continue
		}
		for _, field := range fields {
			if owned(obj, field) {
				w.take = append(w.take, catalogFieldRef{obj, field})
			}
		}
	}
	for _, obj := range plan.release {
		for field, managers := range owners[obj] {
			if _, held := managers[catalogManagerApply]; held && !slices.Contains(d.changed[obj], field) {
				w.drop = append(w.drop, catalogFieldRef{obj, field})
			}
		}
	}
	w.gone = d.gone
	revisions, err := stepUnmovedRevisions(ctx, q, merchantID, before, after, d)
	if err != nil {
		return nil, err
	}
	if err := w.write(ctx, q, merchantID, catalogManagerApply, actor, s.now().UTC()); err != nil {
		return nil, err
	}
	changes := []billing.CatalogChange{}
	for _, obj := range slices.SortedFunc(maps.Keys(d.changed), compareCatalogObjects) {
		fields := d.changed[obj]
		if fields == nil {
			fields = []string{}
		}
		changes = append(changes, billing.CatalogChange{Object: obj.kind, Key: obj.ownKey(), ProductKey: obj.productKey(), Fields: fields, Revision: revisions[obj]})
	}
	return changes, nil
}

// FindingCatalogConflicts is the one finding that names the objects the
// latest catalog document skipped because an edit set their fields.
const FindingCatalogConflicts reconcile.FindingType = "life.catalog.conflicts"

const catalogConflictsSubject = "catalog"

// reportCatalogConflicts keeps the finding in step with the latest document:
// opened or updated while it skips objects, closed once one applies whole.
func (s *Service) reportCatalogConflicts(ctx context.Context, receipt *billing.CatalogApplicationReceipt) error {
	store := &reconcile.PGStore{DB: s.catalogDatabase()}
	if len(receipt.Conflicts) == 0 {
		return store.ResolveRaisedFinding(ctx, FindingCatalogConflicts, catalogConflictsSubject)
	}
	lines := ReadableCatalogConflicts(receipt.Conflicts)
	_, err := store.RaiseFinding(ctx, reconcile.RaisedFinding{
		Type: FindingCatalogConflicts, SubjectKey: catalogConflictsSubject, Severity: reconcile.SeverityMedium,
		RecommendedAction: fmt.Sprintf("The catalog document %s skipped %d object(s) whose fields an edit set differently; everything else applied. Change the document to agree, remove those fields from it, or apply it with force.", receipt.ApplicationID, len(receipt.Conflicts)),
		Evidence:          map[string]any{"application_id": receipt.ApplicationID, "conflicts": lines},
	})
	return err
}

// ReadableCatalogConflicts writes each skipped object and its conflicting
// fields for a person: money and durations in readable units.
func ReadableCatalogConflicts(conflicts []billing.CatalogConflict) []string {
	var out []string
	for _, c := range conflicts {
		name := fmt.Sprintf("%s %s", c.Object, c.Key)
		if c.ProductKey != nil {
			name = fmt.Sprintf("price %s of product %s", c.Key, *c.ProductKey)
		}
		fields := make([]string, 0, len(c.Fields))
		currency := conflictCurrency(c)
		for _, f := range c.Fields {
			fields = append(fields, fmt.Sprintf("%s: the file says %s, an edit set %s (%s, %s)", f.Field,
				readableCatalogValue(f.Field, f.FileValue, currency.file), readableCatalogValue(f.Field, f.LiveValue, currency.live), f.SetBy, f.SetAt.UTC().Format(time.RFC3339)))
		}
		out = append(out, fmt.Sprintf("%s skipped: %s", name, strings.Join(fields, "; ")))
	}
	return out
}

type conflictCurrencies struct{ file, live string }

// conflictCurrency is the currency each side's money is in: the conflicting
// currencies when currency is a field, otherwise the price's.
func conflictCurrency(c billing.CatalogConflict) conflictCurrencies {
	var out conflictCurrencies
	if c.Currency != nil {
		out.file, out.live = *c.Currency, *c.Currency
	}
	for _, f := range c.Fields {
		if f.Field == "currency" {
			_ = json.Unmarshal(f.FileValue, &out.file)
			_ = json.Unmarshal(f.LiveValue, &out.live)
		}
	}
	return out
}

func readableCatalogValue(field string, raw json.RawMessage, currency string) string {
	if string(raw) == "null" {
		return "none"
	}
	switch field {
	case "unit_amount", "trial_unit_amount":
		var text string
		if json.Unmarshal(raw, &text) == nil {
			if amount, err := strconv.ParseInt(text, 10, 64); err == nil {
				if _, known := moneyutil.LookupCurrency(currency); known {
					return moneyutil.FormatAmount(amount, currency)
				}
				return fmt.Sprintf("%d micros", amount)
			}
		}
	case "access_duration_hours", "billing_interval_hours", "trial_duration_hours":
		var hours int
		if json.Unmarshal(raw, &hours) == nil {
			return cadence.FormatHours(hours)
		}
	}
	return string(raw)
}
