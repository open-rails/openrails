package merchantdocs

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
)

// Channel is the Postgres notification channel an edit to a merchant's
// configuration is announced on, one per schema; the payload is the
// merchant id.
func Channel(schema string) string { return "openrails_merchant_config:" + schema }

// Listen reloads each merchant another replica announces until ctx ends,
// reconnecting after a failure. It listens on a connection of its own, dialed
// as pool dials, so it never holds one of pool's.
func (c *Cache) Listen(ctx context.Context, pool *pgxpool.Pool, schema string) {
	if pool == nil {
		return
	}
	channel := pgx.Identifier{Channel(schema)}.Sanitize()
	for ctx.Err() == nil {
		if err := c.listenOnce(ctx, pool, channel); err != nil && ctx.Err() == nil {
			log.WithError(err).Warn("merchant config: edit notifications interrupted; reconnecting")
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
}

func (c *Cache) listenOnce(ctx context.Context, pool *pgxpool.Pool, channel string) error {
	conn, err := dial(ctx, pool.Config())
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
		return err
	}
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		if id, err := uuid.Parse(n.Payload); err == nil {
			c.Invalidate(billing.MerchantID(id))
		}
	}
}

// dial opens a connection as the pool's hooks open one.
func dial(ctx context.Context, cfg *pgxpool.Config) (*pgx.Conn, error) {
	connConfig := cfg.ConnConfig.Copy()
	if cfg.BeforeConnect != nil {
		if err := cfg.BeforeConnect(ctx, connConfig); err != nil {
			return nil, err
		}
	}
	conn, err := pgx.ConnectConfig(ctx, connConfig)
	if err != nil {
		return nil, err
	}
	if cfg.AfterConnect != nil {
		if err := cfg.AfterConnect(ctx, conn); err != nil {
			_ = conn.Close(context.WithoutCancel(ctx))
			return nil, err
		}
	}
	return conn, nil
}
