//go:build cgo && indexprobe

package sqlaudit

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/modules/metrics"
)

// keptUnchosen are non-unique indexes a generic plan on the probe's data
// need not choose, kept for the plan a real deployment makes.
var keptUnchosen = map[string]string{
	"subscriptions_grace_ends_at_idx":                          "dunning past grace across merchants",
	"subscriptions_next_retry_at_rail_idx":                     "dunning due across merchants",
	"subscriptions_current_period_ends_at_next_retry_at_idx":   "a merchant's engine renewals due",
	"products_entitlements_idx":                                "offers by entitlement in a large catalog",
	"notifications_event_type_idx":                             "customer notification list's event_type filter",
	"products_archived_idx":                                    "product list's archived filter",
	"psps_environment_archived_rail_created_at_id_idx":         "PSP list by environment and archived, in page order",
	"provider_mutation_logs_psp_id_idx":                        "external write log's PSP filter",
	"provider_mutation_logs_rail_phase_created_at_idx":         "external write log's rail and phase filters",
	"invoices_status_period_starts_at_id_idx":                  "invoice list by status, in page order",
	"invoices_customer_id_period_from_id_idx":                  "a customer's invoices, in page order",
	"reprice_batches_created_at_id_idx":                        "reprice batch list, in page order",
	"reprice_batches_price_key_created_at_id_idx":              "reprice batch list by price key, in page order",
	"subscription_reprices_created_at_id_idx":                  "subscription reprice list, in page order",
	"rebill_cycles_psp_id_due_at_idx":                          "rebill cycle list by PSP, in due order",
	"payment_methods_custodian_account_updater_checked_at_idx": "account updater work in checked order",
	"payments_metadata_nmi_order_idx":                          "NMI order lookup; a merchant's PSP holds most of its payments",
	"payments_metadata_stripe_invoice_idx":                     "Stripe invoice lookup; a merchant's PSP holds most of its payments",
}

// TestIndexUseProbe fills a disposable copy of the baseline with synthetic
// rows, once with many small merchants and once with a few large ones, plans
// every statement the auditor knows, and fails on a non-unique index no plan
// chooses that is neither a foreign key's only support nor kept above.
// INDEX_PROBE_URL names the database (a superuser connection; CHECKs are
// dropped and rows written); INDEX_PROBE_OUT receives the report.
//
//	go test -tags indexprobe -run TestIndexUseProbe ./internal/db/sqlaudit/
func TestIndexUseProbe(t *testing.T) {
	url, out := os.Getenv("INDEX_PROBE_URL"), os.Getenv("INDEX_PROBE_OUT")
	if url == "" || out == "" {
		t.Skip("INDEX_PROBE_URL and INDEX_PROBE_OUT unset")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	queries, err := LoadQueries(genDir)
	if err != nil {
		t.Fatal(err)
	}
	functions, err := LoadFunctionQueries(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	queries = append(queries, functions...)
	compiled, err := metrics.AuditStatements()
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range compiled {
		queries = append(queries, Query{Name: st.Name, Kind: "metrics", SQL: st.SQL})
	}
	for _, st := range merchantarchive.AuditStatements() {
		queries = append(queries, Query{Name: st.Name, Kind: "archive", SQL: st.SQL})
	}
	parent, err := partitionIndexParents(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}

	used := map[string]map[string]bool{}
	for _, merchants := range []int{50, 5} {
		if err := fill(ctx, conn, merchants); err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{
			`SELECT set_config('openrails.merchant_id', '` + AuditMerchantID + `', false)`,
			`SET search_path = billing, public`,
		} {
			if _, err := conn.Exec(ctx, s); err != nil {
				t.Fatal(err)
			}
		}
		for _, q := range queries {
			if q.bindErr != nil {
				continue
			}
			plan, err := GenericPlan(ctx, conn, q.SQL)
			if err != nil {
				continue
			}
			plan.walk(func(n planNode) {
				if n.IndexName == "" {
					return
				}
				name := n.IndexName
				for parent[name] != "" {
					name = parent[name]
				}
				if used[name] == nil {
					used[name] = map[string]bool{}
				}
				used[name][q.Name] = true
			})
		}
	}

	indexes, err := nonUniqueIndexes(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	var unexpected []string
	present := map[string]bool{}
	for _, ix := range indexes {
		present[ix.name] = true
		by := make([]string, 0, len(used[ix.name]))
		for q := range used[ix.name] {
			by = append(by, q)
		}
		sort.Strings(by)
		verdict := "used"
		switch {
		case len(by) > 0:
		case ix.onlyFKSupport != "":
			verdict = "fk " + ix.onlyFKSupport
		case keptUnchosen[ix.name] != "":
			verdict = "kept: " + keptUnchosen[ix.name]
		default:
			verdict = "unused"
			unexpected = append(unexpected, ix.name+" is never chosen")
		}
		if len(by) > 3 {
			by = append(by[:3], fmt.Sprintf("+%d", len(used[ix.name])-3))
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n", ix.table, ix.name, verdict, strings.Join(by, ","))
	}
	for name := range keptUnchosen {
		if !present[name] {
			unexpected = append(unexpected, name+" no longer exists; drop it from keptUnchosen")
		}
	}
	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	sort.Strings(unexpected)
	for _, u := range unexpected {
		t.Error(u)
	}
}

type probedIndex struct{ name, table, onlyFKSupport string }

func partitionIndexParents(ctx context.Context, conn *pgx.Conn) (map[string]string, error) {
	rows, err := conn.Query(ctx, `SELECT c.relname, p.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid JOIN pg_class p ON p.oid = i.inhparent WHERE c.relkind = 'i'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	parent := map[string]string{}
	for rows.Next() {
		var c, p string
		if err := rows.Scan(&c, &p); err != nil {
			return nil, err
		}
		parent[c] = p
	}
	return parent, rows.Err()
}

// nonUniqueIndexes lists each non-unique index with the foreign keys, if any,
// that no other index on its table leads with.
func nonUniqueIndexes(ctx context.Context, conn *pgx.Conn) ([]probedIndex, error) {
	rows, err := conn.Query(ctx, `
WITH idx AS (
    SELECT i.indexrelid, i.indrelid, i.indisunique, i.indkey::int2[] AS keys
    FROM pg_index i JOIN pg_class c ON c.oid = i.indrelid JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'billing' AND NOT c.relispartition
), supports AS (
    SELECT con.conname, idx.indexrelid, idx.indrelid
    FROM pg_constraint con JOIN idx ON idx.indrelid = con.conrelid
    WHERE con.contype = 'f' AND cardinality(con.conkey) <= cardinality(idx.keys)
      AND (SELECT array_agg(k ORDER BY k) FROM unnest(con.conkey) k)
        = (SELECT array_agg(k ORDER BY k) FROM unnest(idx.keys[0:cardinality(con.conkey)-1]) k)
)
SELECT ic.relname, tc.relname,
       COALESCE((SELECT string_agg(s.conname, ',') FROM supports s
                  WHERE s.indexrelid = idx.indexrelid
                    AND NOT EXISTS (SELECT 1 FROM supports o WHERE o.conname = s.conname AND o.indrelid = s.indrelid AND o.indexrelid <> s.indexrelid)), '')
FROM idx JOIN pg_class ic ON ic.oid = idx.indexrelid JOIN pg_class tc ON tc.oid = idx.indrelid
WHERE NOT idx.indisunique
ORDER BY 2, 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []probedIndex
	for rows.Next() {
		var ix probedIndex
		if err := rows.Scan(&ix.name, &ix.table, &ix.onlyFKSupport); err != nil {
			return nil, err
		}
		out = append(out, ix)
	}
	return out, rows.Err()
}

var enumLiteral = regexp.MustCompile(`'((?:[^']|'')*)'`)
var enumColumn = regexp.MustCompile(`\(?\(?(\w+)\s*(?:= ANY \(ARRAY\[|IN \(|= ')`)

// fill empties the tables and writes rows shaped like their columns: a
// per-row merchant from a pool of the given size, shared customer and parent
// id pools, CHECK enum values, and instants over the last 300 days. CHECKs are
// dropped and triggers skipped; unique keys still hold.
func fill(ctx context.Context, conn *pgx.Conn, merchants int) error {
	exec := func(sql string, args ...any) error {
		if _, err := conn.Exec(ctx, sql, args...); err != nil {
			first := sql
			if i := strings.IndexByte(first, '\n'); i >= 0 {
				first = first[:i]
			}
			return fmt.Errorf("%.120s: %w", first, err)
		}
		return nil
	}
	if err := exec(`SET session_replication_role = replica`); err != nil {
		return err
	}
	type check struct{ table, name, def string }
	var checks []check
	rows, err := conn.Query(ctx, `SELECT c.relname, con.conname, pg_get_constraintdef(con.oid) FROM pg_constraint con JOIN pg_class c ON c.oid = con.conrelid JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'billing' AND con.contype = 'c' AND NOT c.relispartition`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var c check
		if err := rows.Scan(&c.table, &c.name, &c.def); err != nil {
			return err
		}
		checks = append(checks, c)
	}
	rows.Close()
	enums := map[string][]string{}
	for _, c := range checks {
		if !strings.Contains(c.def, " OR ") && !strings.Contains(c.def, " AND ") {
			if m := enumColumn.FindStringSubmatch(c.def); m != nil {
				for _, lit := range enumLiteral.FindAllStringSubmatch(c.def, -1) {
					enums[c.table+"."+m[1]] = append(enums[c.table+"."+m[1]], lit[1])
				}
			}
		}
		if err := exec(fmt.Sprintf(`ALTER TABLE billing.%s DROP CONSTRAINT %s`, pgx.Identifier{c.table}.Sanitize(), pgx.Identifier{c.name}.Sanitize())); err != nil {
			return err
		}
	}
	for _, p := range []string{"usage_events", "admission_operations"} {
		if err := exec(`SELECT billing.ensure_month_partitions($1, now() - interval '11 months', now() + interval '2 months')`, p); err != nil {
			return err
		}
	}

	size := map[string]int{"merchants": merchants, "customers": 10000}
	for _, t := range strings.Fields(`ledger_transfers grants entitlements usage_events admission_operations payments payment_attempts provider_intents provider_mutation_logs notifications subscription_status_transitions webhook_events idempotency_keys host_outbox checkout_attempts rebill_cycles cost_observations invoice_items solana_pay_receipts`) {
		size[t] = 40000
	}
	for _, t := range strings.Fields(`products prices price_psp_bindings merchant_configuration_applications catalog_applications billing_policies billing_policy_bindings catalog_meters catalog_rate_cards reconciliation_state worker_state maintenance_runs merchant_webhooks merchant_slug_aliases merchant_destructive_policy destructive_action_switch price_key_movements psp_refresh_watermarks reprice_batches`) {
		size[t] = 1000
	}
	// A merchant has a PSP or two and one custodian of each kind.
	for t, per := range map[string]int{"psps": 2, "custodians": 1, "merchant_configurations": 1} {
		size[t] = per * merchants
	}
	n := func(table string) int {
		if v, ok := size[table]; ok {
			return v
		}
		return 10000
	}

	fkParent := map[string]string{}
	rows, err = conn.Query(ctx, `
SELECT c.relname, a.attname, pc.relname
FROM pg_constraint con
JOIN pg_class c ON c.oid = con.conrelid
JOIN pg_class pc ON pc.oid = con.confrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
CROSS JOIN LATERAL unnest(con.conkey, con.confkey) AS k(child, par)
JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.child
JOIN pg_attribute pa ON pa.attrelid = pc.oid AND pa.attnum = k.par
WHERE n.nspname = 'billing' AND con.contype = 'f' AND a.attname <> 'merchant_id' AND pa.attname = 'id'`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var table, col, parent string
		if err := rows.Scan(&table, &col, &parent); err != nil {
			return err
		}
		fkParent[table+"."+col] = parent
	}
	rows.Close()

	unique := map[string]bool{}
	rows, err = conn.Query(ctx, `SELECT c.relname, a.attname FROM pg_index i JOIN pg_class c ON c.oid = i.indrelid JOIN pg_namespace n ON n.oid = c.relnamespace JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = ANY (i.indkey::int2[]) WHERE n.nspname = 'billing' AND i.indisunique AND NOT c.relispartition`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var table, col string
		if err := rows.Scan(&table, &col); err != nil {
			return err
		}
		unique[table+"."+col] = true
	}
	rows.Close()

	var tables []string
	rows, err = conn.Query(ctx, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'billing' AND c.relkind IN ('r', 'p') AND NOT c.relispartition ORDER BY 1`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return err
		}
		tables = append(tables, table)
	}
	rows.Close()
	var quoted []string
	for _, table := range tables {
		quoted = append(quoted, "billing."+pgx.Identifier{table}.Sanitize())
	}
	if err := exec(`TRUNCATE ` + strings.Join(quoted, ", ")); err != nil {
		return err
	}

	idOf := func(table, k string) string { return fmt.Sprintf(`md5('%s:' || (%s))::uuid`, table, k) }
	merchant := fmt.Sprintf(`('00000000-0000-4000-8000-' || lpad(((g * 7919) %% %d)::text, 12, '0'))::uuid`, merchants)
	customer := fmt.Sprintf(`('00000000-0000-4000-9000-' || lpad(((g * 104729) %% %d)::text, 12, '0'))::uuid`, n("customers"))
	for _, table := range tables {
		type column struct {
			name, typ string
			notNull   bool
		}
		var cols []column
		rows, err := conn.Query(ctx, `SELECT a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'billing' AND c.relname = $1 AND a.attnum > 0 AND NOT a.attisdropped AND a.attgenerated = '' ORDER BY a.attnum`, table)
		if err != nil {
			return err
		}
		for rows.Next() {
			var c column
			if err := rows.Scan(&c.name, &c.typ, &c.notNull); err != nil {
				return err
			}
			cols = append(cols, c)
		}
		rows.Close()
		var names, exprs []string
		for i, c := range cols {
			key := table + "." + c.name
			var e string
			switch {
			case c.name == "id" && table == "merchants":
				e = `('00000000-0000-4000-8000-' || lpad((g - 1)::text, 12, '0'))::uuid`
			case c.name == "merchant_id":
				e = merchant
			case c.name == "customer_id" || (table == "customers" && c.name == "id"):
				e = customer
			case c.name == "id" && strings.HasPrefix(c.typ, "uuid"):
				e = idOf(table, "g")
			case fkParent[key] != "" && strings.HasPrefix(c.typ, "uuid"):
				p := fkParent[key]
				if unique[key] {
					e = idOf(p, "g")
				} else {
					e = idOf(p, fmt.Sprintf("((g * %d) %% %d) + 1", 31+i, n(p)))
				}
			case strings.HasPrefix(c.typ, "uuid"):
				if unique[key] {
					e = fmt.Sprintf(`md5('%s:' || g)::uuid`, key)
				} else {
					e = fmt.Sprintf(`md5('%s:' || (g %% 5000))::uuid`, key)
				}
			case c.typ == "text" || strings.HasPrefix(c.typ, "character"):
				switch {
				case len(enums[key]) > 0:
					var arr []string
					for _, v := range enums[key] {
						arr = append(arr, "'"+strings.ReplaceAll(v, "'", "''")+"'")
					}
					e = fmt.Sprintf(`(ARRAY[%s])[((g * %d) %% %d) + 1]`, strings.Join(arr, ","), 17+i, len(arr))
				case c.name == "currency":
					e = `(ARRAY['USD','EUR'])[(g % 2) + 1]`
				case unique[key]:
					e = fmt.Sprintf(`'%s-' || g`, c.name)
				default:
					e = fmt.Sprintf(`'%s-' || ((g * %d) %% 1000)`, c.name, 13+i)
				}
			case c.typ == "bigint" || c.typ == "integer" || c.typ == "smallint" || strings.HasPrefix(c.typ, "numeric"):
				if unique[key] {
					e = "g::" + c.typ
				} else {
					e = fmt.Sprintf(`((g * %d) %% 1000)::%s`, 13+i, c.typ)
				}
			case c.typ == "boolean":
				e = fmt.Sprintf(`((g + %d) %% 2 = 0)`, i)
			case strings.HasPrefix(c.typ, "timestamp"):
				if unique[key] {
					e = `now() - g * interval '1 second'`
				} else {
					e = fmt.Sprintf(`now() - ((g * %d) %% 25920000) * interval '1 second'`, 7919+i)
				}
			case c.typ == "date":
				e = `current_date - (g % 300)::int`
			case c.typ == "interval":
				e = `interval '1 hour'`
			case c.typ == "jsonb" || c.typ == "json":
				e = `'{}'::` + c.typ
			case c.typ == "bytea":
				e = `decode(md5(g::text) || md5(g::text), 'hex')`
			case strings.HasSuffix(c.typ, "[]"):
				e = `'{}'::` + c.typ
			case c.typ == "inet":
				e = `'127.0.0.1'::inet`
			default:
				if c.notNull {
					return fmt.Errorf("%s: no generator for %s", key, c.typ)
				}
				continue
			}
			if !c.notNull && !unique[key] {
				e = fmt.Sprintf(`CASE WHEN (g + %d) %% 10 < 3 THEN NULL ELSE %s END`, i, e)
			}
			names = append(names, pgx.Identifier{c.name}.Sanitize())
			exprs = append(exprs, e)
		}
		if err := exec(fmt.Sprintf(`INSERT INTO billing.%s (%s) SELECT %s FROM generate_series(1::bigint, %d) g ON CONFLICT DO NOTHING`,
			pgx.Identifier{table}.Sanitize(), strings.Join(names, ", "), strings.Join(exprs, ", "), n(table))); err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
		if err := exec(`CHECKPOINT`); err != nil {
			return err
		}
	}
	return exec(`ANALYZE`)
}
