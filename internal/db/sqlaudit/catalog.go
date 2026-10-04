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
	PrimaryKeys    map[string][]string   // table -> primary key columns
	PartialIndexes map[string]bool       // index name -> a nontrivial predicate restricts membership
	Columns        map[string][]string   // table -> its column names
	Tables         map[string]struct{}   // every table in the billing schema
	PartitionOf    map[string]string     // partition -> its partitioned table
	PartitionKey   map[string]string     // partitioned table -> its partition key column
}

const catalogSQL = `
SELECT c.relname,
       a.attname,
       EXISTS (SELECT 1 FROM pg_index i
                WHERE i.indrelid = c.oid AND a.attnum = ANY (i.indkey::int2[])) AS indexed
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
 WHERE n.nspname = 'billing' AND c.relkind IN ('r', 'p') AND NOT c.relispartition`

// A partition is planned under its own name; the rules reason about its table.
const partitionsSQL = `
SELECT c.relname, p.relname, a.attname
  FROM pg_inherits i
  JOIN pg_class c ON c.oid = i.inhrelid
  JOIN pg_class p ON p.oid = i.inhparent
  JOIN pg_namespace n ON n.oid = p.relnamespace
  JOIN pg_partitioned_table pt ON pt.partrelid = p.oid
  JOIN pg_attribute a ON a.attrelid = p.oid AND a.attnum = pt.partattrs[0]
 WHERE n.nspname = 'billing' AND c.relkind = 'r'`

const uniqueKeysSQL = `
SELECT c.relname, array_agg(a.attname ORDER BY k.ord), i.indisprimary
  FROM pg_index i
  JOIN pg_class c ON c.oid = i.indrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
  CROSS JOIN LATERAL unnest(i.indkey[0:i.indnkeyatts-1]) WITH ORDINALITY AS k(attnum, ord)
  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum
 WHERE n.nspname = 'billing' AND i.indisunique AND i.indpred IS NULL AND NOT c.relispartition
 GROUP BY c.relname, i.indexrelid, i.indisprimary`

const partialIndexesSQL = `
SELECT idx.relname
FROM pg_index i
JOIN pg_class idx ON idx.oid=i.indexrelid
JOIN pg_namespace ns ON ns.oid=idx.relnamespace
WHERE ns.nspname='billing' AND i.indpred IS NOT NULL
  AND pg_get_expr(i.indpred,i.indrelid) <> 'true'
  AND NOT EXISTS (
    SELECT 1 FROM pg_attribute a
    WHERE a.attrelid=i.indrelid AND a.attnotnull
      AND pg_get_expr(i.indpred,i.indrelid)=format('(%I IS NOT NULL)',a.attname)
  )`

func LoadCatalog(ctx context.Context, conn *pgx.Conn) (*Catalog, error) {
	cat := &Catalog{
		MerchantScoped: map[string]bool{},
		Indexed:        map[string]bool{},
		UniqueKeys:     map[string][][]string{},
		PrimaryKeys:    map[string][]string{},
		PartialIndexes: map[string]bool{},
		Columns:        map[string][]string{},
		Tables:         map[string]struct{}{},
		PartitionOf:    map[string]string{},
		PartitionKey:   map[string]string{},
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
		var primary bool
		if err := urows.Scan(&table, &cols, &primary); err != nil {
			return nil, err
		}
		cat.UniqueKeys[table] = append(cat.UniqueKeys[table], cols)
		if primary {
			cat.PrimaryKeys[table] = cols
		}
	}
	if err := urows.Err(); err != nil {
		return nil, err
	}
	prows, err := conn.Query(ctx, partialIndexesSQL)
	if err != nil {
		return nil, err
	}
	defer prows.Close()
	for prows.Next() {
		var name string
		if err := prows.Scan(&name); err != nil {
			return nil, err
		}
		cat.PartialIndexes[name] = true
	}
	if err := prows.Err(); err != nil {
		return nil, err
	}
	parts, err := conn.Query(ctx, partitionsSQL)
	if err != nil {
		return nil, err
	}
	defer parts.Close()
	for parts.Next() {
		var partition, table, key string
		if err := parts.Scan(&partition, &table, &key); err != nil {
			return nil, err
		}
		cat.PartitionOf[partition] = table
		cat.PartitionKey[table] = key
	}
	return cat, parts.Err()
}

// foldPartitions renames every partition scan to its table, so the rules see
// one relation however many partitions the planner expanded it into.
func (c *Catalog) foldPartitions(n planNode) planNode {
	if table, ok := c.PartitionOf[n.RelationName]; ok {
		n.RelationName = table
	}
	for i := range n.Plans {
		n.Plans[i] = c.foldPartitions(n.Plans[i])
	}
	return n
}

// indexedAnywhere reports whether the column is index-backed on any of the
// tables the query touches. Column names in a plan/AST are not schema-qualified,
// so this is deliberately per-query rather than per-table.
func (c *Catalog) indexedAnywhere(col string, relations []string) bool {
	return c.lookup(c.Indexed, col, relations)
}

// capsRowCount reports whether pinning col (plus merchant_id, which every tenant
// query pins, plus any column the query pins to a literal) covers an entire unique key
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
