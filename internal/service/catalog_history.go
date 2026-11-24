package service

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"slices"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/history"
)

// catalogKeyRename is one product's key renamed by a replacement.
type catalogKeyRename struct{ product, from, to string }

// catalogHistory is what one catalog mutation changed, as history entries:
// for each object its create, update, archive, unarchive or delete, a
// product's key changes and a price key's move as entries of their own, each
// with the object's revision after: revisions' where the mutation stepped
// it, else after's.
func catalogHistory(before, after catalogState, revisions map[catalogObject]int64, renames []catalogKeyRename, applicationID string) ([]history.Entry, error) {
	objects := slices.Collect(maps.Keys(after))
	for obj := range before {
		if after[obj] == nil {
			objects = append(objects, obj)
		}
	}
	slices.SortFunc(objects, compareCatalogObjects)
	var out []history.Entry
	for _, obj := range objects {
		b, a := before[obj], after[obj]
		entry := history.Entry{ObjectType: billing.LogObjectType(obj.kind), ObjectID: obj.key, Action: billing.LogActionUpdate}
		if obj.kind == billing.CatalogObjectPrice {
			entry.ObjectID = history.PriceObjectID(obj.key, obj.priceKey)
		}
		fields := map[string]history.FieldChange{}
		for _, field := range catalogHistoryFields(obj.kind) {
			bv, av := historyValue(b, field), historyValue(a, field)
			switch {
			case b == nil && a != nil:
				if !emptyCatalogValue(av) {
					fields[field] = history.FieldChange{After: av}
				}
			case a == nil && b != nil:
				if !emptyCatalogValue(bv) {
					fields[field] = history.FieldChange{Before: bv}
				}
			case !bytes.Equal(bv, av):
				fields[field] = history.FieldChange{Before: bv, After: av}
			}
		}
		switch {
		case b == nil:
			entry.Action = billing.LogActionCreate
		case a == nil:
			entry.Action = billing.LogActionDelete
		case fields["archived"].After != nil:
			entry.Action = billing.LogActionUnarchive
			if string(fields["archived"].After) == "true" {
				entry.Action = billing.LogActionArchive
			}
		}
		var revision *int64
		if a != nil {
			r := a.revision
			if stepped, ok := revisions[obj]; ok {
				r = stepped
			}
			revision = &r
		}
		entry.Changes = history.Fields{Version: history.Version, Revision: revision, ApplicationID: applicationID, Fields: fields}
		keys, err := catalogKeyHistory(obj, b, a, revision, renames)
		if err != nil {
			return nil, err
		}
		if a == nil {
			out = append(out, keys...)
		}
		if len(fields) > 0 || b == nil || a == nil {
			out = append(out, entry)
		}
		if a != nil {
			out = append(out, keys...)
		}
		move, moved, err := priceMove(obj, before, after, revision)
		if err != nil {
			return nil, err
		}
		if moved {
			out = append(out, move)
		}
	}
	return out, nil
}

// priceMove is a price key's move when the mutation moved it: to another of
// its prices, or between naming its price and, archived, none. A key names
// nothing while its price or its product is archived, and once its price is
// deleted.
func priceMove(obj catalogObject, before, after catalogState, revision *int64) (history.Entry, bool, error) {
	if obj.kind != billing.CatalogObjectPrice {
		return history.Entry{}, false, nil
	}
	live := func(s *catalogObjectState) bool { return s != nil && string(s.fields["archived"]) == "false" }
	archived := func(state catalogState, s *catalogObjectState) bool {
		product := state[productObject(obj.key)]
		return !live(s) || product == nil || string(product.fields["archived"]) == "true"
	}
	b, a := before[obj], after[obj]
	var named *catalogObjectState
	switch {
	case a != nil && b == nil && live(a),
		a != nil && b != nil && (a.priceID != b.priceID || archived(after, a) != archived(before, b)):
		named = a
	case a == nil && live(b):
		named = b
	default:
		return history.Entry{}, false, nil
	}
	price, err := billing.NewPriceID(obj.key, obj.priceKey, named.version)
	if err != nil {
		return history.Entry{}, false, err
	}
	return history.PriceMoved(obj.key, obj.priceKey, revision, price, a == nil || archived(after, a)), true, nil
}

// catalogHistoryFields are the fields an object's entries carry: its owned
// fields but a product's keys, which have entries of their own, and a price's
// provider plans.
func catalogHistoryFields(kind billing.CatalogObjectKind) []string {
	switch kind {
	case billing.CatalogObjectProduct:
		return slices.DeleteFunc(slices.Clone(catalogFields[kind]), func(f string) bool { return f == "entitlements" })
	case billing.CatalogObjectPrice:
		return append(slices.Clone(catalogFields[kind]), "plans")
	}
	return catalogFields[kind]
}

func historyValue(state *catalogObjectState, field string) json.RawMessage {
	switch {
	case state == nil:
		return nil
	case field == "plans":
		if len(state.hidden) == 0 {
			return json.RawMessage("{}")
		}
		return state.hidden
	}
	return state.fields[field]
}

// catalogKeyHistory is a product's key changes: each rename the mutation
// made, then the keys added and removed.
func catalogKeyHistory(obj catalogObject, before, after *catalogObjectState, revision *int64, renames []catalogKeyRename) ([]history.Entry, error) {
	if obj.kind != billing.CatalogObjectProduct {
		return nil, nil
	}
	held := func(state *catalogObjectState) (map[string]bool, error) {
		out := map[string]bool{}
		if state == nil {
			return out, nil
		}
		var keys []string
		if err := json.Unmarshal(state.fields["entitlements"], &keys); err != nil {
			return nil, err
		}
		for _, k := range keys {
			out[k] = true
		}
		return out, nil
	}
	was, err := held(before)
	if err != nil {
		return nil, err
	}
	is, err := held(after)
	if err != nil {
		return nil, err
	}
	var out []history.Entry
	for _, r := range renames {
		if r.product == obj.key && was[r.from] && !is[r.from] && is[r.to] && !was[r.to] {
			out = append(out, history.KeyRenamed(obj.key, revision, r.from, r.to))
			delete(was, r.from)
			delete(is, r.to)
		}
	}
	var added, removed []string
	for k := range is {
		if !was[k] {
			added = append(added, k)
		}
	}
	for k := range was {
		if !is[k] {
			removed = append(removed, k)
		}
	}
	slices.Sort(added)
	slices.Sort(removed)
	if len(removed) > 0 {
		out = append(out, history.KeysRemoved(obj.key, revision, removed))
	}
	if len(added) > 0 {
		out = append(out, history.KeysAdded(obj.key, revision, added))
	}
	return out, nil
}

// recordCatalogHistory records what the mutation changed, in its
// transaction: the entries commit and roll back with the change.
func (s *Service) recordCatalogHistory(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, after catalogState, revisions map[catalogObject]int64) error {
	entries, err := catalogHistory(s.catalogBefore, after, revisions, s.catalogRenames, s.catalogApplicationID)
	if err != nil || len(entries) == 0 {
		return err
	}
	p := history.ProvenanceAt(ctx, s.now())
	if s.catalogApplying && p.Source != billing.LogSourceCLI {
		p.Source = billing.LogSourceCatalogApply
	}
	return history.Record(ctx, q, history.ChangeLog, merchantID, p, entries...)
}
