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
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// A catalog object is what a document declares and an edit changes: a
// product, one of its price keys (every version and its PSP links) or a
// meter. Each carries a revision and per-field ownership; a document applies
// an object whole or skips it.
type catalogObject struct {
	kind     billing.CatalogObjectKind
	key      string // product or meter key; a price's product key
	priceKey string
}

func productObject(key string) catalogObject {
	return catalogObject{kind: billing.CatalogObjectProduct, key: key}
}
func priceObject(product, key string) catalogObject {
	return catalogObject{kind: billing.CatalogObjectPrice, key: product, priceKey: key}
}
func meterObject(key string) catalogObject {
	return catalogObject{kind: billing.CatalogObjectMeter, key: key}
}

// ownKey is the object's own key: a price's key, otherwise key.
func (o catalogObject) ownKey() string {
	if o.kind == billing.CatalogObjectPrice {
		return o.priceKey
	}
	return o.key
}

func (o catalogObject) productKey() *string {
	if o.kind != billing.CatalogObjectPrice {
		return nil
	}
	key := o.key
	return &key
}

func (o catalogObject) String() string {
	if o.kind == billing.CatalogObjectPrice {
		return fmt.Sprintf("price %s/%s", o.key, o.priceKey)
	}
	return fmt.Sprintf("%s %s", o.kind, o.key)
}

func compareCatalogObjects(a, b catalogObject) int {
	return strings.Compare(string(a.kind)+"\x00"+a.key+"\x00"+a.priceKey, string(b.kind)+"\x00"+b.key+"\x00"+b.priceKey)
}

// catalogFields are each object's owned fields, in the order results list them.
var catalogFields = map[billing.CatalogObjectKind][]string{
	billing.CatalogObjectProduct: {"display_name", "description", "tier_group", "tier_rank", "archived", "ownership", "entitlements", "credit_grant", "rate_cards"},
	billing.CatalogObjectPrice:   {"currency", "unit_amount", "access_duration_hours", "billing_interval_hours", "trial_unit_amount", "trial_duration_hours", "customer_amount", "quantity", "archived", "psp_links"},
	billing.CatalogObjectMeter:   {"event_type", "value_property", "aggregation", "unit", "group_by"},
}

// catalogObjectState is one object as stored: each field as the document
// writes it, and content that changes the object without being a field of
// its own (a meter's rate card belongs to its product).
type catalogObjectState struct {
	revision  int64
	fields    map[string]json.RawMessage
	hidden    json.RawMessage
	productID uuid.UUID
	priceID   uuid.UUID // a price key's current version
}

type catalogState map[catalogObject]*catalogObjectState

func catalogJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("catalog value: %v", err))
	}
	return raw
}

func catalogMoney(v int64) json.RawMessage { return catalogJSON(strconv.FormatInt(v, 10)) }

func catalogOptionalMoney(v *int64) json.RawMessage {
	if v == nil {
		return json.RawMessage("null")
	}
	return catalogMoney(*v)
}

func catalogOptionalHours(v *int32) json.RawMessage {
	if v == nil {
		return json.RawMessage("null")
	}
	return catalogJSON(int(*v))
}

func catalogOptionalText(v *string) json.RawMessage {
	if v == nil || *v == "" {
		return json.RawMessage("null")
	}
	return catalogJSON(*v)
}

// readCatalogState reads every catalog object of the merchant in the
// transaction that holds its catalog lock.
func readCatalogState(ctx context.Context, q *gen.Queries, merchantID uuid.UUID) (catalogState, error) {
	state := catalogState{}
	billingState, err := readCatalogBillingQueries(ctx, q, merchantID)
	if err != nil {
		return nil, err
	}
	cards := map[string][]CatalogRateCardSpec{}
	meterCards := map[string]json.RawMessage{}
	for _, card := range billingState.RateCards {
		cards[card.ProductKey] = append(cards[card.ProductKey], card)
		if card.MeterKey != "" {
			meterCards[card.MeterKey] = catalogJSON(card)
		}
	}
	products, err := q.ListCatalogStateProducts(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	productKeys := make(map[uuid.UUID]string, len(products))
	for _, p := range products {
		productKeys[p.ID] = p.Key
		var entitlements []string
		if err := json.Unmarshal(p.Entitlements, &entitlements); err != nil {
			return nil, fmt.Errorf("product %q entitlements: %w", p.Key, err)
		}
		slices.Sort(entitlements)
		credit := json.RawMessage("null")
		if len(p.CreditGrant) > 0 && string(p.CreditGrant) != "null" {
			var spec catalogwire.CreditGrantSpec
			if err := json.Unmarshal(p.CreditGrant, &spec); err != nil {
				return nil, fmt.Errorf("product %q credit grant: %w", p.Key, err)
			}
			normalized, err := normalizeCreditGrant(&spec)
			if err != nil {
				return nil, err
			}
			credit = catalogJSON(normalized)
		}
		rateCards, err := catalogRateCardsValue(cards[p.Key])
		if err != nil {
			return nil, err
		}
		state[productObject(p.Key)] = &catalogObjectState{revision: p.Revision, productID: p.ID, fields: map[string]json.RawMessage{
			"display_name": catalogJSON(p.DisplayName),
			"description":  catalogJSON(p.Description),
			"tier_group":   catalogOptionalText(p.TierGroup),
			"tier_rank":    catalogJSON(int(p.TierRank)),
			"archived":     catalogJSON(p.Archived),
			"ownership":    catalogOptionalText(p.Ownership),
			"entitlements": catalogJSON(append([]string{}, entitlements...)),
			"credit_grant": credit,
			"rate_cards":   rateCards,
		}}
	}
	prices, err := q.ListCatalogStatePrices(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	for _, p := range prices {
		obj := priceObject(productKeys[p.ProductID], p.Key)
		// Rows come oldest version first: the key's current version is its live
		// one, or else its newest.
		if prior := state[obj]; prior != nil && string(prior.fields["archived"]) == "false" {
			continue
		}
		customer := json.RawMessage("null")
		if len(p.CustomerAmount) > 0 && string(p.CustomerAmount) != "null" {
			var amount catalogwire.CustomerAmount
			if err := json.Unmarshal(p.CustomerAmount, &amount); err != nil {
				return nil, fmt.Errorf("price %q customer amount: %w", p.Key, err)
			}
			customer = catalogJSON(amount)
		}
		quantity := json.RawMessage("null")
		if len(p.Quantity) > 0 && string(p.Quantity) != "null" {
			var q catalogwire.Quantity
			if err := json.Unmarshal(p.Quantity, &q); err != nil {
				return nil, fmt.Errorf("price %q quantity: %w", p.Key, err)
			}
			quantity = catalogJSON(q)
		}
		var links map[string]map[string]string
		if err := json.Unmarshal(p.PspLinks, &links); err != nil {
			return nil, fmt.Errorf("price %q links: %w", p.Key, err)
		}
		if links == nil {
			links = map[string]map[string]string{}
		}
		state[obj] = &catalogObjectState{revision: p.KeyRevision, productID: p.ProductID, priceID: p.ID, fields: map[string]json.RawMessage{
			"currency":               catalogJSON(p.Currency),
			"unit_amount":            catalogMoney(p.Amount),
			"access_duration_hours":  catalogOptionalHours(p.AccessDurationHours),
			"billing_interval_hours": catalogOptionalHours(p.BillingIntervalHours),
			"trial_unit_amount":      catalogOptionalMoney(p.TrialUnitAmount),
			"trial_duration_hours":   catalogOptionalHours(p.TrialDurationHours),
			"customer_amount":        customer,
			"quantity":               quantity,
			"archived":               catalogJSON(p.Archived),
			"psp_links":              catalogJSON(links),
		}}
	}
	revisions, err := q.ListCatalogMeterRevisions(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	meterRevisions := make(map[string]int64, len(revisions))
	for _, r := range revisions {
		meterRevisions[r.Key] = r.Revision
	}
	for _, m := range billingState.Meters {
		state[meterObject(m.Key)] = &catalogObjectState{revision: meterRevisions[m.Key], hidden: meterCards[m.Key], fields: catalogMeterValues(m)}
	}
	return state, nil
}

func catalogMeterValues(m CatalogMeterSpec) map[string]json.RawMessage {
	groupBy := json.RawMessage("null")
	if len(m.GroupBy) > 0 {
		groupBy = catalogJSON(m.GroupBy)
	}
	return map[string]json.RawMessage{
		"event_type":     catalogJSON(m.EventType),
		"value_property": catalogJSON(m.ValueProperty),
		"aggregation":    catalogJSON(string(m.Aggregation)),
		"unit":           catalogJSON(m.Unit),
		"group_by":       groupBy,
	}
}

// catalogRateCardsValue is a product's normalized cards as the document
// declares them, by ordinal.
func catalogRateCardsValue(specs []CatalogRateCardSpec) (json.RawMessage, error) {
	specs = slices.Clone(specs)
	slices.SortFunc(specs, func(a, b CatalogRateCardSpec) int { return a.Ordinal - b.Ordinal })
	out := make([]catalogwire.RateCard, 0, len(specs))
	for _, spec := range specs {
		card := catalogwire.RateCard{Ordinal: spec.Ordinal, Meter: spec.MeterKey, PaymentTerm: catalogwire.PaymentTerm(spec.PaymentTerm), Filter: spec.Filter}
		if err := json.Unmarshal(spec.Price, &card.Price); err != nil {
			return nil, err
		}
		if len(spec.Allowance) > 0 {
			card.Allowance = &catalogwire.Allowance{}
			if err := json.Unmarshal(spec.Allowance, card.Allowance); err != nil {
				return nil, err
			}
		}
		out = append(out, card)
	}
	return catalogJSON(out), nil
}

// documentRateCards are a product's declared cards as storage holds them.
func documentRateCards(productKey string, cards []catalogwire.RateCard) ([]CatalogRateCardSpec, error) {
	out := make([]CatalogRateCardSpec, 0, len(cards))
	ordinals := map[int]bool{}
	for i, rc := range cards {
		ordinal := rc.Ordinal
		if ordinal == 0 {
			ordinal = i + 1
		}
		if ordinal < 1 || ordinals[ordinal] {
			return nil, apperr.Invalidf("product %q has invalid or duplicate rate-card ordinal %d", productKey, ordinal)
		}
		ordinals[ordinal] = true
		price, err := json.Marshal(rc.Price)
		if err != nil {
			return nil, err
		}
		var allowance json.RawMessage
		if rc.Allowance != nil {
			if allowance, err = json.Marshal(rc.Allowance); err != nil {
				return nil, err
			}
		}
		out = append(out, CatalogRateCardSpec{ProductKey: productKey, Ordinal: ordinal, MeterKey: rc.Meter, PaymentTerm: string(rc.PaymentTerm), Filter: rc.Filter, Allowance: allowance, Price: price})
	}
	return out, nil
}

// documentProductFields are the product fields a declaration names, valued
// as the live state would hold them.
func documentProductFields(key string, d catalogwire.ApplyProduct) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	if d.DisplayName.Set {
		out["display_name"] = catalogJSON(d.DisplayName.Value)
	}
	if d.Description.Set {
		out["description"] = catalogJSON(d.Description.Value)
	}
	if d.TierGroup.Set {
		out["tier_group"] = catalogOptionalText(&d.TierGroup.Value)
	}
	if d.TierRank.Set {
		out["tier_rank"] = catalogJSON(d.TierRank.Value)
	}
	if d.Archived.Set {
		out["archived"] = catalogJSON(d.Archived.Value)
	}
	if d.Ownership.Set {
		value := string(d.Ownership.Value)
		out["ownership"] = catalogOptionalText(&value)
	}
	if d.Entitlements.Set {
		keys, err := catalogwire.NormalizeEntitlements(d.Entitlements.Value)
		if err != nil {
			return nil, apperr.Invalidf("product %q: %v", key, err)
		}
		keys = append([]string{}, keys...)
		slices.Sort(keys)
		out["entitlements"] = catalogJSON(keys)
	}
	if d.CreditGrant.Set {
		out["credit_grant"] = json.RawMessage("null")
		if !d.CreditGrant.Null {
			normalized, err := normalizeCreditGrant(&d.CreditGrant.Value)
			if err != nil {
				return nil, err
			}
			out["credit_grant"] = catalogJSON(normalized)
		}
	}
	if d.RateCards.Set {
		specs, err := documentRateCards(key, d.RateCards.Value)
		if err != nil {
			return nil, err
		}
		normalized := SyncCatalogSidecarsRequest{RateCards: specs}
		if err := normalizeCatalogBilling(&normalized); err != nil {
			return nil, apperr.Invalidf("product %q: %v", key, err)
		}
		if out["rate_cards"], err = catalogRateCardsValue(normalized.RateCards); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// documentPriceFields are the fields a price declaration names. Its PSP links
// are what the application prepared for it.
func documentPriceFields(d catalogwire.ApplyPrice, links map[string]map[string]string) map[string]json.RawMessage {
	hours := func(f catalogwire.Field[int]) json.RawMessage {
		if f.Null {
			return json.RawMessage("null")
		}
		return catalogJSON(f.Value)
	}
	nullable := func(null bool, value any) json.RawMessage {
		if null {
			return json.RawMessage("null")
		}
		return catalogJSON(value)
	}
	trial := json.RawMessage("null")
	if !d.TrialUnitAmount.Null {
		trial = catalogMoney(d.TrialUnitAmount.Value)
	}
	if links == nil {
		links = map[string]map[string]string{}
	}
	values := map[string]json.RawMessage{
		"currency":               catalogJSON(moneyutil.NormalizeCurrency(d.Currency.Value)),
		"unit_amount":            catalogMoney(d.UnitAmount.Value),
		"access_duration_hours":  hours(d.AccessDurationHours),
		"billing_interval_hours": hours(d.BillingIntervalHours),
		"trial_unit_amount":      trial,
		"trial_duration_hours":   hours(d.TrialDurationHours),
		"customer_amount":        nullable(d.CustomerAmount.Null, d.CustomerAmount.Value),
		"quantity":               nullable(d.Quantity.Null, d.Quantity.Value),
		"archived":               catalogJSON(d.Archived.Value),
		"psp_links":              catalogJSON(links),
	}
	named := map[string]bool{
		"currency": d.Currency.Set, "unit_amount": d.UnitAmount.Set,
		"access_duration_hours": d.AccessDurationHours.Set, "billing_interval_hours": d.BillingIntervalHours.Set,
		"trial_unit_amount": d.TrialUnitAmount.Set, "trial_duration_hours": d.TrialDurationHours.Set,
		"customer_amount": d.CustomerAmount.Set, "quantity": d.Quantity.Set, "archived": d.Archived.Set,
		"psp_links": d.PSPs.Set || d.PSPLinks.Set,
	}
	for field := range values {
		if !named[field] {
			delete(values, field)
		}
	}
	return values
}

// documentMeterFields are the meter fields a declaration names, normalized as
// the apply stores them.
func documentMeterFields(key string, d catalogwire.ApplyMeter) map[string]json.RawMessage {
	spec := CatalogMeterSpec{Key: key, EventType: d.EventType.Value, ValueProperty: d.ValueProperty.Value, Aggregation: d.Aggregation.Value, Unit: d.Unit.Value, GroupBy: d.GroupBy.Value}
	normalized := SyncCatalogSidecarsRequest{Meters: []CatalogMeterSpec{spec}}
	_ = normalizeCatalogBilling(&normalized)
	values := catalogMeterValues(normalized.Meters[0])
	out := map[string]json.RawMessage{}
	for field, set := range map[string]bool{"event_type": d.EventType.Set, "value_property": d.ValueProperty.Set, "aggregation": d.Aggregation.Set, "unit": d.Unit.Set, "group_by": d.GroupBy.Set} {
		if set {
			out[field] = values[field]
		}
	}
	return out
}

// emptyCatalogValue is a field nobody set: an object created without it holds
// it unowned.
func emptyCatalogValue(raw json.RawMessage) bool {
	switch string(raw) {
	case "null", `""`, "[]", "{}", "false", "0":
		return true
	}
	return false
}

// changedFields are the fields of an object that differ between two states;
// for a new object, those it holds a value for.
func changedFields(kind billing.CatalogObjectKind, before, after *catalogObjectState) []string {
	var out []string
	for _, field := range catalogFields[kind] {
		switch {
		case after == nil:
		case before == nil:
			if !emptyCatalogValue(after.fields[field]) {
				out = append(out, field)
			}
		case !bytes.Equal(before.fields[field], after.fields[field]):
			out = append(out, field)
		}
	}
	return out
}

// catalogOwner is who set a field, and when.
type catalogOwner struct {
	actor string
	setAt time.Time
}

const (
	catalogManagerApply = "apply"
	catalogManagerEdit  = "edit"
)

// catalogOwners is each object's fields' managers.
type catalogOwners map[catalogObject]map[string]map[string]catalogOwner

func readCatalogOwners(ctx context.Context, q *gen.Queries, merchantID uuid.UUID) (catalogOwners, error) {
	rows, err := q.ListCatalogFieldOwners(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	out := catalogOwners{}
	for _, row := range rows {
		obj := catalogObject{kind: billing.CatalogObjectKind(row.Object), key: row.Key, priceKey: row.PriceKey}
		if out[obj] == nil {
			out[obj] = map[string]map[string]catalogOwner{}
		}
		if out[obj][row.Field] == nil {
			out[obj][row.Field] = map[string]catalogOwner{}
		}
		out[obj][row.Field][row.Manager] = catalogOwner{actor: row.Actor, setAt: row.SetAt}
	}
	return out, nil
}

// soleFileOwned is an object the document manager holds fields of and no
// edit does.
func (o catalogOwners) soleFileOwned(obj catalogObject) bool {
	applied := false
	for _, managers := range o[obj] {
		if _, edited := managers[catalogManagerEdit]; edited {
			return false
		}
		_, held := managers[catalogManagerApply]
		applied = applied || held
	}
	return applied
}

type catalogFieldRef struct {
	obj   catalogObject
	field string
}

type catalogOwnerWrites struct {
	take    []catalogFieldRef // the manager sets these
	release []catalogFieldRef // the other manager loses these
	drop    []catalogFieldRef // the manager relinquishes these
	gone    []catalogObject
}

func fieldArrays(refs []catalogFieldRef) (objects, keys, priceKeys, fields []string) {
	for _, r := range refs {
		objects = append(objects, string(r.obj.kind))
		keys = append(keys, r.obj.key)
		priceKeys = append(priceKeys, r.obj.priceKey)
		fields = append(fields, r.field)
	}
	return
}

func otherManager(manager string) string {
	if manager == catalogManagerApply {
		return catalogManagerEdit
	}
	return catalogManagerApply
}

func (w catalogOwnerWrites) write(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, manager, actor string, at time.Time) error {
	if len(w.take) > 0 {
		objects, keys, priceKeys, fields := fieldArrays(w.take)
		if err := q.UpsertCatalogFieldOwners(ctx, gen.UpsertCatalogFieldOwnersParams{MerchantID: merchantID, Manager: manager, Actor: actor, SetAt: at, Objects: objects, Keys: keys, PriceKeys: priceKeys, Fields: fields}); err != nil {
			return err
		}
	}
	for _, set := range []struct {
		manager string
		refs    []catalogFieldRef
	}{{otherManager(manager), w.release}, {manager, w.drop}} {
		if len(set.refs) == 0 {
			continue
		}
		objects, keys, priceKeys, fields := fieldArrays(set.refs)
		if err := q.DeleteCatalogFieldOwners(ctx, gen.DeleteCatalogFieldOwnersParams{MerchantID: merchantID, Manager: set.manager, Objects: objects, Keys: keys, PriceKeys: priceKeys, Fields: fields}); err != nil {
			return err
		}
	}
	if len(w.gone) > 0 {
		var objects, keys, priceKeys []string
		for _, obj := range w.gone {
			objects, keys, priceKeys = append(objects, string(obj.kind)), append(keys, obj.key), append(priceKeys, obj.priceKey)
		}
		return q.DeleteCatalogObjectOwners(ctx, gen.DeleteCatalogObjectOwnersParams{MerchantID: merchantID, Objects: objects, Keys: keys, PriceKeys: priceKeys})
	}
	return nil
}

// catalogDiff is what one catalog write changed: per object, its changed
// fields; objects that changed without a field of their own (a meter whose
// card moved) are listed with none.
type catalogDiff struct {
	changed map[catalogObject][]string
	gone    []catalogObject
}

func diffCatalogState(before, after catalogState) catalogDiff {
	d := catalogDiff{changed: map[catalogObject][]string{}}
	for obj, a := range after {
		b := before[obj]
		fields := changedFields(obj.kind, b, a)
		if len(fields) > 0 || b == nil || !bytes.Equal(b.hidden, a.hidden) {
			d.changed[obj] = fields
		}
	}
	for obj := range before {
		if after[obj] == nil {
			d.gone = append(d.gone, obj)
		}
	}
	slices.SortFunc(d.gone, compareCatalogObjects)
	return d
}

// stepUnmovedRevisions advances each changed object whose own row did not
// already: a product whose cards changed, a meter whose card changed. It
// returns each changed object's revision after.
func stepUnmovedRevisions(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, before, after catalogState, d catalogDiff) (map[catalogObject]int64, error) {
	out := make(map[catalogObject]int64, len(d.changed))
	var products []uuid.UUID
	var meters []string
	for _, obj := range slices.SortedFunc(maps.Keys(d.changed), compareCatalogObjects) {
		a, b := after[obj], before[obj]
		out[obj] = a.revision
		if b == nil || a.revision != b.revision {
			continue
		}
		switch obj.kind {
		case billing.CatalogObjectProduct:
			products = append(products, a.productID)
		case billing.CatalogObjectMeter:
			meters = append(meters, obj.key)
		default:
			continue
		}
		out[obj] = a.revision + 1
	}
	if len(products) > 0 {
		if err := q.StepProductRevisions(ctx, gen.StepProductRevisionsParams{MerchantID: merchantID, ProductIds: products}); err != nil {
			return nil, err
		}
	}
	if len(meters) > 0 {
		if err := q.StepMeterRevisions(ctx, gen.StepMeterRevisionsParams{MerchantID: merchantID, Keys: meters}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// recordCatalogEdit makes the edit manager the owner of every field the
// write changed: the document no longer holds them.
func (s *Service) recordCatalogEdit(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, before, after catalogState) error {
	d := diffCatalogState(before, after)
	var w catalogOwnerWrites
	for obj, fields := range d.changed {
		for _, field := range fields {
			w.take = append(w.take, catalogFieldRef{obj, field})
		}
	}
	w.release, w.gone = w.take, d.gone
	if _, err := stepUnmovedRevisions(ctx, q, merchantID, before, after, d); err != nil {
		return err
	}
	return w.write(ctx, q, merchantID, catalogManagerEdit, editActor(ctx), s.now().UTC())
}

// errRevisionMismatch refuses an edit whose object moved past the revision
// it expected; the caller reloads and retries.
func errRevisionMismatch(obj string, expected, current int64) error {
	e := apperr.New(409, billing.CodeRevisionMismatch, fmt.Sprintf("%s is at revision %d, not %d: it changed since it was read", obj, current, expected))
	return e.WithMetadata(map[string]any{"revision": current})
}

// checkRevision refuses an edit sent with a revision the object is no longer
// at. A missing object is at revision 0.
func checkRevision(obj string, expected *int64, current int64) error {
	if expected == nil || *expected == current {
		return nil
	}
	return errRevisionMismatch(obj, *expected, current)
}

// expectCatalogRevision checks an edit's expected revision against the
// object as the mutation found it.
func (s *Service) expectCatalogRevision(obj catalogObject, expected *int64) error {
	if expected == nil {
		return nil
	}
	var current int64
	if state := s.catalogBefore[obj]; state != nil {
		current = state.revision
	}
	return checkRevision(obj.String(), expected, current)
}
