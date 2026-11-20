package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
)

// TryLockPass takes kind's pass lock on a session of its own, so a pass that
// runs for hours pins no slot of the pool. It reports false when another
// session holds it. release closes the session, which frees the lock even
// after a failure.
func (d *DB) TryLockPass(ctx context.Context, kind string) (release func(), locked bool, err error) {
	if d == nil || d.pool == nil {
		return nil, false, fmt.Errorf("pass lock requires a pool")
	}
	acquire, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := d.connectSession(acquire, func(cfg *pgx.ConnConfig) {
		// Server probes free the lock within minutes of a host vanishing.
		if cfg.RuntimeParams == nil {
			cfg.RuntimeParams = map[string]string{}
		}
		cfg.RuntimeParams["tcp_keepalives_idle"] = "60"
		cfg.RuntimeParams["tcp_keepalives_interval"] = "10"
		cfg.RuntimeParams["tcp_keepalives_count"] = "6"
	})
	if err != nil {
		return nil, false, fmt.Errorf("connect pass lock: %w", err)
	}
	release = func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(cleanup)
	}
	locked, err = gen.New(conn).TryLockPass(acquire, "openrails:pass:"+d.rw.schema()+":"+kind)
	if err != nil || !locked {
		release()
		if err != nil {
			return nil, false, fmt.Errorf("lock pass: %w", err)
		}
		return nil, false, nil
	}
	return release, true, nil
}
