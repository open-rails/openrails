//go:build integration

package merchants

import (
	"context"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
)

// retirementNeutralTables are merchant-scoped tables that do not block
// retirement. Every other merchant-scoped table must be a MerchantHasActivity
// blocker or reach one through a NOT NULL foreign key.
var retirementNeutralTables = map[string]string{
	"admission_denials_hourly":      "refused-traffic telemetry anyone can create",
	"webhook_health":                "inbound delivery counters include rejected, unsigned requests",
	"webhook_health_daily":          "rollup of webhook_health",
	"dashboard_configs":             "presentation layout",
	"merchant_configurations":       "settings, not an obligation, connection, customer or catalog",
	"merchant_deks":                 "wrapped key material; stored credentials are merchant_secrets",
	"merchant_destructive_policy":   "operator safety switch",
	"destructive_run_before_images": "operator maintenance ledger",
	"maintenance_runs":              "operator maintenance ledger; historical findings do not represent live merchant activity",
	"reconciliation_findings":       "operator maintenance ledger",
	"reconciliation_state":          "reconciliation watermark",
	"rail_mutation_logs":            "history of provider mutations, which need a PSP or custodian",
	"metered_rating_watermarks":     "derived from customer usage",
	"notifications":                 "inbox records derived from other activity",
}

func TestRetirementActivityClassifiesEveryMerchantTable(t *testing.T) {
	ctx := context.Background()
	superDSN, _ := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(ctx, superDSN)
	require.NoError(t, err)
	defer pool.Close()

	sqlText, err := os.ReadFile("../db/queries/merchant_retirement.sql")
	require.NoError(t, err)
	query := string(sqlText)
	query = query[strings.Index(query, "-- name: MerchantHasActivity"):]
	blockers := map[string]bool{}
	for _, m := range regexp.MustCompile(`FROM openrails\.([a-z0-9_]+) WHERE merchant_id`).FindAllStringSubmatch(query, -1) {
		blockers[m[1]] = true
	}
	require.GreaterOrEqual(t, len(blockers), 10, "blocker parsing is broken")

	scoped := map[string]bool{}
	rows, err := pool.Query(ctx, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		JOIN pg_attribute a ON a.attrelid=c.oid AND a.attname='merchant_id' AND NOT a.attisdropped
		WHERE n.nspname='billing' AND c.relkind IN ('r','p')`)
	require.NoError(t, err)
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		scoped[name] = true
	}
	require.NoError(t, rows.Err())
	require.GreaterOrEqual(t, len(scoped), 50, "merchant table derivation is broken")

	// Blockers must reference the merchant row so the retirement lock serializes
	// concurrent activity inserts.
	for table := range blockers {
		require.True(t, scoped[table], "blocker %s is not a merchant-scoped table", table)
		var locked bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_constraint k
			JOIN pg_attribute a ON a.attrelid=k.conrelid AND a.attnum=ANY(k.conkey)
			WHERE k.contype='f' AND k.conrelid=('billing.'||$1)::regclass
			AND k.confrelid='billing.merchants'::regclass AND a.attname='merchant_id')`, table).Scan(&locked))
		require.True(t, locked, "blocker %s has no merchant_id foreign key to billing.merchants", table)
	}

	parents := map[string][]string{}
	rows, err = pool.Query(ctx, `SELECT c.relname, p.relname FROM pg_constraint k
		JOIN pg_class c ON c.oid=k.conrelid JOIN pg_class p ON p.oid=k.confrelid
		JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname='billing' AND k.contype='f' AND k.conrelid<>k.confrelid
		AND NOT EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid=k.conrelid AND a.attnum=ANY(k.conkey) AND NOT a.attnotnull)`)
	require.NoError(t, err)
	for rows.Next() {
		var child, parent string
		require.NoError(t, rows.Scan(&child, &parent))
		parents[child] = append(parents[child], parent)
	}
	require.NoError(t, rows.Err())
	var implied func(table string, seen map[string]bool) bool
	implied = func(table string, seen map[string]bool) bool {
		if blockers[table] {
			return true
		}
		if seen[table] {
			return false
		}
		seen[table] = true
		for _, parent := range parents[table] {
			if implied(parent, seen) {
				return true
			}
		}
		return false
	}

	var unclassified, doubly, stale []string
	for table := range scoped {
		blocked := implied(table, map[string]bool{})
		_, neutral := retirementNeutralTables[table]
		switch {
		case blocked && neutral:
			doubly = append(doubly, table)
		case !blocked && !neutral:
			unclassified = append(unclassified, table)
		}
	}
	for table := range retirementNeutralTables {
		if !scoped[table] {
			stale = append(stale, table)
		}
	}
	sort.Strings(unclassified)
	sort.Strings(doubly)
	sort.Strings(stale)
	require.Empty(t, unclassified, "classify these merchant tables: add a blocker to MerchantHasActivity or a reasoned retirementNeutralTables entry")
	require.Empty(t, doubly, "neutral tables already block retirement")
	require.Empty(t, stale, "neutral entries name tables that no longer exist")
}

// An uncommitted activity insert holds a key-share lock on the merchant row, so
// retirement waits for it and then refuses instead of tombstoning a merchant
// that just became active.
func TestRetireUnusedWaitsForConcurrentActivity(t *testing.T) {
	ctx := context.Background()
	_, appDSN := dbtest.SharedRLSPostgres(t)
	raw, err := pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	defer raw.Close()
	svc, err := NewDirectoryService(db.WrapPool(raw, config.DefaultSchema))
	require.NoError(t, err)

	group := uuid.NewString()
	m, created, err := svc.Provision(ctx, ProvisionRequest{Slug: "retire-race-" + uuid.NewString()[:8], PermissionGroupID: group})
	require.NoError(t, err)
	require.True(t, created)
	released := false
	release := func(context.Context, string) error { released = true; return nil }

	tx, err := raw.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_config('app.merchant_id', $1, true)`, m.ID.String())
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO billing.customers (merchant_id, issuer, id) VALUES ($1, 'test', $2)`, m.ID.UUID(), uuid.NewString())
	require.NoError(t, err)

	type outcome struct {
		res RetireResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := svc.RetireUnused(ctx, m.ID, group, nil, release)
		done <- outcome{res, err}
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		err := raw.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE wait_event_type='Lock' AND query LIKE '%LockMerchantRetirementState%')`).Scan(&waiting)
		return err == nil && waiting
	}, 10*time.Second, 20*time.Millisecond, "retirement must block on the in-flight activity insert")
	select {
	case got := <-done:
		t.Fatalf("retirement finished before the activity insert committed: %+v", got)
	default:
	}
	require.NoError(t, tx.Commit(ctx))

	got := <-done
	require.NoError(t, got.err)
	require.False(t, got.res.Retired)
	require.Equal(t, RetirementRefusedActive, got.res.Refusal)
	require.False(t, released)
	live, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, StatusActive, live.Status)
}
