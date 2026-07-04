package riverjobs

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/redis/go-redis/v9"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/pkg/merchant"
)

const KindAdmissionDenialFlush = "openrails.admission_denial_flush"

type AdmissionDenialFlushArgs struct{}

func (AdmissionDenialFlushArgs) Kind() string { return KindAdmissionDenialFlush }

// AdmissionDenialFlushWorker moves the Redis hourly denial counters (#733,
// or:mdeny:* hashes written by admission.DenialRecorder) into
// openrails.admission_denials_hourly. Concurrency-safe: it reads each field's
// count, upserts it additively into PG, then HINCRBYs the same amount back
// out — increments landing mid-flush survive for the next cycle. Keys whose
// hour has been closed for > 5 minutes are deleted after draining (no writer
// touches a past hour).
type AdmissionDenialFlushWorker struct {
	river.WorkerDefaults[AdmissionDenialFlushArgs]
	DB    *db.DB
	Redis redis.UniversalClient
	Clock clockwork.Clock
}

func (AdmissionDenialFlushWorker) Kind() string { return KindAdmissionDenialFlush }

func (w AdmissionDenialFlushWorker) Work(ctx context.Context, job *river.Job[AdmissionDenialFlushArgs]) error {
	if w.Redis == nil || w.DB == nil {
		return nil // not wired (no redis in this deployment) — nothing to flush
	}
	clock := w.Clock
	if clock == nil {
		clock = clockwork.NewRealClock()
	}
	now := clock.Now().UTC()
	logger := log.WithContext(ctx).WithField("worker", KindAdmissionDenialFlush)

	var cursor uint64
	flushed := 0
	for {
		keys, next, err := w.Redis.Scan(ctx, cursor, admission.DenialKeyPrefix+"*", 100).Result()
		if err != nil {
			return err
		}
		for _, key := range keys {
			n, err := w.flushKey(ctx, key, now)
			if err != nil {
				logger.WithError(err).WithField("key", key).Error("denial flush failed for key")
				continue // other keys still flush; this one retries next cycle
			}
			flushed += n
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	if flushed > 0 {
		logger.WithField("rows", flushed).Info("flushed admission denial counters")
	}
	return nil
}

func (w AdmissionDenialFlushWorker) flushKey(ctx context.Context, key string, now time.Time) (int, error) {
	merchantStr, hour, ok := admission.ParseDenialKey(key)
	if !ok {
		return 0, nil
	}
	merchantID, err := uuid.Parse(merchantStr)
	if err != nil {
		return 0, nil // foreign key shape; leave to TTL
	}
	fields, err := w.Redis.HGetAll(ctx, key).Result()
	if err != nil {
		return 0, err
	}

	type row struct {
		customer uuid.UUID
		reason   string
		field    string
		count    int64
	}
	var rows []row
	for field, raw := range fields {
		customerStr, reason, ok := admission.ParseDenialField(field)
		if !ok {
			continue
		}
		customerID, err := uuid.Parse(customerStr)
		if err != nil {
			continue
		}
		var count int64
		for _, c := range raw {
			if c < '0' || c > '9' {
				count = 0
				break
			}
			count = count*10 + int64(c-'0')
		}
		if count <= 0 {
			continue
		}
		rows = append(rows, row{customer: customerID, reason: reason, field: field, count: count})
	}

	if len(rows) > 0 {
		mctx := merchant.WithID(ctx, merchant.ID(merchantID))
		if err := w.DB.MerchantTx(mctx, func(ctx context.Context, tx pgx.Tx) error {
			q := gen.New(tx)
			for _, r := range rows {
				if err := q.UpsertAdmissionDenials(ctx, gen.UpsertAdmissionDenialsParams{
					MerchantID:   merchantID,
					CustomerID:   r.customer,
					DenialReason: r.reason,
					HourAt:       hour,
					Denials:      r.count,
				}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return 0, err
		}
		// PG has the counts; subtract exactly what was flushed so concurrent
		// increments survive.
		pipe := w.Redis.Pipeline()
		for _, r := range rows {
			pipe.HIncrBy(ctx, key, r.field, -r.count)
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return len(rows), err
		}
	}

	// A closed hour (grace-padded) gets no new writes: drop the drained key.
	if hour.Add(time.Hour + 5*time.Minute).Before(now) {
		if err := w.Redis.Del(ctx, key).Err(); err != nil {
			return len(rows), err
		}
	}
	return len(rows), nil
}
