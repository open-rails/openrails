//go:build integration

package critical_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Limits counted usage through rate cards priced in the limit's unit. The
// migration to counts keeps what each per-unit card counted and refuses a
// card counts cannot express.
func TestLimitCountsMigration(t *testing.T) {
	t.Run("OR-J31-S16/stored-limit-rate-cards-become-counts", func(t *testing.T) {
		t.Run("converts-per-unit-cards", migrateLimitCards)
		t.Run("refuses-a-card-counts-cannot-express", refuseLimitCards)
	})
}

func (d *seed) limit(key, unit, cards string) {
	d.t.Helper()
	d.exec("INSERT INTO "+d.s.table("catalog_limits")+" (merchant_id, key, unit, reset_kind, overrun, rate_cards, revision) VALUES ($1, $2, $3, 'lifetime', 'require_estimate', $4::jsonb, 1)", d.m, key, unit, cards)
}

func (s *legacySchema) limitCounts(t *testing.T, d *seed, key string) string {
	t.Helper()
	var counts string
	require.NoError(t, s.pool.QueryRow(t.Context(), "SELECT counts::text FROM "+s.table("catalog_limits")+" WHERE merchant_id = $1 AND key = $2", d.m, key).Scan(&counts))
	return counts
}

func migrateLimitCards(t *testing.T) {
	s := newLegacySchemaBefore(t, "_limit_counts.up.sql")
	d := s.seed(t)
	d.limit("tokens", "usage_units", `[
		{"meter": "input-tokens", "price": {"model": "per_unit", "per_unit": {"unit_amount": "1", "divide_by": 1000, "round": "half_up"}}},
		{"meter": "output-tokens", "filter": {"region": ["eu", "us"]}, "price": {"model": "per_unit", "per_unit": {"divide_by": 1000, "round": "half_up",
			"matrix": {"dimension": "model", "cells": {"small": {"unit_amount": "4"}, "large": {"unit_amount": "10"}, "free": {"unit_amount": "0"}}}}}},
		{"meter": "calls", "filter": {"model": ["large"]}, "price": {"model": "per_unit", "per_unit": {"round": "half_up",
			"matrix": {"dimension": "model", "cells": {"small": {"unit_amount": "2"}, "large": {"unit_amount": "3"}}}}}}
	]`)
	d.limit("seats", "seats", `[]`)
	d.limit("spend", "USD", `[]`)
	d.exec("INSERT INTO "+s.table("catalog_field_owners")+" (merchant_id, object, key, price_key, field, manager, actor, set_at) VALUES ($1, 'limit', 'tokens', '', 'rate_cards', 'apply', 'critical', now())", d.m)
	d.commit()

	require.NoError(t, s.apply(t.Context(), s.full))
	require.JSONEq(t, `[
		{"meter": "input-tokens", "weight": 1, "per": 1000},
		{"meter": "output-tokens", "weight": 10, "per": 1000, "filter": {"region": ["eu", "us"], "model": ["large"]}},
		{"meter": "output-tokens", "weight": 4, "per": 1000, "filter": {"region": ["eu", "us"], "model": ["small"]}},
		{"meter": "calls", "weight": 3, "per": 1, "filter": {"model": ["large"]}}
	]`, s.limitCounts(t, d, "tokens"), "one count per card or priced cell; a cell its filter excludes or that counts nothing is dropped")
	require.JSONEq(t, `[]`, s.limitCounts(t, d, "seats"))
	require.JSONEq(t, `[]`, s.limitCounts(t, d, "spend"))
	var field string
	require.NoError(t, s.pool.QueryRow(t.Context(), "SELECT field FROM "+s.table("catalog_field_owners")+" WHERE merchant_id = $1 AND object = 'limit' AND key = 'tokens'", d.m).Scan(&field))
	require.Equal(t, "counts", field)
}

func refuseLimitCards(t *testing.T) {
	s := newLegacySchemaBefore(t, "_limit_counts.up.sql")
	d := s.seed(t)
	d.limit("tokens", "usage_units", `[{"meter": "input-tokens", "price": {"model": "per_unit", "per_unit": {"unit_amount": "1", "divide_by": 1000, "round": "half_up"}}}]`)
	d.limit("calls", "requests", `[{"meter": "calls", "price": {"model": "tiered", "tiered": {"mode": "graduated", "tiers": [{"up_to": 10, "unit_amount": "1"}, {"up_to": null, "unit_amount": "2"}]}}}]`)
	d.commit()

	err := s.apply(t.Context(), s.full)
	require.ErrorContains(t, err, `limit calls of merchant `+d.m.String()+` has a rate card counts cannot express`)
	var moved bool
	require.NoError(t, s.pool.QueryRow(t.Context(), "SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = $1 AND table_name = 'catalog_limits' AND column_name = 'counts')", s.schema).Scan(&moved))
	require.False(t, moved, "the refused migration changed nothing")
}
