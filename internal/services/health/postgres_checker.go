package health

import (
	"context"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// PostgresHealthChecker verifies Postgres connectivity.
type PostgresHealthChecker struct {
	db *bun.DB
}

// NewPostgresHealthChecker creates a Postgres checker.
func NewPostgresHealthChecker(db *bun.DB) *PostgresHealthChecker {
	return &PostgresHealthChecker{db: db}
}

func (p *PostgresHealthChecker) Name() string           { return "postgres" }
func (p *PostgresHealthChecker) Timeout() time.Duration { return 3 * time.Second }

func (p *PostgresHealthChecker) Check(ctx context.Context) error {
	if p == nil || p.db == nil {
		return fmt.Errorf("postgres db is nil")
	}
	if _, err := p.db.ExecContext(ctx, "SELECT 1"); err != nil {
		return fmt.Errorf("postgres health check failed: %w", err)
	}
	return nil
}
