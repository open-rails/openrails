package limits

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/catalogrules"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// Meter is a declared meter as a limit's counts read it.
type Meter struct {
	Key           string
	EventType     string
	ValueProperty string
	Aggregation   catalog.Aggregation
	GroupBy       map[string]string
}

// Meters are the merchant's declared meters, by key.
func Meters(ctx context.Context, q *gen.Queries, merchantID uuid.UUID) (map[string]Meter, error) {
	rows, err := q.ListCatalogMeters(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Meter, len(rows))
	for _, r := range rows {
		m := Meter{Key: r.Key, EventType: r.EventType, ValueProperty: propertyKey(r.ValueProperty), Aggregation: catalog.Aggregation(r.Aggregation)}
		if m.EventType == "" {
			m.EventType = r.Key
		}
		if m.ValueProperty == "" {
			m.ValueProperty = r.Key
		}
		if m.Aggregation == "" {
			m.Aggregation = catalog.AggregationSum
		}
		if err := json.Unmarshal(r.GroupBy, &m.GroupBy); err != nil {
			return nil, fmt.Errorf("decode meter %q group_by: %w", r.Key, err)
		}
		out[r.Key] = m
	}
	return out, nil
}

// Usage is what a limit's counts read of one request or event.
type Usage struct {
	EventType  string
	Dimensions map[string]int64
	Metadata   map[string]any
}

// Counts reports whether def counts usage of eventType: a currency limit
// counts every request in its currency (checked by the caller), a named unit
// the event types its counts' meters read.
func Counts(def Definition, meters map[string]Meter, eventType string) bool {
	for _, c := range def.Counts {
		if m, ok := meters[c.Meter]; ok && m.EventType == eventType {
			return true
		}
	}
	return false
}

// Units is what u counts toward def, in its unit: each count whose meter
// reads u's event type and whose filter admits it adds the meter's quantity
// × weight ÷ per, rounded half up as a per-unit price rounds.
func Units(def Definition, meters map[string]Meter, u Usage) (int64, error) {
	var total int64
	for _, c := range def.Counts {
		m, ok := meters[c.Meter]
		if !ok || m.EventType != u.EventType || !admits(m, c.Filter, u) {
			continue
		}
		quantity, err := quantity(m, u)
		if err != nil {
			return 0, err
		}
		if quantity == 0 {
			continue
		}
		units, err := catalogrules.ChargeModel{Kind: catalog.ModelPerUnit, UnitAmount: max(c.Weight, 1), DivideBy: max(c.Per, 1), Round: catalog.RoundHalfUp}.Rate(quantity)
		if err != nil {
			return 0, fmt.Errorf("limit %q meter %q: %w", def.Key, c.Meter, err)
		}
		if total > maxInt64-units {
			return 0, fmt.Errorf("limit %q: units overflow int64", def.Key)
		}
		total += units
	}
	return total, nil
}

const maxInt64 = int64(^uint64(0) >> 1)

// property is the event field a dimension of the meter reads.
func property(m Meter, dimension string) string {
	if p := propertyKey(m.GroupBy[dimension]); p != "" {
		return p
	}
	return dimension
}

// propertyKey is a meter property as events carry it, as money rating reads
// it.
func propertyKey(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "$.")
	p = strings.TrimPrefix(p, "dimensions.")
	return strings.TrimPrefix(p, "metadata.")
}

func admits(m Meter, filter map[string][]string, u Usage) bool {
	for dimension, allowed := range filter {
		value, _ := eventValue(u, property(m, dimension))
		ok := false
		for _, a := range allowed {
			if a == value {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// quantity is 1 for a count meter and the value property for a sum meter,
// read from the dimensions, then the metadata.
func quantity(m Meter, u Usage) (int64, error) {
	switch m.Aggregation {
	case catalog.AggregationCount:
		return 1, nil
	case catalog.AggregationSum:
	default:
		return 0, fmt.Errorf("meter %q: aggregation %q cannot count toward a limit", m.Key, m.Aggregation)
	}
	key := m.ValueProperty
	if v, ok := u.Dimensions[key]; ok {
		if v < 0 {
			return 0, apperr.Invalidf("dimensions.%s must not be negative", key).WithParam("usage.dimensions." + key)
		}
		return v, nil
	}
	raw, ok := u.Metadata[key]
	if !ok || raw == nil {
		return 0, nil
	}
	v, ok := integerOf(raw)
	if !ok || v < 0 {
		return 0, apperr.Invalidf("metadata.%s must be a nonnegative integer", key).WithParam("usage.metadata." + key)
	}
	return v, nil
}

func integerOf(raw any) (int64, bool) {
	switch v := raw.(type) {
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n, err == nil
	}
	text, err := json.Marshal(raw)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(string(text), 10, 64)
	return n, err == nil
}

// eventValue is the text of an event field, from the metadata, then the
// dimensions, and the request parameter it came from.
func eventValue(u Usage, prop string) (string, string) {
	if raw, ok := u.Metadata[prop]; ok && raw != nil {
		var s string
		switch v := raw.(type) {
		case string:
			s = v
		case json.Number:
			s = v.String()
		default:
			b, _ := json.Marshal(v)
			s = string(b)
		}
		if s != "" {
			return s, "usage.metadata." + prop
		}
	}
	if v, ok := u.Dimensions[prop]; ok {
		return strconv.FormatInt(v, 10), "usage.dimensions." + prop
	}
	return "", "usage.metadata." + prop
}
