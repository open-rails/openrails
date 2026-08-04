// Package replaycache is REQUEST-REPLAY COORDINATION over Redis. It was called
// `idempotency`, and that name was the problem (or#892).
//
// THE BOUNDARY, stated once so it stops being re-derived:
//
//   - MONEY idempotency is `money.IdempotencyKey` + the unique index behind
//     `ledger.ApplyIdempotent`. It is a POSTGRES fact. Losing Redis cannot
//     double-charge anyone.
//   - THIS package is a CACHE with a per-process in-memory fallback. It
//     coordinates duplicate work and serves cached HTTP responses. It is NOT a
//     durable claim, and nothing whose correctness is money may rest on it.
//
// A package named `idempotency` sitting beside a money key that means something
// stricter is exactly the ambiguity that let `SpendCredits` ship with an
// optional key while `CaptureAuthorized` required one. The rename does not
// change behaviour; it stops the next reader assuming this store is the
// guarantee.
//
// For webhook dedup the line is already drawn correctly — the replay truth
// lives in Postgres (openrails.webhook_events, #678), so losing Redis costs
// duplicate work coordination, never correctness.
//
// KNOWN GAP, deliberately not fixed here: the checkout path still uses this as
// its SOLE store, so a flush there loses the claim outright. That is the same
// class as the wasted-spend SetNX (or#894 §4.23) and wants its own induced
// measurement before anyone calls it safe — tracked in or#892, not closed by
// this rename.
package replaycache

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	DefaultTTL           = 5 * time.Minute
	idempotencyKeyPrefix = "idemp:"
	// HTTPReplayTTL is the replay window for the client-facing Idempotency-Key
	// middleware (#579). 24h matches Stripe's documented window.
	HTTPReplayTTL = 24 * time.Hour
)

type Status string

const (
	StatusPending Status = "pending"
	StatusSuccess Status = "success"
	StatusFailed  Status = "failed"
)

type Record struct {
	Status    Status          `json:"status"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// Store is a Redis-backed pending/success/failed record store, with a
// per-process in-memory fallback when no Redis client is configured. See the
// package doc for the boundary against money idempotency: this is coordination
// and cache, never a durable claim.
type Store struct {
	client *redis.Client
	ttl    time.Duration

	mu       sync.RWMutex
	memStore map[string]*memEntry
	stopCh   chan struct{}
	stopOnce sync.Once
}

type memEntry struct {
	record    *Record
	expiresAt time.Time
}

func NewStore(redisClient *redis.Client) *Store {
	s := &Store{
		client:   redisClient,
		ttl:      DefaultTTL,
		memStore: make(map[string]*memEntry),
		stopCh:   make(chan struct{}),
	}

	go s.cleanupLoop()

	return s
}

func NewStoreWithTTL(redisClient *redis.Client, ttl time.Duration) *Store {
	s := &Store{
		client:   redisClient,
		ttl:      ttl,
		memStore: make(map[string]*memEntry),
		stopCh:   make(chan struct{}),
	}
	go s.cleanupLoop()
	return s
}

func (s *Store) cleanupLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.mu.Lock()
			now := time.Now()
			for key, entry := range s.memStore {
				if now.After(entry.expiresAt) {
					delete(s.memStore, key)
				}
			}
			s.mu.Unlock()
		case <-s.stopCh:
			return
		}
	}
}

func (s *Store) Close() {
	if s == nil {
		return
	}
	if s.stopCh == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
}

func (s *Store) Begin(ctx context.Context, operation, key string) (*Record, bool, error) {
	fullKey := s.buildKey(operation, key)

	if s.client != nil {
		record, exists, err := s.beginRedis(ctx, fullKey)
		if err != nil {
			return nil, false, fmt.Errorf("redis idempotency begin failed: %w", err)
		}
		return record, exists, nil
	}

	return s.beginMemory(fullKey)
}

func (s *Store) TryTakeoverPending(ctx context.Context, operation, key string, olderThan time.Duration) (bool, error) {
	fullKey := s.buildKey(operation, key)
	if s.client != nil {
		return s.tryTakeoverPendingRedis(ctx, fullKey, olderThan)
	}
	return s.tryTakeoverPendingMemory(fullKey, olderThan), nil
}

// RenewPending refreshes a still-pending record's CreatedAt (lease heartbeat)
// so stale-pending takeover only fires for dead holders, not slow ones (#678).
// No-op (false) if the record is gone or no longer pending.
func (s *Store) RenewPending(ctx context.Context, operation, key string) (bool, error) {
	return s.TryTakeoverPending(ctx, operation, key, 0)
}

func (s *Store) tryTakeoverPendingRedis(ctx context.Context, redisKey string, olderThan time.Duration) (bool, error) {
	taken := false
	err := s.client.Watch(ctx, func(tx *redis.Tx) error {
		record, err := s.getRedisTx(ctx, tx, redisKey)
		if err != nil {
			if err == redis.Nil {
				return nil
			}
			return err
		}
		if record.Status != StatusPending || time.Since(record.CreatedAt) <= olderThan {
			return nil
		}
		record.CreatedAt = time.Now()
		recordJSON, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("marshal record: %w", err)
		}
		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Set(ctx, redisKey, recordJSON, s.ttl)
			return nil
		})
		if err == nil {
			taken = true
		}
		return err
	}, redisKey)
	if err == redis.TxFailedErr {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("redis pending takeover: %w", err)
	}
	return taken, nil
}

func (s *Store) tryTakeoverPendingMemory(key string, olderThan time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.memStore[key]
	if !ok || entry == nil || entry.record == nil {
		return false
	}
	if entry.record.Status != StatusPending || time.Since(entry.record.CreatedAt) <= olderThan {
		return false
	}
	entry.record.CreatedAt = time.Now()
	entry.expiresAt = time.Now().Add(s.ttl)
	return true
}

func (s *Store) beginRedis(ctx context.Context, redisKey string) (*Record, bool, error) {
	existing, err := s.getRedis(ctx, redisKey)
	if err == nil {
		return existing, true, nil
	}
	if err != nil && err != redis.Nil {
		return nil, false, fmt.Errorf("redis get: %w", err)
	}

	record := &Record{
		Status:    StatusPending,
		CreatedAt: time.Now(),
	}

	recordJSON, err := json.Marshal(record)
	if err != nil {
		return nil, false, fmt.Errorf("marshal record: %w", err)
	}

	set, err := s.client.SetNX(ctx, redisKey, recordJSON, s.ttl).Result()
	if err != nil {
		return nil, false, fmt.Errorf("redis setnx: %w", err)
	}

	if !set {
		existing, err := s.getRedis(ctx, redisKey)
		if err != nil {
			return nil, false, fmt.Errorf("redis get after race: %w", err)
		}
		return existing, true, nil
	}

	return record, false, nil
}

func (s *Store) beginMemory(key string) (*Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	if entry, ok := s.memStore[key]; ok {
		if now.Before(entry.expiresAt) {
			return cloneRecord(entry.record), true, nil
		}
		delete(s.memStore, key)
	}

	record := &Record{
		Status:    StatusPending,
		CreatedAt: now,
	}
	s.memStore[key] = &memEntry{
		record:    record,
		expiresAt: now.Add(s.ttl),
	}

	return cloneRecord(record), false, nil
}

func (s *Store) Complete(ctx context.Context, operation, key string, result json.RawMessage) error {
	fullKey := s.buildKey(operation, key)

	record := &Record{
		Status:    StatusSuccess,
		Result:    result,
		CreatedAt: time.Now(),
	}

	if s.client != nil {
		if err := s.setRedis(ctx, fullKey, record); err != nil {
			return fmt.Errorf("redis idempotency complete failed: %w", err)
		}
		return nil
	}

	s.setMemory(fullKey, record)
	return nil
}

func (s *Store) Fail(ctx context.Context, operation, key string, failure error) error {
	fullKey := s.buildKey(operation, key)

	errMsg := ""
	if failure != nil {
		errMsg = failure.Error()
	}

	record := &Record{
		Status:    StatusFailed,
		Error:     errMsg,
		CreatedAt: time.Now(),
	}

	failureTTL := s.ttl / 2
	if failureTTL < time.Minute {
		failureTTL = time.Minute
	}

	if s.client != nil {
		if err := s.setRedisWithTTL(ctx, fullKey, record, failureTTL); err != nil {
			return fmt.Errorf("redis idempotency fail failed: %w", err)
		}
		return nil
	}

	s.setMemoryWithTTL(fullKey, record, failureTTL)
	return nil
}

func (s *Store) Get(ctx context.Context, operation, key string) (*Record, error) {
	fullKey := s.buildKey(operation, key)

	if s.client != nil {
		record, err := s.getRedis(ctx, fullKey)
		if err == nil {
			return record, nil
		}
		if err != redis.Nil {
			return nil, fmt.Errorf("redis idempotency get failed: %w", err)
		}
		return nil, nil
	}

	return s.getMemory(fullKey), nil
}

func (s *Store) getRedis(ctx context.Context, redisKey string) (*Record, error) {
	data, err := s.client.Get(ctx, redisKey).Bytes()
	if err != nil {
		return nil, err
	}
	return decodeRecord(data)
}

func (s *Store) getRedisTx(ctx context.Context, tx *redis.Tx, redisKey string) (*Record, error) {
	data, err := tx.Get(ctx, redisKey).Bytes()
	if err != nil {
		return nil, err
	}
	return decodeRecord(data)
}

func decodeRecord(data []byte) (*Record, error) {
	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("unmarshal record: %w", err)
	}

	return &record, nil
}

func (s *Store) getMemory(key string) *Record {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if entry, ok := s.memStore[key]; ok {
		if time.Now().Before(entry.expiresAt) {
			return cloneRecord(entry.record)
		}
	}
	return nil
}

func (s *Store) setRedis(ctx context.Context, key string, record *Record) error {
	return s.setRedisWithTTL(ctx, key, record, s.ttl)
}

func (s *Store) setRedisWithTTL(ctx context.Context, key string, record *Record, ttl time.Duration) error {
	recordJSON, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal record: %w", err)
	}
	return s.client.Set(ctx, key, recordJSON, ttl).Err()
}

func (s *Store) setMemory(key string, record *Record) {
	s.setMemoryWithTTL(key, record, s.ttl)
}

func (s *Store) setMemoryWithTTL(key string, record *Record, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.memStore[key] = &memEntry{
		record:    cloneRecord(record),
		expiresAt: time.Now().Add(ttl),
	}
}

func cloneRecord(record *Record) *Record {
	if record == nil {
		return nil
	}
	clone := *record
	clone.Result = append(json.RawMessage(nil), record.Result...)
	return &clone
}

func (s *Store) buildKey(operation, key string) string {
	return idempotencyKeyPrefix + operation + ":" + key
}
