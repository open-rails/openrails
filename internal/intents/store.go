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
	// ProviderAccounts stamps provider_account_id at enqueue. nil = no binding
	// (tests, supersede-only callers): intents enqueue with NULL and execute
	// ungated, same as pre-#518 rows.
	ProviderAccounts ProviderAccountResolver
}

func NewStore(d *db.DB) *Store { return &Store{db: d} }

// WithProviderAccounts attaches the provider-account resolver (chainable).
func (s *Store) WithProviderAccounts(src ProviderAccountResolver) *Store {
	s.ProviderAccounts = src
	return s
}

// EnqueueParams describes one logical intent. MerchantID is stamped explicitly;
// IdempotencyKey makes the enqueue effectively-once (see the query's conflict
// semantics: pending refreshed, superseded/expired revived, rest untouched).
type EnqueueParams struct {
	MerchantID     uuid.UUID
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
	// #518: bind the provider account the producer is configured against.
	// Best-effort — an unresolvable account identity stamps NULL (executes
	// ungated) rather than failing the enqueue: ledger durability beats the
	// guard.
	var providerAccountID *uuid.UUID
	if s.ProviderAccounts != nil {
		if identity, ok := s.ProviderAccounts.ResolveProviderAccount(ctx, p.Provider); ok && identity.AccountID != "" {
			row, err := s.upsertProviderAccount(ctx, p.MerchantID, identity)
			if err != nil {
				return gen.OpenrailsProviderIntent{}, fmt.Errorf("intents: upsert provider account: %w", err)
			}
			providerAccountID = &row.ID
		}
	}
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
		ProviderAccountID: providerAccountID,
	})
}

func (s *Store) upsertProviderAccount(ctx context.Context, merchantID uuid.UUID, identity ProviderAccountIdentity) (gen.OpenrailsProviderAccount, error) {
	var evidence []byte
	if len(identity.Evidence) > 0 {
		b, err := json.Marshal(identity.Evidence)
		if err != nil {
			return gen.OpenrailsProviderAccount{}, fmt.Errorf("marshal evidence: %w", err)
		}
		evidence = b
	}
	var displayName *string
	if identity.DisplayName != "" {
		displayName = &identity.DisplayName
	}
	return s.db.Gen(ctx).UpsertProviderAccount(ctx, gen.UpsertProviderAccountParams{
		MerchantID:     merchantID,
		ProviderType:   identity.ProviderType,
		Environment:    stringPtr(normalizedProviderEnvironment(identity.Environment)),
		AccountID:      identity.AccountID,
		DisplayName:    displayName,
		VaultSecretRef: nil,
		Role:           nil,
		Status:         nil,
		Evidence:       evidence,
		LastVerifiedAt: ptrTime(time.Now().UTC()),
	})
}

func ptrTime(t time.Time) *time.Time { return &t }

func stringPtr(v string) *string { return &v }

// VerifyOrBindPrimaryProviderAccount resolves providerKey with the current
// credentials, records that provider-returned account identity, and promotes it
// as the merchant's enabled primary for that provider type. The providerKey is
// only the local config selector; the durable identity is provider_type +
// account_id. If the same selector now resolves to a different account, that is
// treated as an intentional primary rotation: old rows stay bound to their
// provider_account_id while new default work uses the newly promoted account.
func (s *Store) VerifyOrBindPrimaryProviderAccount(ctx context.Context, merchantID uuid.UUID, providerKey string) (gen.OpenrailsProviderAccount, error) {
	if s.ProviderAccounts == nil {
		return gen.OpenrailsProviderAccount{}, fmt.Errorf("intents: provider account resolver unavailable")
	}
	identity, ok := s.ProviderAccounts.ResolveProviderAccount(ctx, providerKey)
	if !ok || identity.AccountID == "" {
		return gen.OpenrailsProviderAccount{}, fmt.Errorf("intents: no provider account resolvable for provider %q", providerKey)
	}
	account, err := s.upsertProviderAccount(ctx, merchantID, identity)
	if err != nil {
		return gen.OpenrailsProviderAccount{}, err
	}
	if err := s.db.Gen(ctx).DemoteOtherPrimaryProviderAccounts(ctx, gen.DemoteOtherPrimaryProviderAccountsParams{
		ID:           account.ID,
		MerchantID:   merchantID,
		ProviderType: account.ProviderType,
		Environment:  stringPtr(account.Environment),
	}); err != nil {
		return gen.OpenrailsProviderAccount{}, err
	}
	return s.db.Gen(ctx).PromoteProviderAccountToPrimary(ctx, gen.PromoteProviderAccountToPrimaryParams{
		ID:           account.ID,
		MerchantID:   merchantID,
		ProviderType: account.ProviderType,
		Environment:  stringPtr(account.Environment),
	})
}

func normalizedProviderEnvironment(raw string) string {
	env, err := normalizeProviderEnvironment(raw)
	if err != nil {
		return "live"
	}
	return env
}

// RebindProviderIntentsToCurrentAccount rebinds every LIVE intent of the
// provider to the CURRENT account (#518 escape hatch — operator confirmed the
// credential change does not point at a different account, or adopts the new
// one deliberately).
func (s *Store) RebindProviderIntentsToCurrentAccount(ctx context.Context, merchantID uuid.UUID, provider string) (gen.OpenrailsProviderAccount, int64, error) {
	if s.ProviderAccounts == nil {
		return gen.OpenrailsProviderAccount{}, 0, fmt.Errorf("intents: provider account resolver unavailable")
	}
	identity, ok := s.ProviderAccounts.ResolveProviderAccount(ctx, provider)
	if !ok || identity.AccountID == "" {
		return gen.OpenrailsProviderAccount{}, 0, fmt.Errorf("intents: no provider account resolvable for provider %q", provider)
	}
	account, err := s.upsertProviderAccount(ctx, merchantID, identity)
	if err != nil {
		return gen.OpenrailsProviderAccount{}, 0, err
	}
	n, err := s.db.Gen(ctx).RebindProviderIntentsToAccount(ctx, gen.RebindProviderIntentsToAccountParams{
		ProviderAccountID: &account.ID,
		Provider:          provider,
	})
	return account, n, err
}

func (s *Store) GetProviderAccount(ctx context.Context, id uuid.UUID) (gen.OpenrailsProviderAccount, error) {
	return s.db.Gen(ctx).GetProviderAccount(ctx, id)
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
