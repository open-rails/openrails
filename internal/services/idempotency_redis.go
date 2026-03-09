package services

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"
)

const (
	// DefaultIdempotencyTTL is the default time-to-live for idempotency keys
	DefaultIdempotencyTTL = 5 * time.Minute

	// Redis key prefix for idempotency
	idempotencyKeyPrefix = "idemp:"
)

// IdempotencyStatus represents the status of an idempotency request
type IdempotencyStatus string

const (
	IdempotencyStatusPending IdempotencyStatus = "pending"
	IdempotencyStatusSuccess IdempotencyStatus = "success"
	IdempotencyStatusFailed  IdempotencyStatus = "failed"
)

// IdempotencyRecord represents a stored idempotency record
type IdempotencyRecord struct {
	Status    IdempotencyStatus `json:"status"`
	Result    json.RawMessage   `json:"result,omitempty"`
	Error     string            `json:"error,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

// IdempotencyService provides idempotency with Redis backend and in-memory fallback.
// Keys automatically expire after TTL.
type IdempotencyService struct {
	client *redis.Client
	ttl    time.Duration

	// In-memory fallback for when Redis is unavailable
	mu       sync.RWMutex
	memStore map[string]*memEntry
}

type memEntry struct {
	record    *IdempotencyRecord
	expiresAt time.Time
}

// NewIdempotencyService creates a new idempotency service.
// If redisClient is nil, uses in-memory storage only.
func NewIdempotencyService(redisClient *redis.Client) *IdempotencyService {
	s := &IdempotencyService{
		client:   redisClient,
		ttl:      DefaultIdempotencyTTL,
		memStore: make(map[string]*memEntry),
	}

	// Start cleanup goroutine for in-memory fallback
	go s.cleanupLoop()

	return s
}

// NewIdempotencyServiceWithTTL creates a new idempotency service with custom TTL
func NewIdempotencyServiceWithTTL(redisClient *redis.Client, ttl time.Duration) *IdempotencyService {
	s := &IdempotencyService{
		client:   redisClient,
		ttl:      ttl,
		memStore: make(map[string]*memEntry),
	}
	go s.cleanupLoop()
	return s
}

// cleanupLoop periodically removes expired entries from in-memory store
func (s *IdempotencyService) cleanupLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		s.mu.Lock()
		now := time.Now()
		for k, v := range s.memStore {
			if now.After(v.expiresAt) {
				delete(s.memStore, k)
			}
		}
		s.mu.Unlock()
	}
}

// Begin starts an idempotency-protected operation.
// Returns (record, alreadyExists, error).
// If alreadyExists is true, check record.Status to determine action.
func (s *IdempotencyService) Begin(ctx context.Context, operation, key string) (*IdempotencyRecord, bool, error) {
	fullKey := s.buildKey(operation, key)

	// Try Redis first if available
	if s.client != nil {
		record, exists, err := s.beginRedis(ctx, fullKey)
		if err != nil {
			log.WithError(err).Warn("Redis idempotency failed, falling back to in-memory")
		} else {
			return record, exists, nil
		}
	}

	// In-memory fallback
	return s.beginMemory(fullKey)
}

func (s *IdempotencyService) beginRedis(ctx context.Context, redisKey string) (*IdempotencyRecord, bool, error) {
	// Try to get existing record first
	existing, err := s.getRedis(ctx, redisKey)
	if err == nil {
		return existing, true, nil
	}
	if err != nil && err != redis.Nil {
		return nil, false, fmt.Errorf("redis get: %w", err)
	}

	// Create new pending record
	record := &IdempotencyRecord{
		Status:    IdempotencyStatusPending,
		CreatedAt: time.Now(),
	}

	recordJSON, err := json.Marshal(record)
	if err != nil {
		return nil, false, fmt.Errorf("marshal record: %w", err)
	}

	// SET NX - only set if key doesn't exist (atomic)
	set, err := s.client.SetNX(ctx, redisKey, recordJSON, s.ttl).Result()
	if err != nil {
		return nil, false, fmt.Errorf("redis setnx: %w", err)
	}

	if !set {
		// Key was set by another request between our GET and SETNX
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

	// Check if exists and not expired
	if entry, ok := s.memStore[key]; ok {
		if now.Before(entry.expiresAt) {
			return entry.record, true, nil
		}
		// Expired, remove it
		delete(s.memStore, key)
	}

	// Create new record
	record := &IdempotencyRecord{
		Status:    IdempotencyStatusPending,
		CreatedAt: now,
	}
	s.memStore[key] = &memEntry{
		record:    record,
		expiresAt: now.Add(s.ttl),
	}

	return record, false, nil
}

// Complete marks an idempotency request as successful with the result
func (s *IdempotencyService) Complete(ctx context.Context, operation, key string, result json.RawMessage) error {
	fullKey := s.buildKey(operation, key)

	record := &IdempotencyRecord{
		Status:    IdempotencyStatusSuccess,
		Result:    result,
		CreatedAt: time.Now(),
	}

	// Try Redis first
	if s.client != nil {
		if err := s.setRedis(ctx, fullKey, record); err != nil {
			log.WithError(err).Warn("Redis complete failed, updating in-memory")
		} else {
			return nil
		}
	}

	// In-memory fallback
	s.setMemory(fullKey, record)
	return nil
}

// Fail marks an idempotency request as failed with the error
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

	// Use shorter TTL for failures so user can retry sooner
	failureTTL := s.ttl / 2
	if failureTTL < time.Minute {
		failureTTL = time.Minute
	}

	// Try Redis first
	if s.client != nil {
		if err := s.setRedisWithTTL(ctx, fullKey, record, failureTTL); err != nil {
			log.WithError(err).Warn("Redis fail failed, updating in-memory")
		} else {
			return nil
		}
	}

	// In-memory fallback
	s.setMemoryWithTTL(fullKey, record, failureTTL)
	return nil
}

// Get retrieves an idempotency record if it exists
func (s *IdempotencyService) Get(ctx context.Context, operation, key string) (*IdempotencyRecord, error) {
	fullKey := s.buildKey(operation, key)

	if s.client != nil {
		record, err := s.getRedis(ctx, fullKey)
		if err == nil {
			return record, nil
		}
		if err != redis.Nil {
			log.WithError(err).Warn("Redis get failed, checking in-memory")
		}
	}

	return s.getMemory(fullKey), nil
}

func (s *IdempotencyService) getRedis(ctx context.Context, redisKey string) (*IdempotencyRecord, error) {
	data, err := s.client.Get(ctx, redisKey).Bytes()
	if err != nil {
		return nil, err
	}

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
			return entry.record
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
		record:    record,
		expiresAt: time.Now().Add(ttl),
	}
}

func (s *IdempotencyService) buildKey(operation, key string) string {
	return idempotencyKeyPrefix + operation + ":" + key
}

// Key generation helpers - no time bucket needed since TTL handles expiration

// GenerateKeyForSale generates idempotency key for one-time sale
// Format: {user_id}:{price_id}
func GenerateKeyForSale(userID string, priceID uuid.UUID) string {
	return fmt.Sprintf("%s:%s", userID, priceID.String())
}

// GenerateKeyForSubscription generates idempotency key for subscription creation
// Format: {user_id}:{price_id}
func GenerateKeyForSubscription(userID string, priceID uuid.UUID) string {
	return fmt.Sprintf("%s:%s", userID, priceID.String())
}

// GenerateKeyForUpgrade generates idempotency key for subscription upgrade
// Format: {user_id}:{old_subscription_id}:{new_price_id}
func GenerateKeyForUpgrade(userID string, oldSubscriptionID, newPriceID uuid.UUID) string {
	return fmt.Sprintf("%s:%s:%s", userID, oldSubscriptionID.String(), newPriceID.String())
}

// GenerateKeyForRebill generates idempotency key for subscription rebill (dunning)
// Format: {subscription_id}:{period_end_iso}
func GenerateKeyForRebill(subscriptionID uuid.UUID, periodEndISO string) string {
	return fmt.Sprintf("%s:%s", subscriptionID.String(), periodEndISO)
}
