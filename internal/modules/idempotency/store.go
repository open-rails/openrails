// Package idempotency is durable request idempotency in PostgreSQL (#1099):
// who runs a request, and what a replay of it answers, across every replica.
//
// Two layers, never one:
//
//   - This store claims (merchant, operation, key). Exactly one caller holds a
//     processing claim; the rest see it in progress or read its stored result.
//   - A provider call is made once by rail_intents (unique per merchant on its
//     idempotency key), whose keys derive from the request key. A request that
//     runs twice therefore reaches the same intent, which never executes twice.
//
// A processing claim is leased. Its owner renews the lease (Claim.Hold) while
// it works; when the owner dies the lease lapses and exactly one later Begin
// reclaims it and runs the request again. That rerun is safe only because of
// the second layer: nothing guarded here may call a provider except through
// rail_intents.
package idempotency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

type Status string

const (
	StatusProcessing Status = "processing"
	StatusSucceeded  Status = "succeeded"
	StatusFailed     Status = "failed"
)

// ErrClaimLost means the claim lapsed and another caller reclaimed it; the
// superseded owner's write is refused.
var ErrClaimLost = errors.New("idempotency claim was superseded")

type Record struct {
	Status         Status
	Result         json.RawMessage
	Error          string
	Claims         int64
	LeaseExpiresAt time.Time
	ExpiresAt      time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Store claims keys for one class of work. TTL bounds how long a result is
// replayed; Lease is how long a silent owner keeps its claim.
type Store struct {
	db    *db.DB
	clock clockwork.Clock
	ttl   time.Duration
	lease time.Duration
}

func NewStore(database *db.DB, clock clockwork.Clock, ttl, lease time.Duration) (*Store, error) {
	if database == nil || clock == nil || ttl <= 0 || lease <= 0 || lease > ttl {
		return nil, fmt.Errorf("idempotency store needs a database, a clock and 0 < lease <= ttl")
	}
	return &Store{db: database, clock: clock, ttl: ttl, lease: lease}, nil
}

func (s *Store) Lease() time.Duration { return s.lease }

// queries use the request's pinned connection when it has one: waiting on the
// pool while holding that pin would deadlock a saturated pool. Callers claim
// and settle outside their own transactions, so each statement commits alone.
func (s *Store) queries(ctx context.Context) *gen.Queries { return s.db.Gen(ctx) }

// Begin claims key for operation under the context's merchant. A non-nil
// Claim is owned by the caller, who must Complete or Fail it. Otherwise the
// Record is the key's current state: succeeded (replay its Result) or
// processing under a live lease.
func (s *Store) Begin(ctx context.Context, operation, key string) (*Claim, *Record, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, nil, err
	}
	if operation == "" || key == "" {
		return nil, nil, fmt.Errorf("idempotency: operation and key are required")
	}
	q := s.queries(ctx)
	for range 3 {
		now := s.clock.Now().UTC()
		row, err := q.ClaimIdempotencyKey(ctx, gen.ClaimIdempotencyKeyParams{
			MerchantID: mid.UUID(), Operation: operation, IdempotencyKey: key,
			LeaseExpiresAt: now.Add(s.lease), ExpiresAt: now.Add(s.ttl), Now: now,
		})
		if err == nil {
			return s.claim(mid.UUID(), row), nil, nil
		}
		if !db.IsNotFound(err) {
			return nil, nil, fmt.Errorf("claim idempotency key: %w", err)
		}
		row, err = q.ReclaimIdempotencyKey(ctx, gen.ReclaimIdempotencyKeyParams{
			MerchantID: mid.UUID(), Operation: operation, IdempotencyKey: key,
			LeaseExpiresAt: now.Add(s.lease), ExpiresAt: now.Add(s.ttl), Now: now,
		})
		if err == nil {
			return s.claim(mid.UUID(), row), nil, nil
		}
		if !db.IsNotFound(err) {
			return nil, nil, fmt.Errorf("reclaim idempotency key: %w", err)
		}
		row, err = q.GetIdempotencyKey(ctx, gen.GetIdempotencyKeyParams{MerchantID: mid.UUID(), Operation: operation, IdempotencyKey: key})
		if err == nil {
			return nil, record(row), nil
		}
		if !db.IsNotFound(err) {
			return nil, nil, fmt.Errorf("read idempotency key: %w", err)
		}
		// Collected between our statements; claim afresh.
	}
	return nil, nil, fmt.Errorf("idempotency key %s/%s did not settle", operation, key)
}

// Get reads a key's record, nil when absent.
func (s *Store) Get(ctx context.Context, operation, key string) (*Record, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := s.queries(ctx).GetIdempotencyKey(ctx, gen.GetIdempotencyKeyParams{MerchantID: mid.UUID(), Operation: operation, IdempotencyKey: key})
	if db.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return record(row), nil
}

// DeleteExpired deletes up to limit idempotency rows expired at now, across
// every merchant and store.
func DeleteExpired(ctx context.Context, database *db.DB, now time.Time, limit int32) (int64, error) {
	return database.GenDirectory().DeleteExpiredIdempotencyKeys(ctx, gen.DeleteExpiredIdempotencyKeysParams{Now: now.UTC(), RowLimit: limit})
}

func (s *Store) claim(mid uuid.UUID, row gen.OpenrailsIdempotencyKey) *Claim {
	return &Claim{s: s, merchantID: mid, operation: row.Operation, key: row.IdempotencyKey, token: row.Claims, Reclaimed: row.Claims > 1}
}

func record(row gen.OpenrailsIdempotencyKey) *Record {
	r := &Record{
		Status: Status(row.Status), Result: json.RawMessage(row.Result), Claims: row.Claims,
		LeaseExpiresAt: row.LeaseExpiresAt, ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
	if row.Error != nil {
		r.Error = *row.Error
	}
	return r
}

// Claim is an owned processing claim. Reclaimed reports that an earlier claim
// on the key failed, expired or was abandoned by a dead owner.
type Claim struct {
	s          *Store
	merchantID uuid.UUID
	operation  string
	key        string
	token      int64
	Reclaimed  bool
}

// Renew extends the lease from now. It runs beside the owner's work, so it
// takes its own short-lived pool connection rather than the owner's pin.
func (c *Claim) Renew(ctx context.Context) error {
	now := c.s.clock.Now().UTC()
	n, err := c.s.db.GenDirectory().RenewIdempotencyKey(ctx, gen.RenewIdempotencyKeyParams{
		MerchantID: c.merchantID, Operation: c.operation, IdempotencyKey: c.key, Claims: c.token,
		LeaseExpiresAt: now.Add(c.s.lease), Now: now,
	})
	return fenced(n, err)
}

// Hold renews the lease every quarter lease until the returned stop is
// called, so only a dead owner's claim lapses, never a slow one's.
func (c *Claim) Hold(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	ticker := c.s.clock.NewTicker(c.s.lease / 4)
	go func() {
		defer close(done)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.Chan():
				if err := c.Renew(ctx); err != nil && ctx.Err() == nil {
					log.WithContext(ctx).WithError(err).WithFields(log.Fields{"operation": c.operation, "idempotency_key": c.key}).
						Warn("idempotency lease renewal failed")
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// Complete records the replayable result and releases the lease.
func (c *Claim) Complete(ctx context.Context, result json.RawMessage) error {
	ctx, cancel := detached(ctx)
	defer cancel()
	now := c.s.clock.Now().UTC()
	var body []byte
	if len(result) > 0 {
		body = result
	}
	n, err := c.s.queries(ctx).CompleteIdempotencyKey(ctx, gen.CompleteIdempotencyKeyParams{
		MerchantID: c.merchantID, Operation: c.operation, IdempotencyKey: c.key, Claims: c.token,
		Result: body, ExpiresAt: now.Add(c.s.ttl), Now: now,
	})
	return fenced(n, err)
}

// Fail releases the key: the next Begin reclaims it and runs again.
func (c *Claim) Fail(ctx context.Context, cause error) error {
	ctx, cancel := detached(ctx)
	defer cancel()
	now := c.s.clock.Now().UTC()
	msg := "failed"
	if cause != nil {
		msg = cause.Error()
	}
	n, err := c.s.queries(ctx).FailIdempotencyKey(ctx, gen.FailIdempotencyKeyParams{
		MerchantID: c.merchantID, Operation: c.operation, IdempotencyKey: c.key, Claims: c.token,
		Error: msg, ExpiresAt: now.Add(c.s.ttl), Now: now,
	})
	return fenced(n, err)
}

// detached lets an outcome be recorded after the caller has gone: losing it
// would leave the claim processing until its lease lapses.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return db.DetachedWriteContext(ctx, 10*time.Second)
}

func fenced(n int64, err error) error {
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrClaimLost
	}
	return nil
}
