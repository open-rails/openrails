package webhooks

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
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
	// WebhookClaimLease is how long a silent delivery keeps its claim; the
	// owner renews it while the handler runs. WebhookClaimTTL bounds the
	// claim row; webhook_events keeps the applied fact for WebhookEventRetention.
	WebhookClaimLease     = 2 * time.Minute
	WebhookClaimTTL       = 24 * time.Hour
	WebhookEventRetention = 90 * 24 * time.Hour
)

// DeduplicationService dedups webhook deliveries across replicas (#1099).
// A delivery is claimed in openrails.idempotency_keys, so exactly one replica
// processes an event at a time. The applied fact is openrails.webhook_events
// (#678), written with the handler's effects where it can be.
type DeduplicationService struct {
	claims *idempotency.Store
	db     *db.DB
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

// WebhookRefusal is a deliberate, permanent refusal of a well-formed event.
// Ingress acknowledges it (redelivery cannot change the answer) and reports
// Code, so the refusal is never mistaken for acceptance.
type WebhookRefusal struct {
	Code string
	Err  error
}

func (e *WebhookRefusal) Error() string { return e.Code + ": " + e.Err.Error() }

func (e *WebhookRefusal) Unwrap() error { return e.Err }

// WebhookRefusalCode returns the refusal code carried by err, or "".
func WebhookRefusalCode(err error) string {
	var refusal *WebhookRefusal
	if errors.As(err, &refusal) {
		return refusal.Code
	}
	return ""
}

// NewDeduplicationService requires the claim store and the database holding
// webhook_events.
func NewDeduplicationService(claims *idempotency.Store, database *db.DB) (*DeduplicationService, error) {
	if claims == nil || database == nil {
		return nil, fmt.Errorf("webhook dedup requires the idempotency store and the database")
	}
	return &DeduplicationService{claims: claims, db: database}, nil
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

// newDedupMark resolves the truth-row identity, or nil when no merchant is on
// ctx: both the claim and the truth row are merchant-scoped.
func (s *DeduplicationService) newDedupMark(ctx context.Context, op, eventID string) *dedupMark {
	mid, err := merchant.Require(ctx)
	if err != nil {
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

// ProcessWebhook handles webhook deduplication and processing coordination.
// source is WHO sent the event — a rail, or a custodian that emits its own
// instrument events (or#879); the dedup namespace is per-source either way.
func (s *DeduplicationService) ProcessWebhook(ctx context.Context, eventID, eventType string, source models.EventSource, processingFunc func(ctx context.Context) error) error {
	trimmedEventID := strings.TrimSpace(eventID)
	op := fmt.Sprintf("webhook.%s.%s", source, eventType)
	// Provider event IDs are account-local, including retries after archive.
	// Carry the authenticated identity through the claim and truth keys.
	if pspID := db.PSPIDFromContext(ctx); pspID != uuid.Nil {
		op += ".psp." + pspID.String()
	} else if custodianID := db.CustodianIDFromContext(ctx); custodianID != uuid.Nil {
		op += ".custodian." + custodianID.String()
	}

	if trimmedEventID == "" {
		return s.run(ctx, nil, nil, processingFunc)
	}
	fields := log.Fields{"eventID": trimmedEventID, "eventType": eventType, "source": source}
	mark := s.newDedupMark(ctx, op, trimmedEventID)
	if mark == nil {
		log.WithContext(ctx).WithFields(fields).Warn("no merchant on context: processing webhook without dedupe protection")
		return s.run(ctx, nil, nil, processingFunc)
	}
	claim, rec, err := s.claims.Begin(ctx, op, trimmedEventID)
	if err != nil {
		return fmt.Errorf("failed to claim webhook: %w", err)
	}
	if claim == nil {
		if rec.Status == idempotency.StatusSucceeded {
			log.WithContext(ctx).WithFields(fields).Info("Webhook already processed successfully, skipping")
			return nil
		}
		log.WithContext(ctx).WithFields(fields).Info("Webhook already in progress, skipping concurrent duplicate")
		return fmt.Errorf("webhook already in progress")
	}
	if claim.Reclaimed {
		log.WithContext(ctx).WithFields(fields).Warn("Reclaimed webhook after a failed or abandoned delivery")
	}
	// The claim says who processes; webhook_events says whether it was applied
	// (a crash after the effects committed, or an earlier claim collected).
	done, err := s.markCompleted(ctx, mark)
	if err != nil {
		s.release(ctx, claim, fields, err)
		return fmt.Errorf("failed to check webhook dedup truth: %w", err)
	}
	if done {
		s.complete(ctx, claim, fields)
		log.WithContext(ctx).WithFields(fields).Info("Webhook already processed (postgres truth), skipping")
		return nil
	}
	return s.run(ctx, claim, mark, processingFunc)
}

// run executes the handler under an owned claim (nil = no dedupe) and records
// the outcome: applied or non-retryable -> truth mark + completed claim;
// retryable -> released claim, so the provider's redelivery runs it again.
func (s *DeduplicationService) run(ctx context.Context, claim *idempotency.Claim, mark *dedupMark, processingFunc func(ctx context.Context) error) error {
	fields := log.Fields{}
	if mark != nil {
		fields = log.Fields{"op": mark.op, "eventID": mark.eventID}
		ctx = context.WithValue(ctx, dedupMarkCtxKey{}, mark)
	}
	stop := func() {}
	if claim != nil {
		stop = claim.Hold(ctx)
	}
	processingErr := processingFunc(ctx)
	stop()
	if processingErr != nil && !IsWebhookErrorNonRetryable(processingErr) {
		log.WithContext(ctx).WithFields(fields).WithError(processingErr).Error("Webhook processing failed")
		if claim != nil {
			s.release(ctx, claim, fields, processingErr)
		}
		return fmt.Errorf("webhook processing failed: %w", processingErr)
	}
	// TRUTH: verify-or-write the mark. A no-op when the handler committed it
	// with its effects (MarkWebhookProcessedInTx). Failure is retryable: #675
	// replay-safety makes the redelivered effects converge.
	if mark != nil {
		if err := s.writeMark(ctx, mark); err != nil {
			if claim != nil {
				s.release(ctx, claim, fields, err)
			}
			return fmt.Errorf("failed to record webhook dedup mark: %w", err)
		}
	}
	if claim != nil {
		s.complete(ctx, claim, fields)
	}
	if processingErr != nil {
		log.WithContext(ctx).WithFields(fields).WithError(processingErr).Warn("Webhook failed with non-retryable error; marked complete to avoid futile retries")
		return nil
	}
	log.WithContext(ctx).WithFields(fields).Info("Webhook processed successfully")
	return nil
}

func (s *DeduplicationService) complete(ctx context.Context, claim *idempotency.Claim, fields log.Fields) {
	if err := claim.Complete(ctx, nil); err != nil {
		log.WithContext(ctx).WithFields(fields).WithError(err).Warn("webhook claim not completed (webhook_events holds the applied fact)")
	}
}

func (s *DeduplicationService) release(ctx context.Context, claim *idempotency.Claim, fields log.Fields, cause error) {
	if err := claim.Fail(ctx, cause); err != nil {
		log.WithContext(ctx).WithFields(fields).WithError(err).Warn("webhook claim not released; it lapses with its lease")
	}
}
