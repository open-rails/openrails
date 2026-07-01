package intents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

// Store persists intents on the ledger. Producers call Enqueue/Supersede from
// request contexts (merchant-pinned connections, RLS double-checks the stamp);
// the Runner's claims and transitions run on the worker pool.
type Store struct {
	db *db.DB
}

func NewStore(d *db.DB) *Store { return &Store{db: d} }

// EnqueueParams describes one logical intent. MerchantID is stamped explicitly;
// IdempotencyKey makes the enqueue effectively-once (see the query's conflict
// semantics: pending refreshed, superseded/expired revived, rest untouched).
type EnqueueParams struct {
	MerchantID        uuid.UUID
	Provider          string
	IntentType        string
	SubscriptionID    *uuid.UUID
	PaymentID         *uuid.UUID
	PriceID           *uuid.UUID
	ProviderAccountID *uuid.UUID
	Payload           any
	IdempotencyKey    string
	NextAttemptAt     time.Time
	Origin            Origin
	OriginReason      string
	ExpiresAt         *time.Time
}

// Enqueue records the intent (idempotent) and returns the canonical row for
// its idempotency key.
func (s *Store) Enqueue(ctx context.Context, p EnqueueParams) (gen.OpenrailsProviderIntent, error) {
	if p.IntentType == "" || p.IdempotencyKey == "" {
		return gen.OpenrailsProviderIntent{}, fmt.Errorf("intents: enqueue requires intent_type and idempotency_key")
	}
	var payload []byte
	if p.Payload != nil {
		b, err := json.Marshal(p.Payload)
		if err != nil {
			return gen.OpenrailsProviderIntent{}, fmt.Errorf("intents: marshal payload: %w", err)
		}
		payload = b
	}
	var originReason *string
	if p.OriginReason != "" {
		originReason = &p.OriginReason
	}
	// provider_account_id is stamped only when the producer already has observed
	// provenance (for example an existing subscription pinned to an account).
	return s.db.Gen(ctx).EnqueueProviderIntent(ctx, gen.EnqueueProviderIntentParams{
		MerchantID:        p.MerchantID,
		Provider:          p.Provider,
		IntentType:        p.IntentType,
		SubscriptionID:    p.SubscriptionID,
		PaymentID:         p.PaymentID,
		PriceID:           p.PriceID,
		Payload:           payload,
		IdempotencyKey:    p.IdempotencyKey,
		NextAttemptAt:     p.NextAttemptAt.UTC(),
		Origin:            string(p.Origin),
		OriginReason:      originReason,
		ExpiresAt:         p.ExpiresAt,
		ProviderAccountID: p.ProviderAccountID,
	})
}

// SupersedeBySubject marks every live (pending / failed_retryable /
// unknown_needs_verify) intent of the type for the subscription superseded.
// in_flight rows are left to their executor, whose relevance re-check is the
// authoritative guard.
func (s *Store) SupersedeBySubject(ctx context.Context, intentType string, subscriptionID uuid.UUID, reason string) (int64, error) {
	return s.db.Gen(ctx).SupersedeProviderIntentsBySubject(ctx, gen.SupersedeProviderIntentsBySubjectParams{
		IntentType:     intentType,
		SubscriptionID: &subscriptionID,
		Reason:         &reason,
	})
}

// ClaimByID leases ONE specific intent for the synchronous execute path.
// ok=false means the row is not claimable (terminal, expired, or leased by a
// live executor) — the caller inspects the canonical row instead.
func (s *Store) ClaimByID(ctx context.Context, id uuid.UUID, now, leaseUntil time.Time) (gen.OpenrailsProviderIntent, bool, error) {
	row, err := s.db.Gen(ctx).ClaimProviderIntentByID(ctx, gen.ClaimProviderIntentByIDParams{
		ID:         id,
		Now:        now.UTC(),
		LeaseUntil: leaseUntil.UTC(),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return gen.OpenrailsProviderIntent{}, false, nil
		}
		return gen.OpenrailsProviderIntent{}, false, err
	}
	return row, true, nil
}

// Get returns the intent row by id.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (gen.OpenrailsProviderIntent, error) {
	return s.db.Gen(ctx).GetProviderIntent(ctx, id)
}

// ClaimDue leases up to batch due executable intents (SKIP LOCKED).
func (s *Store) ClaimDue(ctx context.Context, now, leaseUntil time.Time, batch int64) ([]gen.OpenrailsProviderIntent, error) {
	return s.db.Gen(ctx).ClaimDueProviderIntents(ctx, gen.ClaimDueProviderIntentsParams{
		Now:        now.UTC(),
		LeaseUntil: leaseUntil.UTC(),
		BatchSize:  batch,
	})
}

// ClaimDueVerify leases up to batch due unknown_needs_verify intents.
func (s *Store) ClaimDueVerify(ctx context.Context, now, leaseUntil time.Time, batch int64) ([]gen.OpenrailsProviderIntent, error) {
	return s.db.Gen(ctx).ClaimDueVerifyProviderIntents(ctx, gen.ClaimDueVerifyProviderIntentsParams{
		Now:        now.UTC(),
		LeaseUntil: leaseUntil.UTC(),
		BatchSize:  batch,
	})
}

// ExpireOverdue expires every live intent whose relevance window elapsed.
func (s *Store) ExpireOverdue(ctx context.Context, now time.Time) (int64, error) {
	return s.db.Gen(ctx).ExpireOverdueProviderIntents(ctx, now.UTC())
}

func (s *Store) MarkSucceeded(ctx context.Context, id uuid.UUID, now time.Time, evidence map[string]any) error {
	var ev []byte
	if len(evidence) > 0 {
		b, err := json.Marshal(evidence)
		if err != nil {
			return fmt.Errorf("intents: marshal evidence: %w", err)
		}
		ev = b
	}
	return one(s.db.Gen(ctx).MarkProviderIntentSucceeded(ctx, gen.MarkProviderIntentSucceededParams{
		ID: id, Now: now.UTC(), ResultEvidence: ev,
	}))
}

// pruneEvidenceKeys are the result-pointer keys a slim succeeded tombstone
// retains by default — the dunning repair path reads them back off
// result_evidence (internal/river/jobs_dunning.go: transaction_id for the
// lifecycle repair, response_code for hard/soft decline classification; the
// admin operations view also surfaces transaction_id). Everything else is
// forensic and was already durably logged to ClickHouse before the success
// transition, so it is safe to drop from Postgres — UNLESS the handler asks to
// keep its evidence (PrunePolicy), which the catalog archive/sunset handlers do
// because pkg/service/catalog_extras.go renders their verification booleans.
var pruneEvidenceKeys = []string{"transaction_id", "response_code"}

// PruneSucceeded slims a just-succeeded intent down to a dedupe tombstone:
// the heavy payload is dropped and result_evidence is reduced to the pointer
// keys downstream readers still need. The ROW ITSELF IS RETAINED — it is the
// effectively-once tombstone (UNIQUE on merchant_id+idempotency_key); deleting
// it would let a re-enqueue re-run the provider mutation (a double charge).
//
// keepPayload/keepEvidence are the handler's PrunePolicy: a handler whose
// payload is read AFTER success (the refund producer's conflict detection reads
// reservation_id off the durable succeeded row — admin_payments.go) sets
// keepPayload; a handler whose result_evidence is read after success (catalog
// archive/sunset detail — catalog_extras.go) sets keepEvidence.
//
// The WHERE status='succeeded' guard makes this a no-op on anything not (still)
// succeeded. Best-effort by design: a prune failure leaves the full row in
// place, which is correct — just larger — and never weakens the dedupe.
//
// RAW pgx (no sqlc): runs on Qx(ctx) so the schema rewriter (#471) and RLS
// apply exactly as they do for the generated queries.
func (s *Store) PruneSucceeded(ctx context.Context, id uuid.UUID, evidence map[string]any, keepPayload, keepEvidence bool) error {
	if keepPayload && keepEvidence {
		return nil // nothing to slim
	}
	qx := s.db.Qx(ctx)
	if keepEvidence {
		// Drop the payload only; leave result_evidence intact for the handler's
		// post-success readers.
		_, err := qx.Exec(ctx,
			`UPDATE openrails.provider_intents
			    SET payload = NULL, updated_at = now()
			  WHERE id = $1 AND status = 'succeeded'`, id)
		return err
	}
	var ev []byte
	if slim := slimEvidence(evidence); len(slim) > 0 {
		b, err := json.Marshal(slim)
		if err != nil {
			return fmt.Errorf("intents: marshal slim evidence: %w", err)
		}
		ev = b
	}
	if keepPayload {
		_, err := qx.Exec(ctx,
			`UPDATE openrails.provider_intents
			    SET result_evidence = $2, updated_at = now()
			  WHERE id = $1 AND status = 'succeeded'`, id, ev)
		return err
	}
	_, err := qx.Exec(ctx,
		`UPDATE openrails.provider_intents
		    SET payload = NULL, result_evidence = $2, updated_at = now()
		  WHERE id = $1 AND status = 'succeeded'`, id, ev)
	return err
}

// slimEvidence keeps only pruneEvidenceKeys (when present) off a succeeded
// intent's evidence; returns nil when none are present so the column is set
// NULL rather than to an empty object.
func slimEvidence(evidence map[string]any) map[string]any {
	if len(evidence) == 0 {
		return nil
	}
	slim := map[string]any{}
	for _, k := range pruneEvidenceKeys {
		if v, ok := evidence[k]; ok {
			slim[k] = v
		}
	}
	if len(slim) == 0 {
		return nil
	}
	return slim
}

func (s *Store) MarkFailedRetryable(ctx context.Context, id uuid.UUID, nextAttemptAt time.Time, reason string) error {
	return one(s.db.Gen(ctx).MarkProviderIntentFailedRetryable(ctx, gen.MarkProviderIntentFailedRetryableParams{
		ID: id, NextAttemptAt: nextAttemptAt.UTC(), Reason: &reason,
	}))
}

func (s *Store) MarkUnknown(ctx context.Context, id uuid.UUID, nextAttemptAt time.Time, reason string) error {
	return one(s.db.Gen(ctx).MarkProviderIntentUnknown(ctx, gen.MarkProviderIntentUnknownParams{
		ID: id, NextAttemptAt: nextAttemptAt.UTC(), Reason: &reason,
	}))
}

// MarkFailedTerminal records an unretryable failure. evidence (optional)
// carries structured forensics — e.g. the gateway decline response code that
// the dunning worker's decline classification reads back off the ledger.
func (s *Store) MarkFailedTerminal(ctx context.Context, id uuid.UUID, reason string, evidence map[string]any) error {
	var ev []byte
	if len(evidence) > 0 {
		b, err := json.Marshal(evidence)
		if err != nil {
			return fmt.Errorf("intents: marshal evidence: %w", err)
		}
		ev = b
	}
	return one(s.db.Gen(ctx).MarkProviderIntentFailedTerminal(ctx, gen.MarkProviderIntentFailedTerminalParams{
		ID: id, Reason: &reason, ResultEvidence: ev,
	}))
}

func (s *Store) Park(ctx context.Context, id uuid.UUID, nextAttemptAt time.Time, reason string) error {
	return one(s.db.Gen(ctx).ParkProviderIntent(ctx, gen.ParkProviderIntentParams{
		ID: id, NextAttemptAt: nextAttemptAt.UTC(), Reason: &reason,
	}))
}

func (s *Store) MarkSuperseded(ctx context.Context, id uuid.UUID, reason string) error {
	return one(s.db.Gen(ctx).MarkProviderIntentSuperseded(ctx, gen.MarkProviderIntentSupersededParams{
		ID: id, Reason: &reason,
	}))
}

// one normalizes :execrows transitions: 0 rows means the row raced into a
// state the transition no longer applies to — not an error, the next sweep
// re-evaluates.
func one(rows int64, err error) error {
	if err != nil {
		return err
	}
	return nil
}
