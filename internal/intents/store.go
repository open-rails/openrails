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
// request contexts (tenant-pinned connections, RLS double-checks the stamp);
// the Runner's claims and transitions run on the worker pool.
type Store struct {
	db *db.DB
	// Fingerprints stamps account_fingerprint at enqueue (#365). nil = no
	// stamping (tests, supersede-only callers): intents enqueue with NULL and
	// execute ungated, same as pre-#365 rows.
	Fingerprints FingerprintSource
}

func NewStore(d *db.DB) *Store { return &Store{db: d} }

// WithFingerprints attaches the #365 account-fingerprint source (chainable).
func (s *Store) WithFingerprints(src FingerprintSource) *Store {
	s.Fingerprints = src
	return s
}

// EnqueueParams describes one logical intent. TenantID is stamped explicitly;
// IdempotencyKey makes the enqueue effectively-once (see the query's conflict
// semantics: pending refreshed, superseded/expired revived, rest untouched).
type EnqueueParams struct {
	TenantID       uuid.UUID
	Provider       string
	IntentType     string
	SubscriptionID *uuid.UUID
	PaymentID      *uuid.UUID
	PriceID        *uuid.UUID
	Payload        any
	IdempotencyKey string
	NextAttemptAt  time.Time
	Origin         Origin
	OriginReason   string
	ExpiresAt      *time.Time
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
	// #365: stamp the provider account the producer is configured against.
	// Best-effort — an unresolvable fingerprint stamps NULL (executes ungated)
	// rather than failing the enqueue: ledger durability beats the guard.
	var fingerprint *string
	if s.Fingerprints != nil {
		if fp, ok := s.Fingerprints.AccountFingerprint(ctx, p.Provider); ok && fp != "" {
			fingerprint = &fp
		}
	}
	return s.db.Gen(ctx).EnqueueProviderIntent(ctx, gen.EnqueueProviderIntentParams{
		TenantID:           p.TenantID,
		Provider:           p.Provider,
		IntentType:         p.IntentType,
		SubscriptionID:     p.SubscriptionID,
		PaymentID:          p.PaymentID,
		PriceID:            p.PriceID,
		Payload:            payload,
		IdempotencyKey:     p.IdempotencyKey,
		NextAttemptAt:      p.NextAttemptAt.UTC(),
		Origin:             string(p.Origin),
		OriginReason:       originReason,
		ExpiresAt:          p.ExpiresAt,
		AccountFingerprint: fingerprint,
	})
}

// Refingerprint re-stamps every LIVE intent of the provider to the given
// fingerprint (#365 escape hatch — operator confirmed the credential change
// does not point at a different account, or adopts the new one deliberately).
func (s *Store) Refingerprint(ctx context.Context, provider, fingerprint string) (int64, error) {
	return s.db.Gen(ctx).RefingerprintProviderIntents(ctx, gen.RefingerprintProviderIntentsParams{
		Provider:           provider,
		AccountFingerprint: &fingerprint,
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
