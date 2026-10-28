package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jonboulle/clockwork"
	"github.com/redis/go-redis/v9"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/admission"
)

const KindAdmissionDenialFlush = "openrails.admission_denial_flush"

type AdmissionDenialFlushArgs struct{}

func (AdmissionDenialFlushArgs) Kind() string { return KindAdmissionDenialFlush }

func (AdmissionDenialFlushArgs) InsertOpts() river.InsertOpts {
	return periodPass(AdmissionDenialFlushInterval)
}

// AdmissionDenialFlushInterval is the flush cadence: an hour's counts appear
// within this, plus denialHourGrace, of its close.
const AdmissionDenialFlushInterval = time.Hour

// denialHourGrace pads an hour's close for writers whose clock runs late.
const denialHourGrace = 5 * time.Minute

// AdmissionDenialFlushWorker moves the Redis hourly denial counters
// (admission.DenialRecorder) of closed hours into admission_denials_hourly: it
// writes each hour's counts as they stand, then deletes the key. No writer
// touches a closed hour, so a repeated or concurrent flush writes the same
// values.
type AdmissionDenialFlushWorker struct {
	WorkerDefaults[AdmissionDenialFlushArgs]
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

	flushed := 0
	var keyErrs error
	scan := func(ctx context.Context, node redis.UniversalClient) error {
		var cursor uint64
		for {
			keys, next, err := node.Scan(ctx, cursor, admission.DenialKeyPrefix+"*", 100).Result()
			if err != nil {
				return err
			}
			for _, key := range keys {
				n, err := w.flushKey(ctx, key, now)
				if err != nil {
					keyErrs = errors.Join(keyErrs, fmt.Errorf("key %s: %w", key, err))
					continue // other keys still flush
				}
				flushed += n
			}
			if cursor = next; cursor == 0 {
				return nil
			}
		}
	}
	// A cluster's SCAN reads one node: scan each primary.
	if cluster, ok := w.Redis.(*redis.ClusterClient); ok {
		var mu sync.Mutex
		if err := cluster.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
			mu.Lock()
			defer mu.Unlock()
			return scan(ctx, node)
		}); err != nil {
			return err
		}
	} else if err := scan(ctx, w.Redis); err != nil {
		return err
	}
	if flushed > 0 {
		logger.WithField("rows", flushed).Info("flushed admission denial counters")
	}
	return keyErrs
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
	if hour.Add(time.Hour + denialHourGrace).After(now) {
		return 0, nil // the hour is still open
	}
	fields, err := w.Redis.HGetAll(ctx, key).Result()
	if err != nil {
		return 0, err
	}

	type row struct {
		customer uuid.UUID
		reason   string
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
		rows = append(rows, row{customer: customerID, reason: reason, count: count})
	}

	mctx := merchant.WithID(ctx, billing.MerchantID(merchantID))
	write := func(rows []row) error {
		return w.DB.MerchantTx(mctx, func(ctx context.Context, tx pgx.Tx) error {
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
		})
	}
	written, err := len(rows), error(nil)
	if written > 0 {
		err = write(rows)
	}
	if err != nil {
		if !foreignKeyViolation(err) {
			return 0, err
		}
		// A customer or merchant erased since never gets a row: write the
		// others one by one and drop its count.
		written = 0
		for _, r := range rows {
			if err := write([]row{r}); err != nil {
				if !foreignKeyViolation(err) {
					return written, err
				}
				log.WithContext(ctx).WithFields(log.Fields{"merchant_id": merchantID, "customer_id": r.customer, "hour_at": hour}).
					Warn("admission denial count dropped: its customer no longer exists")
				continue
			}
			written++
		}
	}
	if err := w.Redis.Del(ctx, key).Err(); err != nil {
		return written, err
	}
	return written, nil
}

func foreignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
