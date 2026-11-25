package hostdeps

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/server/internal/hostconfig"
)

// defaultProbe bounds the one ping that decides whether the default
// ClickHouse is there.
const defaultProbe = 3 * time.Second

// ClickHouse opens the clickhouse section's connection: the native protocol,
// lz4. It dials on first use and selects no database: the event store
// creates its own. The default section is pinged once first: when it does
// not answer, ClickHouse logs that usage is off and returns nil.
func ClickHouse(ctx context.Context, logger *slog.Logger, c *hostconfig.ClickHouseConnection) (driver.Conn, error) {
	opts, err := clickhouse.ParseDSN(strings.TrimSpace(c.URL))
	if err != nil {
		return nil, fmt.Errorf("clickhouse.url: %w", redactURL(err, c.URL))
	}
	opts.Auth.Database = ""
	opts.Compression = &clickhouse.Compression{Method: clickhouse.CompressionLZ4}
	if c.Default {
		if err := probe(ctx, *opts); err != nil {
			logger.InfoContext(ctx, "the default local ClickHouse is unreachable, so usage is off: set CLICKHOUSE_URL to enable it", "url", c.URL, logx.Err(err))
			return nil, nil
		}
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: %w", err)
	}
	return conn, nil
}

// probe pings opts once on a connection of its own, the handshake included
// within defaultProbe, so a silent listener on the port is no ClickHouse.
func probe(ctx context.Context, opts clickhouse.Options) error {
	opts.DialTimeout = defaultProbe
	conn, err := clickhouse.Open(&opts)
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, defaultProbe)
	defer cancel()
	return conn.Ping(ctx)
}
