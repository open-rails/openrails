// Command load qualifies OpenRails' usage gate and usage path under load: the
// SDK gate over HTTP against the standalone server and against the embedded
// engine, on PostgreSQL, Redis and ClickHouse. It writes raw results
// (results.json) and a summary (summary.md).
//
//	load run -out DIR [-plan plan.json] [-ramp 2000,4000] [-heavy-rows N] [-worlds http,embedded,posting]
//
// run starts the server and gate processes it needs as children of the same
// binary (load serve, load gates). It needs disposable services only: every
// world drops and recreates its own schema and ClickHouse database.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/open-rails/openrails/catalog"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: load run|serve|gates ...")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(ctx, os.Args[2:])
	case "serve":
		err = serveCmd(ctx, os.Args[2:])
	case "gates":
		err = gatesCmd(ctx, os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		os.Exit(1)
	}
}

func runCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	out := fs.String("out", "", "directory for results.json, summary.md and process logs")
	planFile := fs.String("plan", "", "JSON plan overriding the defaults")
	ramp := fs.String("ramp", "", "hot merchant rates to step through, events/s (comma separated)")
	heavy := fs.Int64("heavy-rows", -1, "usage events of the heavy customer in the posting world")
	worlds := fs.String("worlds", "", "worlds to run: http,embedded,posting")
	step := fs.Int("step-seconds", 0, "seconds per ramp step")
	_ = fs.Parse(args)
	if *out == "" {
		return fmt.Errorf("-out is required")
	}
	plan := defaultPlan()
	if *planFile != "" {
		if err := readJSON(*planFile, &plan); err != nil {
			return fmt.Errorf("plan: %w", err)
		}
	}
	env := func(dst *string, name string) {
		if v := os.Getenv(name); v != "" && *dst == "" {
			*dst = v
		}
	}
	env(&plan.DSN, "OPENRAILS_LOAD_DSN")
	env(&plan.RedisURL, "OPENRAILS_LOAD_REDIS_URL")
	env(&plan.ClickHouse, "OPENRAILS_LOAD_CLICKHOUSE_ADDR")
	env(&plan.ClickHouseContainer, "OPENRAILS_LOAD_CLICKHOUSE_CONTAINER")
	if *ramp != "" {
		r, err := parseInts(*ramp)
		if err != nil {
			return fmt.Errorf("-ramp: %w", err)
		}
		plan.Ramp = r
	}
	if *heavy >= 0 {
		plan.HeavyRows = *heavy
	}
	if *worlds != "" {
		plan.Worlds = strings.Split(*worlds, ",")
	}
	if *step > 0 {
		plan.StepSeconds = *step
	}
	if err := plan.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	return coordinate(ctx, plan, *out)
}

// quietLogger logs warnings and errors as text to w.
func quietLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func openPool(ctx context.Context, dsn string, maxConns int32, tracer pgx.QueryTracer) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	pc.MaxConns = maxConns
	if tracer != nil {
		pc.ConnConfig.Tracer = tracer
	}
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// openClickHouse opens ClickHouse as the standalone server does: the native
// protocol with lz4.
func openClickHouse(addr string) (driver.Conn, error) {
	return clickhouse.Open(&clickhouse.Options{Addr: []string{addr}, Compression: &clickhouse.Compression{Method: clickhouse.CompressionLZ4},
		DialTimeout: 5 * time.Second, MaxOpenConns: 32})
}

func openRedis(url string) (redis.UniversalClient, error) {
	if url == "" {
		return nil, nil
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, err
	}
	return redis.NewClient(opts), nil
}

// fxWire answers every exchange-rate fetch with a rate of 1, so no process
// reaches the internet.
type fxWire struct{}

func (fxWire) RoundTrip(r *http.Request) (*http.Response, error) {
	base := strings.TrimSuffix(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], ".json")
	rates := map[string]int{}
	for _, c := range catalog.Currencies() {
		rates[strings.ToLower(string(c.Code))] = 1
	}
	body, _ := json.Marshal(map[string]any{"date": time.Now().UTC().Format(time.DateOnly), base: rates})
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
}
