//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/migrate"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
	"github.com/open-rails/openrails/internal/modules/metrics"
	"github.com/open-rails/openrails/internal/sqlschema"
)

// An upgrade to MRR read from its facts keeps every MRR measure of a book
// the rollups recorded.
func TestMRRUpgrade(t *testing.T) {
	t.Run("OR-J42-S37/an-upgrade-keeps-every-mrr-measure-of-a-populated-database", upgradeKeepsEveryMRRMeasure)
}

// rollupReads is critical/testdata/mrr_rollup_reads.json: the statements the
// rollups answered each plan with, as the build before the upgrade compiled
// them for its merchant, at its instant.
type rollupReads struct {
	Merchant uuid.UUID `json:"merchant_id"`
	Now      time.Time `json:"now"`
	SQL      []string  `json:"sql"`
	Plans    []struct {
		Zone       string                     `json:"zone"`
		Query      billing.QueryMetricsParams `json:"query"`
		Statements []struct {
			SQL  int `json:"sql"`
			Args []struct {
				Kind  string   `json:"kind"`
				UUID  string   `json:"uuid"`
				Time  string   `json:"time"`
				Times []string `json:"times"`
				Text  string   `json:"text"`
				Texts []string `json:"texts"`
			} `json:"args"`
		} `json:"statements"`
	} `json:"plans"`
}

// metricCells runs a plan's statements and keys each nonzero leaf cell by
// its leaf, bucket and dimensions.
func metricCells(t *testing.T, pool *pgxpool.Pool, schema string, statements []metrics.Statement, cells map[string]string) {
	t.Helper()
	for _, st := range statements {
		sql, err := sqlschema.Rewrite(st.SQL, schema)
		require.NoError(t, err)
		rows, err := pool.Query(t.Context(), sql, st.Args...)
		require.NoError(t, err, sql)
		fields := rows.FieldDescriptions()
		for rows.Next() {
			values, err := rows.Values()
			require.NoError(t, err)
			var key []string
			leaves := map[string]any{}
			skip := false
			for i, f := range fields {
				switch name := f.Name; {
				case name == "bucket":
					if values[i] == nil {
						skip = true
						continue
					}
					key = append(key, values[i].(time.Time).UTC().Format(time.RFC3339))
				case strings.HasPrefix(name, "dim_"):
					if values[i] == nil {
						skip = true
						continue
					}
					key = append(key, name+"="+fmt.Sprint(values[i]))
				case strings.HasPrefix(name, "m_"):
					leaves[strings.TrimPrefix(name, "m_")] = values[i]
				}
			}
			if skip {
				continue
			}
			for leaf, v := range leaves {
				if v == nil || fmt.Sprint(v) == "0" {
					continue
				}
				k := leaf + " " + strings.Join(key, " ")
				require.NotContains(t, cells, k, "one cell per leaf, bucket and dimensions")
				cells[k] = fmt.Sprint(v)
			}
		}
		require.NoError(t, rows.Err())
		rows.Close()
	}
}

func upgradeKeepsEveryMRRMeasure(t *testing.T) {
	ctx := t.Context()
	dsn := os.Getenv("OPENRAILS_CRITICAL_DSN")
	require.NotEmpty(t, dsn, "OPENRAILS_CRITICAL_DSN must name a disposable PostgreSQL database; critical scenarios never skip")
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	schema := criticalSchema()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
		pool.Close()
	})
	raw, err := os.ReadFile("testdata/mrr_rollup_reads.json")
	require.NoError(t, err)
	var reads rollupReads
	require.NoError(t, json.Unmarshal(raw, &reads))
	logger := slog.New(slog.DiscardHandler)

	// The chain the rollups shipped in: everything before MRR was read from
	// its facts.
	earlier := fstest.MapFS{}
	names, err := fs.Glob(postgresmigrations.FS, "*.up.sql")
	require.NoError(t, err)
	for _, name := range names {
		if name >= "0013_" {
			continue
		}
		data, err := fs.ReadFile(postgresmigrations.FS, name)
		require.NoError(t, err)
		earlier[name] = &fstest.MapFile{Data: data}
	}
	require.NoError(t, migrate.Apply(ctx, pool, migrate.Options{Schema: schema, Chain: earlier, Logger: logger}))

	table := func(name string) string { return pgx.Identifier{schema, name}.Sanitize() }
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err, sql)
	}
	m := reads.Merchant
	id := func(n int) uuid.UUID { return uuid.MustParse(fmt.Sprintf("0193f000-1422-7000-8000-%012d", n)) }
	psp, core, addon := id(2), uuid.MustParse("0193f000-1422-7000-8000-0000000000a1"), uuid.MustParse("0193f000-1422-7000-8000-0000000000a2")
	exec("INSERT INTO "+table("merchants")+" (id, slug, status) VALUES ($1, 'upgrade-1422', 'active')", m)
	exec("INSERT INTO "+table("psps")+" (id, merchant_id, key, rail, environment, account_id) VALUES ($1, $2, 'stripe', 'stripe', 'test', 'acct_upgrade1422')", psp, m)
	exec("INSERT INTO "+table("products")+" (id, merchant_id, key, display_name, tier_rank, archived) VALUES ($1, $2, 'core', 'Core', 0, false)", core, m)
	exec("INSERT INTO "+table("products")+" (id, merchant_id, key, display_name, tier_rank, archived) VALUES ($1, $2, 'addon', 'Add-on', 0, false)", addon, m)
	type price struct {
		id      uuid.UUID
		product uuid.UUID
	}
	prices := map[string]price{}
	for i, p := range []struct {
		key, currency string
		amount        int64
		product       uuid.UUID
	}{{"basic-USD", "USD", 10_000_000, core}, {"pro-USD", "USD", 25_000_000, core}, {"basic-EUR", "EUR", 9_000_000, core}, {"pro-EUR", "EUR", 22_000_000, core},
		{"addon-USD", "USD", 5_000_000, addon}} {
		prices[p.key] = price{id(100 + i), p.product}
		exec("INSERT INTO "+table("prices")+" (id, merchant_id, product_id, key, amount, currency, archived, billing_interval_months) VALUES ($1, $2, $3, $4, $5, $6, false, 1)",
			id(100+i), m, p.product, p.key, p.amount, p.currency)
	}
	at := func(month time.Month, day int) time.Time { return time.Date(2026, month, day, 10, 0, 0, 0, time.UTC) }

	// The schema's own writes, each one transaction, as every writer makes
	// them: the triggers keep MRR.
	customers := map[string]uuid.UUID{}
	cards := map[string]uuid.UUID{}
	subscribe := func(customer string, sub int, key string, start, end time.Time) {
		t.Helper()
		p := prices[key]
		exec("INSERT INTO "+table("subscriptions")+` (id, merchant_id, customer_id, price_id, product_id, status, rail, collection_policy, payment_method_id,
			current_period_starts_at, current_period_ends_at, started_at, psp_id, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, 'active', 'stripe', 'engine', $6, $7, $8, $7, $9, $7, $7)`,
			id(sub), m, customers[customer], p.id, p.product, cards[customer], start, end, psp)
	}
	arrive := func(customer string, n, sub int, key string, start, end time.Time) {
		t.Helper()
		customers[customer], cards[customer] = id(1000+n), id(2000+n)
		exec("INSERT INTO "+table("customers")+" (id, merchant_id, created_at, last_seen_at) VALUES ($1, $2, $3, $3)", customers[customer], m, start)
		exec("INSERT INTO "+table("payment_methods")+` (id, merchant_id, customer_id, rail, psp_id, custodian, card_brand, card_last4,
			card_exp_month, card_exp_year, charge_via, status, created_at, updated_at)
			VALUES ($1, $2, $3, 'stripe', $4, 'psp', 'visa', '4242', 12, 2032, 'pan_proxy', 'active', $5, $5)`, cards[customer], m, customers[customer], psp, start)
		subscribe(customer, sub, key, start, end)
	}
	set := func(sub int, when time.Time, status, key string, start, end time.Time, cancel any, ended any) {
		t.Helper()
		exec("UPDATE "+table("subscriptions")+` SET status = $3, price_id = $4, current_period_starts_at = $5, current_period_ends_at = $6, cancel_type = $7,
			canceled_at = CASE WHEN $3 = 'canceled' THEN $8 END, ended_at = $9, lifecycle_rev = lifecycle_rev + 1, updated_at = $8
			WHERE merchant_id = $1 AND id = $2`, m, id(sub), status, prices[key].id, start, end, cancel, when, ended)
	}

	// An upgrade, then a cancellation at period end its job took.
	arrive("c1", 1, 1, "basic-USD", at(1, 5), at(2, 5))
	set(1, at(2, 10), "active", "pro-USD", at(2, 5), at(3, 5), nil, nil)
	set(1, at(5, 20), "canceled", "pro-USD", at(5, 5), at(6, 5), "user", at(6, 5))
	// An add-on, then the plan canceled at an end still ahead.
	arrive("c2", 2, 2, "basic-USD", at(1, 12), at(2, 12))
	subscribe("c2", 3, "addon-USD", at(2, 1), at(3, 1))
	set(2, at(7, 14), "canceled", "basic-USD", at(7, 12), at(8, 12), "user", at(8, 12))
	// Euros: past due and recovered, then past due again, still.
	arrive("c3", 3, 4, "basic-EUR", at(2, 3), at(3, 3))
	set(4, at(4, 3), "past_due", "basic-EUR", at(3, 3), at(4, 3), nil, nil)
	set(4, at(4, 6), "active", "basic-EUR", at(4, 3), at(5, 3), nil, nil)
	set(4, at(6, 3), "past_due", "basic-EUR", at(5, 3), at(6, 3), nil, nil)
	// The merchant ends a plan; the customer comes back.
	arrive("c4", 4, 5, "pro-USD", at(1, 20), at(2, 20))
	set(5, at(3, 15), "canceled", "pro-USD", at(2, 20), at(3, 20), "merchant", at(3, 15))
	subscribe("c4", 6, "basic-USD", at(5, 2), at(6, 2))
	// A cancellation at period end whose end came before the upgrade.
	arrive("c5", 5, 7, "basic-USD", at(3, 1), at(4, 1))
	set(7, at(6, 20), "canceled", "basic-USD", at(6, 1), at(7, 1), "user", at(7, 1))
	// Waiting for a card, then expired: churn at risk.
	arrive("c6", 6, 8, "basic-USD", at(2, 15), at(3, 15))
	set(8, at(5, 15), "awaiting_method", "basic-USD", at(4, 15), at(5, 15), nil, nil)
	set(8, at(6, 1), "canceled", "basic-USD", at(4, 15), at(5, 15), "expired", at(6, 1))
	// Euros: the only plan canceled at an end still ahead.
	arrive("c7", 7, 9, "pro-EUR", at(4, 10), at(5, 10))
	set(9, at(7, 10), "canceled", "pro-EUR", at(7, 10), at(8, 10), "user", at(8, 10))
	// A cancellation at period end resumed.
	arrive("c8", 8, 10, "basic-USD", at(3, 10), at(4, 10))
	set(10, at(4, 20), "canceled", "basic-USD", at(4, 10), at(5, 10), "user", at(5, 10))
	set(10, at(4, 25), "active", "basic-USD", at(4, 10), at(5, 10), nil, nil)
	// A card that expires before the renewal it pays for.
	arrive("c9", 9, 11, "basic-USD", at(3, 20), at(4, 20))
	exec("UPDATE "+table("payment_methods")+" SET card_exp_month = 6, card_exp_year = 2026, updated_at = $3 WHERE merchant_id = $1 AND id = $2", m, cards["c9"], at(6, 22))
	set(11, at(6, 22), "active", "basic-USD", at(6, 20), at(7, 20), nil, nil)
	// A downgrade scheduled for the renewal ahead.
	arrive("c10", 10, 12, "pro-USD", at(4, 1), at(5, 1))
	set(12, at(7, 1), "active", "pro-USD", at(7, 1), at(8, 1), nil, nil)
	exec("INSERT INTO "+table("scheduled_changes")+` (id, merchant_id, subscription_id, from_price_id, price_id, effective_at, source, status, acknowledged_short_notice, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'change', 'scheduled', false, $7)`, id(300), m, id(12), prices["pro-USD"].id, prices["basic-USD"].id, at(8, 1), at(7, 5))
	// A customer delinquent in dollars.
	arrive("c11", 11, 13, "basic-USD", at(2, 20), at(3, 20))
	exec("INSERT INTO "+table("customer_delinquency")+` (merchant_id, customer_id, currency, state, overdue_started_at, entered_at, overdue_amount,
		overdue_invoices, transition_seq, evaluated_at, created_at, updated_at) VALUES ($1, $2, 'USD', 'delinquent', $3, $3, 20000000, 1, 1, $3, $3, $3)`,
		m, customers["c11"], at(6, 10))
	// Euros, plainly.
	arrive("c12", 12, 14, "basic-EUR", at(5, 15), at(6, 15))

	// As the build before ran its jobs: each end that came, and the risk
	// changes queued.
	exec("SELECT "+table("effect_due_mrr")+"(merchant_id, subscription_id, $2) FROM "+table("subscription_mrr")+" WHERE merchant_id = $1 AND next_change_at <= $2",
		m, reads.Now)
	exec("SELECT "+table("effect_due_mrr_risk")+"($1, 100000, NULL)", m)

	// Every MRR measure, as the rollups answered it.
	zones := map[string]*time.Location{}
	before := make([]map[string]string, len(reads.Plans))
	for i, plan := range reads.Plans {
		var statements []metrics.Statement
		for _, st := range plan.Statements {
			var args []any
			for _, a := range st.Args {
				switch a.Kind {
				case "uuid":
					args = append(args, uuid.MustParse(a.UUID))
				case "time":
					v, err := time.Parse(time.RFC3339Nano, a.Time)
					require.NoError(t, err)
					args = append(args, v)
				case "times":
					vs := []time.Time{}
					for _, s := range a.Times {
						v, err := time.Parse(time.RFC3339Nano, s)
						require.NoError(t, err)
						vs = append(vs, v)
					}
					args = append(args, vs)
				case "text":
					args = append(args, a.Text)
				case "texts":
					args = append(args, a.Texts)
				default:
					t.Fatalf("argument kind %s", a.Kind)
				}
			}
			statements = append(statements, metrics.Statement{SQL: reads.SQL[st.SQL], Args: args})
		}
		before[i] = map[string]string{}
		metricCells(t, pool, schema, statements, before[i])
		if _, ok := zones[plan.Zone]; !ok {
			zone, err := time.LoadLocation(plan.Zone)
			require.NoError(t, err)
			zones[plan.Zone] = zone
		}
	}
	measured := 0
	for _, cells := range before {
		measured += len(cells)
	}
	require.Greater(t, measured, 500, "the book moves every measure")

	require.NoError(t, migrate.Apply(ctx, pool, migrate.Options{Schema: schema, Logger: logger}), "an upgrade over the rollups' book")

	// The same measures from the facts.
	for i, plan := range reads.Plans {
		q := plan.Query
		validated, refused := metrics.ValidateIn(&q, reads.Now, zones[plan.Zone])
		require.Nil(t, refused, "%+v", plan.Query)
		statements, err := metrics.Statements(validated, m)
		require.NoError(t, err)
		after := map[string]string{}
		metricCells(t, pool, schema, statements, after)
		if !maps.Equal(before[i], after) {
			var differ []string
			for k, v := range before[i] {
				if after[k] != v {
					differ = append(differ, fmt.Sprintf("%s: rollups %s, facts %q", k, v, after[k]))
				}
			}
			for k, v := range after {
				if _, ok := before[i][k]; !ok {
					differ = append(differ, fmt.Sprintf("%s: rollups none, facts %s", k, v))
				}
			}
			sort.Strings(differ)
			t.Errorf("%s %+v in %s:\n%s", plan.Query.Measures[0], plan.Query, plan.Zone, strings.Join(differ, "\n"))
		}
	}

	// The cancellations whose ends are ahead are movements dated there:
	// churn alone, contraction beside the add-on.
	scheduled := func() []string {
		t.Helper()
		rows, err := pool.Query(ctx, "SELECT currency || ' ' || kind || ' ' || delta || ' ' || logo_delta || ' ' || to_char(occurred_at AT TIME ZONE 'UTC', 'MM-DD') FROM "+
			table("mrr_movements")+" WHERE merchant_id = $1 AND scheduled ORDER BY occurred_at", m)
		require.NoError(t, err)
		out, err := pgx.CollectRows(rows, pgx.RowTo[string])
		require.NoError(t, err)
		return out
	}
	require.Equal(t, []string{"EUR churn -22000000 -1 08-10", "USD contraction -10000000 0 08-12"}, scheduled())
	var astray int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM "+table("mrr_movement_lines")+" l JOIN "+table("mrr_movements")+
		" v ON v.merchant_id = l.merchant_id AND v.id = l.movement_id WHERE l.merchant_id = $1 AND (l.occurred_at <> v.occurred_at OR l.currency <> v.currency)", m).Scan(&astray))
	require.Zero(t, astray, "each line carries its movement's instant and currency")
	cases := func() map[string]string {
		t.Helper()
		rows, err := pool.Query(ctx, "SELECT c.entry_reason, COALESCE(c.outcome, 'open'), (SELECT count(*) FROM "+table("mrr_risk_episodes")+
			" e WHERE e.merchant_id = c.merchant_id AND e.case_id = c.id AND e.reason = 'any') FROM "+table("mrr_risk_cases")+" c WHERE c.merchant_id = $1", m)
		require.NoError(t, err)
		out := map[string]string{}
		for rows.Next() {
			var reason, outcome string
			var any int
			require.NoError(t, rows.Scan(&reason, &outcome, &any))
			require.Equal(t, 1, any, "each case has its episode of reason any")
			out[reason] = strings.TrimSpace(out[reason] + " " + outcome)
		}
		require.NoError(t, rows.Err())
		return out
	}
	got := cases()
	for _, reason := range []string{"card_expiring", "delinquent"} {
		require.Equal(t, "superseded", got[reason], "%s ends at the upgrade, counted in no recovery rate", reason)
	}
	require.ElementsMatch(t, []string{"recovered", "open"}, strings.Fields(got["past_due"]), "the status cases go on")
	require.Equal(t, "churned", got["needs_payment_method"])
	var stray int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM "+table("subscription_mrr")+" WHERE merchant_id = $1 AND NOT (risk || recorded_risk) <@ ARRAY['past_due', 'needs_payment_method', 'unverified']::text[]",
		m).Scan(&stray))
	require.Zero(t, stray, "a subscription's reasons are its status's")

	// A write after the upgrade settles on the converted book: the add-on's
	// customer resumes, and the contraction ahead goes.
	set(2, at(7, 15), "active", "basic-USD", at(7, 12), at(8, 12), nil, nil)
	require.Equal(t, []string{"EUR churn -22000000 -1 08-10"}, scheduled())
}
