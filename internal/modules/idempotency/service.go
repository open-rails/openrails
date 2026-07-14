package idempotency

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	DefaultIdempotencyTTL = 5 * time.Minute
	idempotencyKeyPrefix  = "idemp:"
	// HTTPIdempotencyTTL is the replay window for the client-facing Idempotency-Key
	// middleware (#579). 24h matches Stripe's documented window.
	HTTPIdempotencyTTL = 24 * time.Hour
)

type IdempotencyStatus string

const (
	IdempotencyStatusPending IdempotencyStatus = "pending"
	IdempotencyStatusSuccess IdempotencyStatus = "success"
	IdempotencyStatusFailed  IdempotencyStatus = "failed"
)

type IdempotencyRecord struct {
	Status    IdempotencyStatus `json:"status"`
	Result    json.RawMessage   `json:"result,omitempty"`
	Error     string            `json:"error,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

// IdempotencyService is a Redis-backed pending/success/failed record store,
// with a per-process in-memory fallback when no Redis client is configured.
// For webhook dedup it is COORDINATION + CACHE only — the replay truth lives
// in Postgres (openrails.webhook_events, #678) — so losing Redis (or falling
// back to the memStore) costs duplicate work coordination, never correctness
// there. Other consumers (e.g. checkout) still use it as their sole store.
type IdempotencyService struct {
	client *redis.Client
	ttl    time.Duration

	mu       sync.RWMutex
	memStore map[string]*memEntry
	stopCh   chan struct{}
	stopOnce sync.Once
}

type memEntry struct {
	record    *IdempotencyRecord
	expiresAt time.Time
}

func NewIdempotencyService(redisClient *redis.Client) *IdempotencyService {
	s := &IdempotencyService{
		client:   redisClient,
		ttl:      DefaultIdempotencyTTL,
		memStore: make(map[string]*memEntry),
		stopCh:   make(chan struct{}),
	}

	go s.cleanupLoop()

	return s
}

func NewIdempotencyServiceWithTTL(redisClient *redis.Client, ttl time.Duration) *IdempotencyService {
	s := &IdempotencyService{
		client:   redisClient,
		ttl:      ttl,
		memStore: make(map[string]*memEntry),
		stopCh:   make(chan struct{}),
	}
	go s.cleanupLoop()
	return s
}

func (s *IdempotencyService) cleanupLoop() {
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

func (s *IdempotencyService) Close() {
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

func (s *IdempotencyService) Begin(ctx context.Context, operation, key string) (*IdempotencyRecord, bool, error) {
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

func (s *IdempotencyService) TryTakeoverPending(ctx context.Context, operation, key string, olderThan time.Duration) (bool, error) {
	fullKey := s.buildKey(operation, key)
	if s.client != nil {
		return s.tryTakeoverPendingRedis(ctx, fullKey, olderThan)
	}
	return s.tryTakeoverPendingMemory(fullKey, olderThan), nil
}

// RenewPending refreshes a still-pending record's CreatedAt (lease heartbeat)
// so stale-pending takeover only fires for dead holders, not slow ones (#678).
// No-op (false) if the record is gone or no longer pending.
func (s *IdempotencyService) RenewPending(ctx context.Context, operation, key string) (bool, error) {
	return s.TryTakeoverPending(ctx, operation, key, 0)
}

func (s *IdempotencyService) tryTakeoverPendingRedis(ctx context.Context, redisKey string, olderThan time.Duration) (bool, error) {
	taken := false
	err := s.client.Watch(ctx, func(tx *redis.Tx) error {
		record, err := s.getRedisTx(ctx, tx, redisKey)
		if err != nil {
			if err == redis.Nil {
				return nil
			}
			return err
		}
		if record.Status != IdempotencyStatusPending || time.Since(record.CreatedAt) <= olderThan {
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

func (s *IdempotencyService) tryTakeoverPendingMemory(key string, olderThan time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.memStore[key]
	if !ok || entry == nil || entry.record == nil {
		return false
	}
	if entry.record.Status != IdempotencyStatusPending || time.Since(entry.record.CreatedAt) <= olderThan {
		return false
	}
	entry.record.CreatedAt = time.Now()
	entry.expiresAt = time.Now().Add(s.ttl)
	return true
}

func (s *IdempotencyService) beginRedis(ctx context.Context, redisKey string) (*IdempotencyRecord, bool, error) {
	existing, err := s.getRedis(ctx, redisKey)
	if err == nil {
		return existing, true, nil
	}
	if err != nil && err != redis.Nil {
		return nil, false, fmt.Errorf("redis get: %w", err)
	}

	record := &IdempotencyRecord{
		Status:    IdempotencyStatusPending,
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

func (s *IdempotencyService) beginMemory(key string) (*IdempotencyRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	if entry, ok := s.memStore[key]; ok {
		if now.Before(entry.expiresAt) {
			return cloneIdempotencyRecord(entry.record), true, nil
		}
		delete(s.memStore, key)
	}

	record := &IdempotencyRecord{
		Status:    IdempotencyStatusPending,
		CreatedAt: now,
	}
	s.memStore[key] = &memEntry{
		record:    record,
		expiresAt: now.Add(s.ttl),
	}

	return cloneIdempotencyRecord(record), false, nil
}

func (s *IdempotencyService) Complete(ctx context.Context, operation, key string, result json.RawMessage) error {
	fullKey := s.buildKey(operation, key)

	record := &IdempotencyRecord{
		Status:    IdempotencyStatusSuccess,
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

func (s *IdempotencyService) Fail(ctx context.Context, operation, key string, failure error) error {
	fullKey := s.buildKey(operation, key)

	errMsg := ""
	if failure != nil {
		errMsg = failure.Error()
	}

	record := &IdempotencyRecord{
		Status:    IdempotencyStatusFailed,
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

func (s *IdempotencyService) Get(ctx context.Context, operation, key string) (*IdempotencyRecord, error) {
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

func (s *IdempotencyService) getRedis(ctx context.Context, redisKey string) (*IdempotencyRecord, error) {
	data, err := s.client.Get(ctx, redisKey).Bytes()
	if err != nil {
		return nil, err
	}
	return decodeIdempotencyRecord(data)
}

func (s *IdempotencyService) getRedisTx(ctx context.Context, tx *redis.Tx, redisKey string) (*IdempotencyRecord, error) {
	data, err := tx.Get(ctx, redisKey).Bytes()
	if err != nil {
		return nil, err
	}
	return decodeIdempotencyRecord(data)
}

func decodeIdempotencyRecord(data []byte) (*IdempotencyRecord, error) {
	var record IdempotencyRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("unmarshal record: %w", err)
	}

	return &record, nil
}

func (s *IdempotencyService) getMemory(key string) *IdempotencyRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if entry, ok := s.memStore[key]; ok {
		if time.Now().Before(entry.expiresAt) {
			return cloneIdempotencyRecord(entry.record)
		}
	}
	return nil
}

func (s *IdempotencyService) setRedis(ctx context.Context, key string, record *IdempotencyRecord) error {
	return s.setRedisWithTTL(ctx, key, record, s.ttl)
}

func (s *IdempotencyService) setRedisWithTTL(ctx context.Context, key string, record *IdempotencyRecord, ttl time.Duration) error {
	recordJSON, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal record: %w", err)
	}
	return s.client.Set(ctx, key, recordJSON, ttl).Err()
}

func (s *IdempotencyService) setMemory(key string, record *IdempotencyRecord) {
	s.setMemoryWithTTL(key, record, s.ttl)
}

func (s *IdempotencyService) setMemoryWithTTL(key string, record *IdempotencyRecord, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.memStore[key] = &memEntry{
		record:    cloneIdempotencyRecord(record),
		expiresAt: time.Now().Add(ttl),
	}
}

func cloneIdempotencyRecord(record *IdempotencyRecord) *IdempotencyRecord {
	if record == nil {
		return nil
	}
	clone := *record
	clone.Result = append(json.RawMessage(nil), record.Result...)
	return &clone
}

func (s *IdempotencyService) buildKey(operation, key string) string {
	return idempotencyKeyPrefix + operation + ":" + key
}
