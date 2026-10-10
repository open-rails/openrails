package ratelimit

import (
	"context"
	"time"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

// Windows keeps rate-limit windows, lockouts and captcha challenges in
// PostgreSQL (billing.rate_windows), keyed as in Redis, so replicas without
// Redis count the same ones. Redis, when configured, stays the fast path.
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

// Mark holds key until until.
func (w *Windows) Mark(ctx context.Context, key string, until time.Time) error {
	return w.db.GenDirectory().MarkRateWindow(ctx, gen.MarkRateWindowParams{Key: key, ExpiresAt: until})
}

// Live reports whether key is held, and until when.
func (w *Windows) Live(ctx context.Context, key string) (time.Time, bool, error) {
	until, err := w.db.GenDirectory().LiveRateWindow(ctx, key)
	if db.IsNotFound(err) {
		return time.Time{}, false, nil
	}
	return until, err == nil, err
}

// Clear deletes keys.
func (w *Windows) Clear(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	return w.db.GenDirectory().ClearRateWindows(ctx, keys)
}

// Prune deletes up to limit expired keys and returns how many it deleted.
func (w *Windows) Prune(ctx context.Context, limit int32) (int64, error) {
	return w.db.GenDirectory().PruneRateWindows(ctx, limit)
}
