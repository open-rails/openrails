package merchantdocs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/logx"
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

// Listen reloads each merchant another replica announces until ctx ends, on
// a session of its own. Each (re)connect expires every cached merchant, so an
// edit announced while nothing listened is read on the merchant's next use.
func (c *Cache) Listen(ctx context.Context, database *db.DB, schema string) {
	if database.Pool() == nil {
		return
	}
	database.Listen(ctx, db.Listener{
		Channel:   Channel(schema),
		Connected: func(context.Context) { c.Expire() },
		Notified: func(_ context.Context, payload string) {
			if id, err := uuid.Parse(payload); err == nil {
				c.Invalidate(billing.MerchantID(id))
			}
		},
		Interrupted: func(ctx context.Context, err error) {
			c.Log.WarnContext(ctx, "merchant config: edit notifications interrupted; reconnecting", logx.Err(err))
		},
	})
}
