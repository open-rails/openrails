package health

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresHealthChecker verifies Postgres connectivity.
type PostgresHealthChecker struct {
	pool *pgxpool.Pool
}

// NewPostgresHealthChecker creates a Postgres checker over the pgx pool.
func NewPostgresHealthChecker(pool *pgxpool.Pool) *PostgresHealthChecker {
	return &PostgresHealthChecker{pool: pool}
}

func (p *PostgresHealthChecker) Name() string           { return "postgres" }
func (p *PostgresHealthChecker) Timeout() time.Duration { return 3 * time.Second }

func (p *PostgresHealthChecker) Check(ctx context.Context) error {
	if p == nil || p.pool == nil {
		return fmt.Errorf("postgres pool is nil")
	}
	if _, err := p.pool.Exec(ctx, "SELECT 1"); err != nil {
		return fmt.Errorf("postgres health check failed: %w", err)
	}
	return nil
}
