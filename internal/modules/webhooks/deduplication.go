package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/pkg/merchant"
	log "github.com/sirupsen/logrus"
)

const (
	webhookPendingLease   = 2 * time.Minute
	WebhookIdempotencyTTL = 90 * 24 * time.Hour
)

// DeduplicationService dedups webhook deliveries. TRUTH lives in Postgres
// (openrails.webhook_events, #678): a row means the event's effects are
// durably applied. Redis (via IdempotencyService) is a fast-path cache of
// completed keys plus the pending-lease coordination layer — flushing it
// costs extra Postgres checks and lost lease coordination, never a replay.
type DeduplicationService struct {
	idem *idempotency.IdempotencyService
	// db hosts the webhook_events truth table; nil = Redis/memory-only
	// dedup (legacy behavior, unit tests).
	db *db.DB
	// pendingLease overrides webhookPendingLease (tests only; zero = default).
	pendingLease time.Duration
}

// NonRetryableWebhookError marks a processing failure as terminal.
// ProcessWebhook will mark idempotency as success and stop retries for this error.
type NonRetryableWebhookError struct {
	Err error
}

func (e *NonRetryableWebhookError) Error() string {
	if e == nil || e.Err == nil {
		return "non-retryable webhook error"
	}
	return e.Err.Error()
}

func (e *NonRetryableWebhookError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// MarkWebhookErrorNonRetryable wraps err so ProcessWebhook treats it as terminal.
func MarkWebhookErrorNonRetryable(err error) error {
	if err == nil {
		return nil
	}
	var existing *NonRetryableWebhookError
	if errors.As(err, &existing) {
		return err
	}
	return &NonRetryableWebhookError{Err: err}
}

func IsWebhookErrorNonRetryable(err error) bool {
	var nonRetryable *NonRetryableWebhookError
	return errors.As(err, &nonRetryable)
}

// NewDeduplicationService creates a webhook deduplication service. database
// carries the Postgres truth table; nil degrades to Redis/memory-only dedup.
func NewDeduplicationService(idem *idempotency.IdempotencyService, database *db.DB) *DeduplicationService {
	return &DeduplicationService{idem: idem, db: database}
}

// dedupMarkCtxKey carries the in-flight event's truth-row identity so handlers
// can commit the mark atomically with their effects (MarkWebhookProcessedInTx).
type dedupMarkCtxKey struct{}

// dedupMark identifies the Postgres truth row for an in-flight webhook.
type dedupMark struct {
	merchantID merchant.ID
	op         string
	eventID    string
}

func (m *dedupMark) params() gen.MarkWebhookEventCompletedParams {
	return gen.MarkWebhookEventCompletedParams{
		MerchantID: m.merchantID.UUID(),
		Op:         m.op,
		EventID:    m.eventID,
	}
}

// newDedupMark resolves the truth-row identity, or nil when Postgres truth is
// unavailable (no DB wired, or no merchant on ctx — then Redis is the only net).
func (s *DeduplicationService) newDedupMark(ctx context.Context, op, eventID string) *dedupMark {
	if s == nil || s.db == nil {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		log.WithContext(ctx).WithFields(log.Fields{"op": op, "eventID": eventID}).
			Warn("no merchant on context: webhook dedup has no Postgres truth row for this event (Redis-only)")
		return nil
	}
	return &dedupMark{merchantID: mid, op: op, eventID: eventID}
}

// markCompleted reports whether the truth row exists (RLS-scoped via MerchantTx).
func (s *DeduplicationService) markCompleted(ctx context.Context, m *dedupMark) (bool, error) {
	var done bool
	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var qerr error
		done, qerr = gen.New(tx).WebhookEventCompleted(ctx, gen.WebhookEventCompletedParams{
			MerchantID: m.merchantID.UUID(),
			Op:         m.op,
			EventID:    m.eventID,
		})
		return qerr
	})
	return done, err
}

// writeMark records the truth row. Idempotent (ON CONFLICT DO NOTHING): the
// write-after path and the verify-after-in-tx path are the same statement.
func (s *DeduplicationService) writeMark(ctx context.Context, m *dedupMark) error {
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := gen.New(tx).MarkWebhookEventCompleted(ctx, m.params())
		return err
	})
}

// cacheCompleted backfills the Redis completed-cache (best-effort: Postgres is
// the truth, so a cache write failure only costs the next check a DB hit).
func (s *DeduplicationService) cacheCompleted(ctx context.Context, op, key string, payload []byte) {
	if s.idem == nil {
		return
	}
	if err := s.idem.Complete(ctx, op, key, payload); err != nil {
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{"op": op, "eventID": key}).
			Warn("failed to backfill webhook dedup cache (postgres truth is recorded)")
	}
}

// MarkWebhookProcessedInTx writes the completed dedup mark on the handler's
// OWN effect tx, so mark and effects commit (or roll back) atomically (#678).
// No-op when no Postgres dedup mark is in flight. ProcessWebhook re-asserts
// the mark after the handler returns (ON CONFLICT DO NOTHING), so calling this
// is an atomicity upgrade, never a requirement.
func MarkWebhookProcessedInTx(ctx context.Context, tx pgx.Tx) error {
	m, _ := ctx.Value(dedupMarkCtxKey{}).(*dedupMark)
	if m == nil {
		return nil
	}
	if _, err := gen.New(tx).MarkWebhookEventCompleted(ctx, m.params()); err != nil {
		return fmt.Errorf("mark webhook processed in tx: %w", err)
	}
	return nil
}

func (s *DeduplicationService) lease() time.Duration {
	if s != nil && s.pendingLease > 0 {
		return s.pendingLease
	}
	return webhookPendingLease
}

// startPendingHeartbeat renews the pending lease while the handler runs, so
// stale-pending takeover only fires for dead holders, not slow ones (#678).
// Returned func stops the heartbeat.
func (s *DeduplicationService) startPendingHeartbeat(ctx context.Context, op, key string) func() {
	hbCtx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(s.lease() / 4)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				if _, err := s.idem.RenewPending(hbCtx, op, key); err != nil && hbCtx.Err() == nil {
					log.WithContext(hbCtx).WithError(err).WithFields(log.Fields{
						"op":      op,
						"eventID": key,
					}).Warn("webhook pending-lease renewal failed")
				}
			}
		}
	}()
	return cancel
}

// ProcessWebhook handles webhook deduplication and processing coordination.
func (s *DeduplicationService) ProcessWebhook(ctx context.Context, eventID, eventType string, rail models.Rail, payload interface{}, processingFunc func(ctx context.Context) error) error {
	var payloadBytes []byte
	if payload != nil {
		if data, err := json.Marshal(payload); err == nil {
			payloadBytes = data
		} else {
			log.WithContext(ctx).WithError(err).Warn("failed to marshal webhook payload for idempotency storage")
		}
	}

	trimmedEventID := strings.TrimSpace(eventID)
	op := fmt.Sprintf("webhook.%s.%s", rail, eventType)

	var shouldRecordOutcome bool
	var mark *dedupMark
	if trimmedEventID != "" {
		mark = s.newDedupMark(ctx, op, trimmedEventID)
		if s == nil || s.idem == nil {
			if mark == nil {
				log.WithContext(ctx).WithFields(log.Fields{
					"eventID":   trimmedEventID,
					"eventType": eventType,
					"rail":      rail,
				}).Warn("IdempotencyService is not configured; processing webhook without dedupe protection")
			}
		} else {
			rec, alreadyExists, err := s.idem.Begin(ctx, op, trimmedEventID)
			if err != nil {
				return fmt.Errorf("failed to begin idempotency: %w", err)
			}
			if alreadyExists && rec.Status == idempotency.IdempotencyStatusSuccess {
				log.WithContext(ctx).WithFields(log.Fields{
					"eventID":   trimmedEventID,
					"eventType": eventType,
					"rail":      rail,
				}).Info("Webhook already processed successfully, skipping")
				return nil
			}
			if alreadyExists && rec.Status == idempotency.IdempotencyStatusPending {
				if time.Since(rec.CreatedAt) > s.lease() {
					taken, err := s.idem.TryTakeoverPending(ctx, op, trimmedEventID, s.lease())
					if err != nil {
						return fmt.Errorf("failed to take over stale webhook idempotency: %w", err)
					}
					if !taken {
						log.WithContext(ctx).WithFields(log.Fields{
							"eventID":   trimmedEventID,
							"eventType": eventType,
							"rail":      rail,
						}).Info("Stale pending webhook was claimed by another worker")
						return fmt.Errorf("webhook already in progress")
					}
					log.WithContext(ctx).WithFields(log.Fields{
						"eventID":   trimmedEventID,
						"eventType": eventType,
						"rail":      rail,
					}).Warn("Taking over stale pending webhook idempotency record")
					shouldRecordOutcome = true
				} else {
					log.WithContext(ctx).WithFields(log.Fields{
						"eventID":   trimmedEventID,
						"eventType": eventType,
						"rail":      rail,
					}).Info("Webhook already in progress, skipping concurrent duplicate")
					return fmt.Errorf("webhook already in progress")
				}
			} else {
				shouldRecordOutcome = rec == nil || rec.Status != idempotency.IdempotencyStatusSuccess
			}
		}

		// TRUTH check (#678): the cache had no completed record — consult
		// Postgres before running effects. A hit (cache flushed/evicted, or a
		// crash after the truth write) backfills the cache and skips.
		if mark != nil {
			done, err := s.markCompleted(ctx, mark)
			if err != nil {
				return fmt.Errorf("failed to check webhook dedup truth: %w", err)
			}
			if done {
				s.cacheCompleted(ctx, op, trimmedEventID, payloadBytes)
				log.WithContext(ctx).WithFields(log.Fields{
					"eventID":   trimmedEventID,
					"eventType": eventType,
					"rail":      rail,
				}).Info("Webhook already processed (postgres truth), skipping")
				return nil
			}
		}
	}

	// We own the pending claim: heartbeat it so slow handlers keep exclusivity (#678).
	if shouldRecordOutcome && trimmedEventID != "" {
		stopHeartbeat := s.startPendingHeartbeat(ctx, op, trimmedEventID)
		defer stopHeartbeat()
	}

	// Expose the truth-row identity so the handler's final effect tx can
	// commit the mark atomically with the effects (MarkWebhookProcessedInTx).
	if mark != nil {
		ctx = context.WithValue(ctx, dedupMarkCtxKey{}, mark)
	}

	processingErr := processingFunc(ctx)
	if processingErr != nil {
		nonRetryable := IsWebhookErrorNonRetryable(processingErr)

		log.WithContext(ctx).WithFields(log.Fields{
			"eventID":   trimmedEventID,
			"eventType": eventType,
			"rail":      rail,
			"error":     processingErr.Error(),
		}).Error("Webhook processing failed")

		// Non-retryable = terminally handled: record the truth mark so
		// redeliveries stay no-ops even across a cache flush.
		if nonRetryable && mark != nil {
			if err := s.writeMark(ctx, mark); err != nil {
				return fmt.Errorf("failed to record non-retryable webhook dedup mark: %w", err)
			}
		}
		if shouldRecordOutcome && trimmedEventID != "" {
			if nonRetryable {
				if err := s.idem.Complete(ctx, op, trimmedEventID, payloadBytes); err != nil {
					if mark == nil {
						return fmt.Errorf("failed to mark non-retryable webhook idempotency as complete: %w", err)
					}
					log.WithContext(ctx).WithError(err).Warn("failed to write webhook dedup cache (postgres truth is recorded)")
				}
			} else {
				if err := s.idem.Fail(ctx, op, trimmedEventID, processingErr); err != nil {
					log.WithContext(ctx).WithError(err).Warn("failed to mark webhook idempotency as failed")
				}
			}
		}

		if nonRetryable {
			log.WithContext(ctx).WithFields(log.Fields{
				"eventID":   trimmedEventID,
				"eventType": eventType,
				"rail":      rail,
			}).Warn("Webhook failed with non-retryable error; marked complete to avoid futile retries")
			return nil
		}

		return fmt.Errorf("webhook processing failed: %w", processingErr)
	}

	// TRUTH: verify-or-write the Postgres mark. A no-op when the handler
	// already committed it in its own effect tx (MarkWebhookProcessedInTx);
	// otherwise this is the write-after mark. Failure = retryable: #675
	// replay-safety makes the redelivered effects converge.
	if mark != nil {
		if err := s.writeMark(ctx, mark); err != nil {
			return fmt.Errorf("failed to record webhook dedup mark: %w", err)
		}
	}
	if shouldRecordOutcome && trimmedEventID != "" {
		if err := s.idem.Complete(ctx, op, trimmedEventID, payloadBytes); err != nil {
			if mark == nil {
				return fmt.Errorf("failed to mark webhook idempotency as complete: %w", err)
			}
			log.WithContext(ctx).WithError(err).Warn("failed to write webhook dedup cache (postgres truth is recorded)")
		}
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"eventID":   trimmedEventID,
		"eventType": eventType,
		"rail":      rail,
	}).Info("Webhook processed successfully")

	return nil
}
