package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	catalogwire "github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/catalogrules"
	"github.com/open-rails/openrails/internal/catalogrules/syntax"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// normalizeLimitRateCards validates a limit's rate cards and writes them as
// storage holds them.
func normalizeLimitRateCards(key string, cards []catalogwire.LimitRateCard) ([]catalogwire.LimitRateCard, error) {
	out := make([]catalogwire.LimitRateCard, 0, len(cards))
	for i, card := range cards {
		raw, err := json.Marshal(card)
		if err != nil {
			return nil, err
		}
		var copied catalogwire.LimitRateCard
		if err := json.Unmarshal(raw, &copied); err != nil {
			return nil, err
		}
		if err := catalogrules.ValidateLimitRateCard(fmt.Sprintf("limit %q rate card #%d", key, i+1), &copied); err != nil {
			return nil, apperr.Invalidf("%s", err)
		}
		out = append(out, copied)
	}
	return out, nil
}

// declaredLimit is def with decl's named fields set; a new limit needs unit
// and reset, and its overrun defaults to require_estimate.
func declaredLimit(key string, current *limits.Definition, decl catalogwire.ApplyLimit) (limits.Definition, error) {
	next := limits.Definition{Key: key, Overrun: catalogwire.OverrunRequireEstimate, RateCards: []catalogwire.LimitRateCard{}}
	if current != nil {
		next = *current
	} else if !decl.Unit.Set || !decl.Reset.Set {
		return next, apperr.Invalidf("new limit %q requires unit and reset", key)
	}
	if decl.Unit.Set {
		next.Unit = decl.Unit.Value
	}
	if decl.Reset.Set {
		next.Reset = decl.Reset.Value
	}
	if decl.Overrun.Set {
		next.Overrun = decl.Overrun.Value
	}
	if decl.RateCards.Set {
		cards, err := normalizeLimitRateCards(key, decl.RateCards.Value)
		if err != nil {
			return next, err
		}
		next.RateCards = cards
	}
	return next, validateLimit(next)
}

// validateLimit checks a whole declaration.
func validateLimit(d limits.Definition) error {
	if err := syntax.ValidateLimitKey(d.Key); err != nil {
		return apperr.Invalidf("%s", err).WithParam("key")
	}
	if err := syntax.ValidateLimitUnit(d.Unit); err != nil {
		return apperr.Invalidf("limit %q: %s", d.Key, err).WithParam("unit")
	}
	if err := d.Reset.Validate(); err != nil {
		return apperr.Invalidf("limit %q: %s", d.Key, err).WithParam("reset")
	}
	if err := d.Overrun.Validate(); err != nil {
		return apperr.Invalidf("limit %q: %s", d.Key, err).WithParam("overrun")
	}
	if syntax.LimitUnitCurrency(d.Unit) && len(d.RateCards) > 0 {
		return apperr.Invalidf("limit %q: a currency limit counts the rated amount and has no rate_cards", d.Key).WithParam("rate_cards")
	}
	return nil
}

// writeLimit stores a declaration that changes the limit. Its rate cards'
// meters exist, and a limit with windows keeps its unit.
func writeLimit(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, current *limits.Definition, next limits.Definition) error {
	if current != nil && bytes.Equal(catalogJSON(catalogLimitValues(*current)), catalogJSON(catalogLimitValues(next))) {
		return nil
	}
	if len(next.RateCards) > 0 {
		meters, err := q.ListCatalogMeters(ctx, merchantID)
		if err != nil {
			return err
		}
		declared := map[string]gen.ListCatalogMetersRow{}
		for _, m := range meters {
			declared[m.Key] = m
		}
		for i, card := range next.RateCards {
			meter, ok := declared[card.Meter]
			if !ok {
				return apperr.Invalidf("limit %q: rate card names meter %q, which is not declared", next.Key, card.Meter).WithParam("rate_cards")
			}
			var groupBy map[string]string
			if err := json.Unmarshal(meter.GroupBy, &groupBy); err != nil {
				return err
			}
			where := fmt.Sprintf("limit %q rate card #%d", next.Key, i+1)
			if err := catalogrules.ValidateMeterPrice(where, catalogwire.Aggregation(meter.Aggregation), card.Price); err != nil {
				return apperr.Invalidf("%s", err).WithParam("rate_cards")
			}
			if err := catalogrules.ValidateDimensions(where, groupBy, card.Filter, &card.Price); err != nil {
				return apperr.Invalidf("%s", err).WithParam("rate_cards")
			}
		}
	}
	if current != nil && current.Unit != next.Unit {
		used, err := q.CatalogLimitHasWindows(ctx, gen.CatalogLimitHasWindowsParams{MerchantID: merchantID, LimitKey: next.Key})
		if err != nil {
			return err
		}
		if used {
			return apperr.Conflictf("limit %q has windows counted in %s; it keeps its unit", next.Key, current.Unit).WithParam("unit")
		}
	}
	cards, err := json.Marshal(next.RateCards)
	if err != nil {
		return err
	}
	params := gen.UpsertCatalogLimitParams{MerchantID: merchantID, LimitKey: next.Key, Unit: next.Unit, ResetKind: string(next.Reset.Kind), Overrun: string(next.Overrun), RateCards: cards}
	if next.Reset.WindowSeconds != 0 {
		seconds := int32(next.Reset.WindowSeconds) // #nosec G115 -- bounded by catalog.MaxLimitWindowSeconds
		params.WindowSeconds = &seconds
	}
	if next.Reset.Anchor != "" {
		anchor := string(next.Reset.Anchor)
		params.ResetAnchor = &anchor
	}
	return q.UpsertCatalogLimit(ctx, params)
}

// applyCatalogLimits applies a document's limits, then the limits of each
// product that names them, replacing that product's whole set.
func (s *Service) applyCatalogLimits(ctx context.Context, doc catalogwire.Application) error {
	named := len(doc.Limits) > 0
	for _, p := range doc.Products {
		named = named || p.Limits.Set
	}
	if !named {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	q := s.catalogDatabase().Gen(ctx)
	defs, err := limits.Definitions(ctx, q, mid.UUID())
	if err != nil {
		return err
	}
	known := map[string]*limits.Definition{}
	for i := range defs {
		known[defs[i].Key] = &defs[i]
	}
	for _, key := range slices.Sorted(maps.Keys(doc.Limits)) {
		next, err := declaredLimit(key, known[key], doc.Limits[key])
		if err != nil {
			return err
		}
		if err := writeLimit(ctx, q, mid.UUID(), known[key], next); err != nil {
			return err
		}
		known[key] = &next
	}
	stored, err := q.ListCatalogProductLimits(ctx, mid.UUID())
	if err != nil {
		return err
	}
	for _, key := range slices.Sorted(maps.Keys(doc.Products)) {
		decl := doc.Products[key].Limits
		if !decl.Set {
			continue
		}
		product, err := q.GetProductByKey(ctx, gen.GetProductByKeyParams{MerchantID: mid.UUID(), Key: key})
		if errors.Is(err, pgx.ErrNoRows) {
			continue // a product the document did not create
		}
		if err != nil {
			return err
		}
		if err := setProductLimits(ctx, q, mid.UUID(), product.ID, key, decl.Value, stored, known); err != nil {
			return err
		}
	}
	return nil
}

// setProductLimits makes a product's limits exactly capacities.
func setProductLimits(ctx context.Context, q *gen.Queries, merchantID, productID uuid.UUID, productKey string, capacities map[string]catalogwire.Capacity, stored []gen.ListCatalogProductLimitsRow, known map[string]*limits.Definition) error {
	current := map[string]int64{}
	for _, row := range stored {
		if row.ProductID == productID {
			current[row.LimitKey] = row.Capacity
		}
	}
	for _, limit := range slices.Sorted(maps.Keys(capacities)) {
		if known[limit] == nil {
			return apperr.Invalidf("product %q gives capacity to limit %q, which is not declared", productKey, limit)
		}
		capacity := int64(capacities[limit])
		if old, ok := current[limit]; ok && old == capacity {
			continue
		}
		if err := q.UpsertCatalogProductLimit(ctx, gen.UpsertCatalogProductLimitParams{MerchantID: merchantID, ProductID: productID, LimitKey: limit, Capacity: capacity}); err != nil {
			return err
		}
	}
	for _, limit := range slices.Sorted(maps.Keys(current)) {
		if _, kept := capacities[limit]; kept {
			continue
		}
		if err := q.DeleteCatalogProductLimit(ctx, gen.DeleteCatalogProductLimitParams{MerchantID: merchantID, ProductID: productID, LimitKey: limit}); err != nil {
			return err
		}
	}
	return nil
}

// limitDeclaration is SetLimitParams as a whole declaration.
func limitDeclaration(key string, params billing.SetLimitParams) (limits.Definition, error) {
	decl := catalogwire.ApplyLimit{Unit: catalogwire.Value(params.Unit), Reset: catalogwire.Value(params.Reset), RateCards: catalogwire.Value(params.RateCards)}
	if params.Overrun != "" {
		decl.Overrun = catalogwire.Value(params.Overrun)
	}
	if params.RateCards == nil {
		decl.RateCards = catalogwire.Value([]catalogwire.LimitRateCard{})
	}
	return declaredLimit(key, nil, decl)
}

// SetLimit declares the limit under key whole, as an edit: a document skips
// a field this sets differently.
func (s *Service) SetLimit(ctx context.Context, key string, params billing.SetLimitParams) (*billing.Limit, error) {
	next, err := limitDeclaration(key, params)
	if err != nil {
		return nil, err
	}
	_, err = catalogCommand(ctx, s, "limit/"+key, next, "", limitEditPlan(next, params.ExpectedRevision), func(ctx context.Context, s *Service) (struct{}, error) {
		return catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (struct{}, error) {
			if err := scoped.expectCatalogRevision(limitObject(key), params.ExpectedRevision); err != nil {
				return struct{}{}, err
			}
			mid, err := merchant.Require(ctx)
			if err != nil {
				return struct{}{}, err
			}
			q := scoped.catalogDatabase().Gen(ctx)
			current, err := limits.Get(ctx, q, mid.UUID(), key)
			switch {
			case errors.Is(err, limits.ErrNotFound):
				return struct{}{}, writeLimit(ctx, q, mid.UUID(), nil, next)
			case err != nil:
				return struct{}{}, err
			}
			return struct{}{}, writeLimit(ctx, q, mid.UUID(), &current, next)
		})
	})
	if err != nil {
		return nil, err
	}
	return s.GetLimit(ctx, key)
}

// limitEditPlan admits a change to an existing limit; a new one is additive.
func limitEditPlan(next limits.Definition, expected *int64) catalogClassification {
	return func(_ context.Context, _ *Service, state catalogState) ([]catalogObject, error) {
		obj := limitObject(next.Key)
		old := state[obj]
		var revision int64
		if old != nil {
			revision = old.revision
		}
		if err := checkRevision(obj.String(), expected, revision); err != nil {
			return nil, err
		}
		if catalogFieldsChanged(obj, old, catalogLimitValues(next)) {
			return []catalogObject{obj}, nil
		}
		return nil, nil
	}
}

// limitPosition is a limit list's keyset position.
type limitPosition struct {
	Key string `json:"k"`
}

// ListLimits is one page of the merchant's limits, by key.
func (s *Service) ListLimits(ctx context.Context, page billing.PageRequest) (billing.ListPage[billing.Limit], error) {
	var out billing.ListPage[billing.Limit]
	limit, err := pagination.Limit(page)
	if err != nil {
		return out, err
	}
	var position limitPosition
	present, err := pagination.Decode(page.Cursor, &position)
	if err != nil {
		return out, err
	}
	var after *string
	if present {
		if position.Key == "" {
			return out, pagination.ErrInvalidCursor
		}
		after = &position.Key
	}
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return out, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return out, err
	}
	database := s.catalogDatabase()
	err = database.RunInMerchantConn(ctx, func(ctx context.Context) error {
		q := database.Gen(ctx)
		rows, err := q.PageCatalogLimits(ctx, gen.PageCatalogLimitsParams{MerchantID: mid.UUID(), AfterKey: after, FetchLimit: pagination.Fetch(limit)})
		if err != nil {
			return err
		}
		defs := make([]limits.Definition, 0, len(rows))
		for _, r := range rows {
			d, err := limits.Row(r).Decode()
			if err != nil {
				return err
			}
			defs = append(defs, d)
		}
		items, err := limitViews(ctx, q, mid.UUID(), defs)
		if err != nil {
			return err
		}
		out = pagination.Cut(items, limit, func(l billing.Limit) any { return limitPosition{Key: l.Key} })
		return nil
	})
	return out, err
}

// GetLimit reads one limit with the products that give it capacity.
func (s *Service) GetLimit(ctx context.Context, key string) (*billing.Limit, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var out *billing.Limit
	database := s.catalogDatabase()
	err = database.RunInMerchantConn(ctx, func(ctx context.Context) error {
		q := database.Gen(ctx)
		def, err := limits.Get(ctx, q, mid.UUID(), key)
		if err != nil {
			return err
		}
		views, err := limitViews(ctx, q, mid.UUID(), []limits.Definition{def})
		if err != nil {
			return err
		}
		out = &views[0]
		return nil
	})
	return out, err
}

// limitViews are the definitions with the products that give them capacity.
func limitViews(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, defs []limits.Definition) ([]billing.Limit, error) {
	keys := make([]string, 0, len(defs))
	for _, d := range defs {
		keys = append(keys, d.Key)
	}
	products := map[string][]billing.LimitProduct{}
	if len(keys) > 0 {
		rows, err := q.ListCatalogLimitProducts(ctx, gen.ListCatalogLimitProductsParams{MerchantID: merchantID, LimitKeys: keys})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			products[r.LimitKey] = append(products[r.LimitKey], billing.LimitProduct{ProductID: billing.ProductID(r.ProductID), ProductKey: r.ProductKey, Capacity: r.Capacity})
		}
	}
	out := make([]billing.Limit, 0, len(defs))
	for _, d := range defs {
		view := billing.Limit{Key: d.Key, Unit: d.Unit, Reset: d.Reset, Overrun: d.Overrun, RateCards: d.RateCards, Products: products[d.Key], Revision: d.Revision, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt}
		if view.Products == nil {
			view.Products = []billing.LimitProduct{}
		}
		if view.RateCards == nil {
			view.RateCards = []catalogwire.LimitRateCard{}
		}
		out = append(out, view)
	}
	return out, nil
}
