//go:build cgo

package sqlaudit

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Catalog is the schema facts the rules need, read straight from pg_catalog on
// the vet DB — never hardcoded, so it can't drift from migrations/.
type Catalog struct {
	MerchantScoped map[string]bool       // table -> has a merchant_id column
	Indexed        map[string]bool       // "table.column" -> column is part of some index
	UniqueKeys     map[string][][]string // table -> key column sets of each UNIQUE index
	Columns        map[string][]string   // table -> its column names
	Tables         map[string]struct{}   // every table in the openrails schema
}

const catalogSQL = `
SELECT c.relname,
       a.attname,
       EXISTS (SELECT 1 FROM pg_index i
                WHERE i.indrelid = c.oid AND a.attnum = ANY (i.indkey::int2[])) AS indexed
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
 WHERE n.nspname = 'openrails' AND c.relkind = 'r'`

const uniqueKeysSQL = `
SELECT c.relname, array_agg(a.attname ORDER BY k.ord)
  FROM pg_index i
  JOIN pg_class c ON c.oid = i.indrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
  CROSS JOIN LATERAL unnest(i.indkey[0:i.indnkeyatts-1]) WITH ORDINALITY AS k(attnum, ord)
  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum
 WHERE n.nspname = 'openrails' AND i.indisunique AND i.indpred IS NULL
 GROUP BY c.relname, i.indexrelid`

func LoadCatalog(ctx context.Context, conn *pgx.Conn) (*Catalog, error) {
	cat := &Catalog{
		MerchantScoped: map[string]bool{},
		Indexed:        map[string]bool{},
		UniqueKeys:     map[string][][]string{},
		Columns:        map[string][]string{},
		Tables:         map[string]struct{}{},
	}
	rows, err := conn.Query(ctx, catalogSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var table, col string
		var indexed bool
		if err := rows.Scan(&table, &col, &indexed); err != nil {
			return nil, err
		}
		cat.Tables[table] = struct{}{}
		cat.Columns[table] = append(cat.Columns[table], col)
		if col == "merchant_id" {
			cat.MerchantScoped[table] = true
		}
		if indexed {
			cat.Indexed[table+"."+col] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	urows, err := conn.Query(ctx, uniqueKeysSQL)
	if err != nil {
		return nil, err
	}
	defer urows.Close()
	for urows.Next() {
		var table string
		var cols []string
		if err := urows.Scan(&table, &cols); err != nil {
			return nil, err
		}
		cat.UniqueKeys[table] = append(cat.UniqueKeys[table], cols)
	}
	return cat, urows.Err()
}

// indexedAnywhere reports whether the column is index-backed on any of the
// tables the query touches. Column names in a plan/AST are not schema-qualified,
// so this is deliberately per-query rather than per-table.
func (c *Catalog) indexedAnywhere(col string, relations []string) bool {
	return c.lookup(c.Indexed, col, relations)
}

// capsRowCount reports whether pinning col (plus merchant_id, which RLS always
// pins, plus any column the query pins to a literal) covers an entire unique key
// on one of the query's tables. Only then does `col = ANY($n)` cap the result at
// one row per list element. A column that is merely PART of a composite key and
// leaves the rest open (subscriptions.rail alone in UNIQUE(rail,
// rail_subscription_id)) caps nothing.
func (c *Catalog) capsRowCount(col string, s *Structure, relations []string) bool {
	for _, t := range relations {
		for _, key := range c.UniqueKeys[t] {
			covered := true
			for _, k := range key {
				if _, konst := s.EqConsts[k]; k != col && k != "merchant_id" && !konst {
					covered = false
					break
				}
			}
			if covered {
				return true
			}
		}
	}
	return false
}

func (c *Catalog) lookup(m map[string]bool, col string, relations []string) bool {
	for _, t := range relations {
		if m[t+"."+col] {
			return true
		}
	}
	return false
}

func (c *Catalog) anyMerchantScoped(relations []string) bool {
	for _, t := range relations {
		if c.MerchantScoped[t] {
			return true
		}
	}
	return false
}
