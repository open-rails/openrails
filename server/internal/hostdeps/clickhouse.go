package hostdeps

import (
	"fmt"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/open-rails/openrails/server/internal/hostconfig"
)

// ClickHouse opens the clickhouse section's connection: the native protocol,
// lz4. It dials on first use and selects no database: the event store
// creates its own.
func ClickHouse(c *hostconfig.ClickHouseConnection) (driver.Conn, error) {
	opts, err := clickhouse.ParseDSN(strings.TrimSpace(c.URL))
	if err != nil {
		return nil, fmt.Errorf("clickhouse.url: %w", redactURL(err, c.URL))
	}
	opts.Auth.Database = ""
	opts.Compression = &clickhouse.Compression{Method: clickhouse.CompressionLZ4}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: %w", err)
	}
	return conn, nil
}
