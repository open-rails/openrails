package hostdeps

import (
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/open-rails/openrails/server/internal/hostconfig"
)

// ClickHouse opens the clickhouse section's connection: the native protocol,
// lz4. It dials on first use.
func ClickHouse(c *hostconfig.ClickHouseLogin) (driver.Conn, error) {
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:        []string{c.Addr},
		Auth:        clickhouse.Auth{Username: c.Username, Password: c.Password},
		Compression: &clickhouse.Compression{Method: clickhouse.CompressionLZ4},
	})
	if err != nil {
		return nil, fmt.Errorf("clickhouse: %w", err)
	}
	return conn, nil
}
