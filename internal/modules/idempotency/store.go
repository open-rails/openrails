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
// A processing claim is leased, on the database's clock. Its owner renews the
// lease (Claim.Hold) on a small pool of its own, so renewals never queue
// behind request traffic. When a renewal cannot be confirmed before the lease
// could lapse, Hold cancels the owner's context: an owner stops before anyone
// else can reclaim. A lapsed claim passes to exactly one later Begin. The
// token fences the superseded owner: its Complete, Fail and Renew are refused,
// and Claim.InTx lets its transactions refuse to commit.
package idempotency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

// ErrClaimLost means the claim lapsed or passed to another caller; the
// superseded owner may not act on it.
var ErrClaimLost = errors.New("idempotency claim was lost")

type Record struct {
	Status Status
	Result json.RawMessage
	Error  string
	Claims int64
	// Leased reports a processing claim whose lease has not lapsed (Get only).
	Leased         bool
	LeaseExpiresAt time.Time
	ExpiresAt      time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Store claims keys for one class of work. TTL bounds how long a result is
// replayed; Lease is how long a silent owner keeps its claim.
type Store struct {
	db     *db.DB
	leases *db.DB
	ttl    time.Duration
	lease  time.Duration
}

// NewStore claims on database; leases is the small pool lease renewals use.
func NewStore(database, leases *db.DB, ttl, lease time.Duration) (*Store, error) {
	if database == nil || leases == nil || ttl <= 0 || lease <= 0 || lease > ttl {
		return nil, fmt.Errorf("idempotency store needs a database, a lease pool and 0 < lease <= ttl")
	}
	return &Store{db: database, leases: leases, ttl: ttl, lease: lease}, nil
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
		token := uuid.New()
		start := time.Now()
		row, err := q.ClaimIdempotencyKey(ctx, gen.ClaimIdempotencyKeyParams{
			MerchantID: mid.UUID(), Operation: operation, IdempotencyKey: key, Token: token,
			LeaseSeconds: s.lease.Seconds(), TtlSeconds: s.ttl.Seconds(),
		})
		if err == nil {
			return s.claim(mid.UUID(), row, start), nil, nil
		}
		if !db.IsNotFound(err) {
			return nil, nil, fmt.Errorf("claim idempotency key: %w", err)
		}
		row, err = q.ReclaimIdempotencyKey(ctx, gen.ReclaimIdempotencyKeyParams{
			MerchantID: mid.UUID(), Operation: operation, IdempotencyKey: key, Token: token,
			LeaseSeconds: s.lease.Seconds(), TtlSeconds: s.ttl.Seconds(),
		})
		if err == nil {
			return s.claim(mid.UUID(), row, start), nil, nil
		}
		if !db.IsNotFound(err) {
			return nil, nil, fmt.Errorf("reclaim idempotency key: %w", err)
		}
		rec, err := s.get(ctx, q, mid.UUID(), operation, key)
		if err != nil {
			return nil, nil, fmt.Errorf("read idempotency key: %w", err)
		}
		if rec != nil {
			return nil, rec, nil
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
	return s.get(ctx, s.queries(ctx), mid.UUID(), operation, key)
}

// Watch reads a key's record on the lease pool, holding no request
// connection: for callers that wait out another owner.
func (s *Store) Watch(ctx context.Context, operation, key string) (*Record, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return s.get(ctx, s.leases.GenDirectory(), mid.UUID(), operation, key)
}

func (s *Store) get(ctx context.Context, q *gen.Queries, mid uuid.UUID, operation, key string) (*Record, error) {
	row, err := q.GetIdempotencyKey(ctx, gen.GetIdempotencyKeyParams{MerchantID: mid, Operation: operation, IdempotencyKey: key})
	if db.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r := record(gen.OpenrailsIdempotencyKey{
		MerchantID: row.MerchantID, Operation: row.Operation, IdempotencyKey: row.IdempotencyKey, Status: row.Status,
		Token: row.Token, Claims: row.Claims, Result: row.Result, Error: row.Error,
		LeaseExpiresAt: row.LeaseExpiresAt, ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	})
	r.Leased = row.Leased
	return r, nil
}

// DeleteExpired deletes up to limit idempotency rows past their expiry,
// across every merchant and store.
func DeleteExpired(ctx context.Context, database *db.DB, limit int32) (int64, error) {
	return database.GenDirectory().DeleteExpiredIdempotencyKeys(ctx, limit)
}

func (s *Store) claim(mid uuid.UUID, row gen.OpenrailsIdempotencyKey, start time.Time) *Claim {
	return &Claim{s: s, merchantID: mid, operation: row.Operation, key: row.IdempotencyKey, token: row.Token,
		Reclaimed: row.Claims > 1, confirmed: start}
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
// on the key failed, expired or was abandoned.
type Claim struct {
	s          *Store
	merchantID uuid.UUID
	operation  string
	key        string
	token      uuid.UUID
	Reclaimed  bool
	// confirmed is when the last confirmed lease was requested: the lease
	// runs from at least then, on the database's clock.
	confirmed time.Time
}

// Renew extends the lease; ErrClaimLost when it already lapsed or passed on.
func (c *Claim) Renew(ctx context.Context) error {
	_, err := c.renew(ctx)
	return err
}

// renew returns when the confirmed lease was requested.
func (c *Claim) renew(ctx context.Context) (time.Time, error) {
	start := time.Now()
	n, err := c.s.leases.GenDirectory().RenewIdempotencyKey(ctx, gen.RenewIdempotencyKeyParams{
		MerchantID: c.merchantID, Operation: c.operation, IdempotencyKey: c.key, Token: c.token,
		LeaseSeconds: c.s.lease.Seconds(),
	})
	return start, fenced(n, err)
}

// Hold renews the lease every quarter lease and returns the context the owner
// works under. It is cancelled with ErrClaimLost once a renewal is refused,
// or when none was confirmed in time to be sure the lease has not lapsed.
// stop ends the renewals; call it before Complete or Fail.
func (c *Claim) Hold(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	quit := make(chan struct{})
	done := make(chan struct{})
	margin := c.s.lease / 5
	confirmed := c.confirmed
	go func() {
		defer close(done)
		ticker := time.NewTicker(c.s.lease / 4)
		defer ticker.Stop()
		deadline := time.NewTimer(time.Until(confirmed.Add(c.s.lease - margin)))
		defer deadline.Stop()
		for {
			select {
			case <-quit:
				return
			case <-ctx.Done():
				return
			case <-deadline.C:
				cancel(ErrClaimLost)
				return
			case <-ticker.C:
				// A renewal never runs past the deadline it could extend.
				rctx, rcancel := context.WithDeadline(ctx, confirmed.Add(c.s.lease-margin))
				at, err := c.renew(rctx)
				rcancel()
				if errors.Is(err, ErrClaimLost) {
					cancel(ErrClaimLost)
					return
				}
				if err != nil {
					log.WithContext(ctx).WithError(err).WithFields(log.Fields{"operation": c.operation, "idempotency_key": c.key}).
						Warn("idempotency lease renewal failed")
					continue
				}
				confirmed = at
				deadline.Reset(time.Until(confirmed.Add(c.s.lease - margin)))
			}
		}
	}()
	var once sync.Once
	return ctx, func() {
		once.Do(func() { close(quit) })
		<-done
		cancel(context.Canceled)
	}
}

// InTx proves inside tx that the claim is still held, and share-locks it so
// no reclaim can pass it on before tx ends. Use it as a commit guard.
func (c *Claim) InTx(ctx context.Context, tx pgx.Tx) error {
	_, err := gen.New(tx).HoldIdempotencyKeyInTx(ctx, gen.HoldIdempotencyKeyInTxParams{
		MerchantID: c.merchantID, Operation: c.operation, IdempotencyKey: c.key, Token: c.token,
	})
	if db.IsNotFound(err) {
		return ErrClaimLost
	}
	return err
}

// Complete records the replayable result and releases the lease.
func (c *Claim) Complete(ctx context.Context, result json.RawMessage) error {
	ctx, cancel := detached(ctx)
	defer cancel()
	var body []byte
	if len(result) > 0 {
		body = result
	}
	n, err := c.s.queries(ctx).CompleteIdempotencyKey(ctx, gen.CompleteIdempotencyKeyParams{
		MerchantID: c.merchantID, Operation: c.operation, IdempotencyKey: c.key, Token: c.token,
		Result: body, TtlSeconds: c.s.ttl.Seconds(),
	})
	return fenced(n, err)
}

// Fail releases the key: the next Begin reclaims it and runs again.
func (c *Claim) Fail(ctx context.Context, cause error) error {
	ctx, cancel := detached(ctx)
	defer cancel()
	msg := "failed"
	if cause != nil {
		msg = cause.Error()
	}
	n, err := c.s.queries(ctx).FailIdempotencyKey(ctx, gen.FailIdempotencyKeyParams{
		MerchantID: c.merchantID, Operation: c.operation, IdempotencyKey: c.key, Token: c.token,
		Error: msg, TtlSeconds: c.s.ttl.Seconds(),
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
