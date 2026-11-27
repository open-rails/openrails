// Command measure1418 measures OpenRails' MRR measures read from the rollups
// against the same measures read from the MRR facts at query time, on a
// synthetic merchant whose history is written through the schema's own
// triggers. Throwaway: it runs in CI on a disposable PostgreSQL.
package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/internal/migrate"
)

//go:embed sim.sql
var simSQL string

//go:embed settle_facts.sql
var settleFactsSQL string

//go:embed proto.sql
var protoSQL string

//go:embed proto_indexes.sql
var protoIndexesSQL string

//go:embed month_close.sql
var monthCloseSQL string

type config struct {
	dsn          string
	customers    int
	years        int
	seed         int64
	out          string
	runs         int
	genBudget    time.Duration
	writeSamples int
}

func main() {
	var c config
	flag.StringVar(&c.dsn, "dsn", os.Getenv("MEASURE_DSN"), "disposable PostgreSQL")
	flag.IntVar(&c.customers, "customers", 10000, "monthly paying customers to hold")
	flag.IntVar(&c.years, "years", 3, "years of history")
	flag.Int64Var(&c.seed, "seed", 1418, "random seed")
	flag.StringVar(&c.out, "out", "measure-out", "output directory")
	flag.IntVar(&c.runs, "runs", 7, "timed runs per query")
	flag.DurationVar(&c.genBudget, "gen-budget", 0, "stop generating history after this long (0: none)")
	flag.IntVar(&c.writeSamples, "write-samples", 200, "subscription changes timed per kind and arm")
	dump := flag.String("dump-sql", "", "write every read's statements to this directory and exit")
	flag.Parse()
	if *dump != "" {
		if err := dumpSQL(*dump); err != nil {
			log.Fatal(err)
		}
		return
	}
	if c.dsn == "" {
		log.Fatal("-dsn is required")
	}
	if err := run(context.Background(), c); err != nil {
		log.Fatal(err)
	}
}

// world is the seeded merchant and its catalog.
type world struct {
	merchant uuid.UUID
	psp      uuid.UUID
	core     uuid.UUID // product with three tiers
	addon    uuid.UUID // the add-on product
	// prices[currency][tier]; tier 3 is the add-on.
	prices  [2][4]uuid.UUID
	tierOf  map[uuid.UUID]int
	amounts [2][4]int64
}

var currencies = [2]string{"USD", "EUR"}

func run(ctx context.Context, c config) error {
	if err := os.MkdirAll(c.out, 0o755); err != nil {
		return err
	}
	rep := newReport(c)
	cfg, err := pgxpool.ParseConfig(c.dsn)
	if err != nil {
		return err
	}
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	step := func(name string) func() {
		start := time.Now()
		logf("== %s", name)
		return func() { logf("== %s done in %s", name, time.Since(start).Round(time.Millisecond)) }
	}

	done := step("migrate")
	if err := migrate.Apply(ctx, pool, migrate.Options{Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))}); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if _, err := pool.Exec(ctx, simSQL); err != nil {
		return fmt.Errorf("sim helpers: %w", err)
	}
	var version string
	if err := pool.QueryRow(ctx, "SELECT version()").Scan(&version); err != nil {
		return err
	}
	rep.Version = version
	done()

	done = step("seed")
	w, err := seed(ctx, pool)
	if err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	done()

	asof := time.Now().UTC().Truncate(time.Second)
	done = step(fmt.Sprintf("simulate %d customers over %d years to %s", c.customers, c.years, asof.Format(time.RFC3339)))
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	s := newSim(c, w, conn.Conn())
	end, err := s.run(ctx, asof.AddDate(-c.years, 0, 0), asof)
	conn.Release()
	if err != nil {
		return fmt.Errorf("simulate: %w", err)
	}
	rep.Sim = s.summary(end)
	asof = end
	done()

	done = step("settle what is due at the end")
	if _, err := pool.Exec(ctx, `SELECT sim.effect_due(merchant_id, subscription_id, NULL, $2) FROM billing.subscription_mrr
		WHERE merchant_id = $1 AND next_change_at <= $2`, w.merchant, asof); err != nil {
		return fmt.Errorf("final due pass: %w", err)
	}
	if _, err := pool.Exec(ctx, `SELECT billing.effect_due_mrr_risk($1, 100000, NULL)`, w.merchant); err != nil {
		return fmt.Errorf("final drain: %w", err)
	}
	done()

	done = step("facts-only end state and indexes")
	tag, err := pool.Exec(ctx, protoSQL, w.merchant, asof)
	if err != nil {
		return fmt.Errorf("future-dated churn: %w", err)
	}
	rep.FutureMovements = tag.RowsAffected()
	if _, err := pool.Exec(ctx, protoIndexesSQL); err != nil {
		return fmt.Errorf("proto indexes: %w", err)
	}
	if _, err := pool.Exec(ctx, monthCloseSQL, w.merchant, asof); err != nil {
		return fmt.Errorf("month close: %w", err)
	}
	if _, err := pool.Exec(ctx, "VACUUM (ANALYZE)"); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	if err := rep.tables(ctx, pool, w.merchant); err != nil {
		return fmt.Errorf("table sizes: %w", err)
	}
	done()
	rep.flush()

	done = step("reads")
	if err := measureReads(ctx, pool, c, w, asof, rep); err != nil {
		return fmt.Errorf("reads: %w", err)
	}
	done()
	rep.flush()

	done = step("writes")
	if err := measureWrites(ctx, pool, c, w, asof, rep); err != nil {
		return fmt.Errorf("writes: %w", err)
	}
	done()
	rep.flush()
	rep.print()
	return nil
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, time.Now().UTC().Format("15:04:05")+" "+format+"\n", args...)
}

// seed writes the merchant, its PSP and the catalog: a core product in three
// tiers and an add-on, monthly, in USD and EUR.
func seed(ctx context.Context, pool *pgxpool.Pool) (*world, error) {
	w := &world{merchant: uuid.New(), psp: uuid.New(), core: uuid.New(), addon: uuid.New(), tierOf: map[uuid.UUID]int{}}
	w.amounts = [2][4]int64{{10_000_000, 25_000_000, 60_000_000, 5_000_000}, {9_000_000, 22_000_000, 55_000_000, 4_500_000}}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	steps := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO billing.merchants (id, slug, status) VALUES ($1, 'measure-1418', 'active')`, []any{w.merchant}},
		{`INSERT INTO billing.psps (id, merchant_id, key, rail, environment, account_id) VALUES ($1, $2, 'stripe', 'stripe', 'test', 'acct_measure1418')`, []any{w.psp, w.merchant}},
		{`INSERT INTO billing.products (id, merchant_id, key, display_name, tier_rank, archived) VALUES ($1, $2, 'core', 'Core', 0, false)`, []any{w.core, w.merchant}},
		{`INSERT INTO billing.products (id, merchant_id, key, display_name, tier_rank, archived) VALUES ($1, $2, 'addon', 'Add-on', 0, false)`, []any{w.addon, w.merchant}},
	}
	for _, s := range steps {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			return nil, fmt.Errorf("%s: %w", s.sql, err)
		}
	}
	names := [4]string{"basic", "pro", "team", "addon"}
	for ci, cur := range currencies {
		for tier := 0; tier < 4; tier++ {
			id := uuid.New()
			product := w.core
			if tier == 3 {
				product = w.addon
			}
			if _, err := tx.Exec(ctx, `INSERT INTO billing.prices (id, merchant_id, product_id, key, amount, currency, archived, billing_interval_months)
				VALUES ($1, $2, $3, $4, $5, $6, false, 1)`, id, w.merchant, product, names[tier]+"-"+cur, w.amounts[ci][tier], cur); err != nil {
				return nil, fmt.Errorf("price %s %s: %w", names[tier], cur, err)
			}
			w.prices[ci][tier] = id
			w.tierOf[id] = tier
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return w, nil
}

// measureConn is a connection set up as the metrics reads run: sequential
// scans off (a merchant's rows among every merchant's), no JIT.
func measureConn(ctx context.Context, pool *pgxpool.Pool) (*pgxpool.Conn, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	for _, set := range []string{"SET enable_seqscan = off", "SET jit = off", "SET statement_timeout = '180s'"} {
		if _, err := conn.Exec(ctx, set); err != nil {
			conn.Release()
			return nil, err
		}
	}
	return conn, nil
}

func outPath(c config, name string) string { return filepath.Join(c.out, name) }
