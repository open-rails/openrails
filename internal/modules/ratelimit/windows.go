package ratelimit

import (
	"context"
	"time"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

// Windows keeps the standalone resource server's spent DPoP proofs in
// PostgreSQL (billing.rate_windows). Rate limits, lockouts and captcha
// challenges are abusestate's, never here.
type Windows struct{ db *db.DB }

// NewWindows is the store over d; nil without a database.
func NewWindows(d *db.DB) *Windows {
	if d == nil || d.Pool() == nil {
		return nil
	}
	return &Windows{db: d}
}

// Hit adds n to key's window, which ends at end when it opens, and returns the
// window's count and when it ends.
func (w *Windows) Hit(ctx context.Context, key string, n int64, end time.Time) (int64, time.Time, error) {
	row, err := w.db.GenDirectory().HitRateWindow(ctx, gen.HitRateWindowParams{Key: key, Hits: n, ExpiresAt: end})
	return row.Hits, row.ExpiresAt, err
}

// Prune deletes up to limit expired keys and returns how many it deleted.
func (w *Windows) Prune(ctx context.Context, limit int32) (int64, error) {
	return w.db.GenDirectory().PruneRateWindows(ctx, limit)
}
