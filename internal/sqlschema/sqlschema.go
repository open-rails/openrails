// Package sqlschema relocates OpenRails SQL to the configured Postgres schema.
//
// All SQL (queries, migrations, inline statements) is authored in
// config.DefaultSchema and runs verbatim there. For any other schema one
// token-aware rewrite moves schema references only: qualifiers ("billing".x
// included), the name after SCHEMA, SET search_path values and reg* literals
// ('billing.x'::regclass, to_regclass('billing.x')). Every other literal and
// comment is data and stays byte-identical, so a lock key such as
// 'openrails.default_payment_method:' hashes the same in queries and triggers.
package sqlschema

import (
	"sync"
	"sync/atomic"

	"github.com/open-rails/openrails/internal/config"
)

// Rewrite relocates authored SQL to schema ("" means the default). The schema
// must already be a validated identifier (config validates db.schema).
func Rewrite(sql, schema string) (string, error) {
	if schema == "" || schema == config.DefaultSchema {
		return sql, nil
	}
	return relocate(sql, config.DefaultSchema, schema)
}

// maxCached bounds the cache; the statement set is fixed (sqlc queries plus
// definition-built SQL), so only a runaway caller would reach it.
const maxCached = 8192

// Relocator rewrites statements for one schema, caching each result by input.
// A nil Relocator is the default schema.
type Relocator struct {
	schema string
	cache  sync.Map // sql -> result
	cached atomic.Int64
}

type result struct {
	sql string
	err error
}

// New returns the relocator for schema, or nil for the default schema ("" or
// config.DefaultSchema), where SQL runs verbatim.
func New(schema string) *Relocator {
	if schema == "" || schema == config.DefaultSchema {
		return nil
	}
	return &Relocator{schema: schema}
}

// Schema is the target schema.
func (r *Relocator) Schema() string {
	if r == nil {
		return config.DefaultSchema
	}
	return r.schema
}

// SQL relocates one statement.
func (r *Relocator) SQL(sql string) (string, error) {
	if r == nil {
		return sql, nil
	}
	if v, ok := r.cache.Load(sql); ok {
		res := v.(result)
		return res.sql, res.err
	}
	out, err := relocate(sql, config.DefaultSchema, r.schema)
	if r.cached.Load() < maxCached {
		if _, loaded := r.cache.LoadOrStore(sql, result{out, err}); !loaded {
			r.cached.Add(1)
		}
	}
	return out, err
}
