package merchantdocs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
)

// Channel is the Postgres notification channel an edit to a merchant's
// configuration is announced on, one per schema; the payload is the
// merchant id.
func Channel(schema string) string {
	const prefix = "openrails_merchant_config:"
	channel := prefix + schema
	if len(channel) <= 63 {
		return channel
	}
	// pg_notify rejects identifiers exceeding PostgreSQL's 63-byte limit.
	// Keep existing short names for replicas spanning a version update.
	sum := sha256.Sum256([]byte(schema))
	return prefix + hex.EncodeToString(sum[:16])
}

// Listen reloads each merchant another replica announces until ctx ends,
// reconnecting after a failure on a dedicated session.
func (c *Cache) Listen(ctx context.Context, database *db.DB, schema string) {
	if database.Pool() == nil {
		return
	}
	channel := pgx.Identifier{Channel(schema)}.Sanitize()
	for ctx.Err() == nil {
		if err := c.listenOnce(ctx, database, channel); err != nil && ctx.Err() == nil {
			log.WithError(err).Warn("merchant config: edit notifications interrupted; reconnecting")
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
}

func (c *Cache) listenOnce(ctx context.Context, database *db.DB, channel string) error {
	conn, err := database.ConnectSession(ctx)
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
