package holds

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const indexTTL = 7 * 24 * time.Hour

type Hold struct {
	MerchantID      string    `json:"merchant_id"`
	RequestID       string    `json:"request_id"`
	CustomerID      string    `json:"customer_id"`
	Invoker         string    `json:"invoker"`
	InvokerType     string    `json:"invoker_type"`
	Currency        string    `json:"currency"`
	EstimatedAmount int64     `json:"estimated_amount"`
	Source          string    `json:"source,omitempty"`
	Resource        string    `json:"resource,omitempty"`
	PolicyCurrency  string    `json:"policy_currency,omitempty"`
	PolicyAmount    int64     `json:"policy_amount,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	ExpiresAt       time.Time `json:"expires_at"`
}

type Store struct {
	rdb *redis.Client
}

func NewStore(rdb *redis.Client) *Store {
	return &Store{rdb: rdb}
}

func (s *Store) Place(ctx context.Context, h Hold, capacity int64) (bool, int64, error) {
	if s == nil || s.rdb == nil {
		return false, 0, fmt.Errorf("hold store: redis not configured")
	}
	if strings.TrimSpace(h.MerchantID) == "" {
		return false, 0, fmt.Errorf("hold merchant_id required")
	}
	if strings.TrimSpace(h.RequestID) == "" {
		return false, 0, fmt.Errorf("hold request_id required")
	}
	if strings.TrimSpace(h.CustomerID) == "" {
		return false, 0, fmt.Errorf("hold customer_id required")
	}
	if strings.TrimSpace(h.Currency) == "" {
		return false, 0, fmt.Errorf("hold currency required")
	}
	if h.EstimatedAmount <= 0 {
		return false, 0, fmt.Errorf("hold estimated_amount must be > 0")
	}
	now := time.Now().UTC()
	if h.CreatedAt.IsZero() {
		h.CreatedAt = now
	}
	if h.ExpiresAt.IsZero() || !h.ExpiresAt.After(now) {
		return false, 0, fmt.Errorf("hold expires_at must be in the future")
	}
	raw, err := json.Marshal(h)
	if err != nil {
		return false, 0, err
	}
	ttl := time.Until(h.ExpiresAt)
	if ttl <= 0 {
		return false, 0, fmt.Errorf("hold expires_at must be in the future")
	}
	keys := []string{
		holdKey(h.MerchantID, h.RequestID),
		expiryIndexKey(h.MerchantID, h.CustomerID, h.Currency),
		amountIndexKey(h.MerchantID, h.CustomerID, h.Currency),
	}
	res, err := placeScript.Run(ctx, s.rdb, keys,
		h.RequestID,
		now.UnixMilli(),
		h.ExpiresAt.UnixMilli(),
		ttl.Milliseconds(),
		h.EstimatedAmount,
		capacity,
		string(raw),
		indexTTL.Milliseconds(),
	).Slice()
	if err != nil {
		return false, 0, err
	}
	if len(res) != 2 {
		return false, 0, fmt.Errorf("hold store: unexpected place result")
	}
	allowed, _ := toInt64(res[0])
	active, _ := toInt64(res[1])
	return allowed == 1, active, nil
}

func (s *Store) Get(ctx context.Context, merchantID, requestID string) (*Hold, error) {
	if s == nil || s.rdb == nil {
		return nil, fmt.Errorf("hold store: redis not configured")
	}
	raw, err := s.rdb.Get(ctx, holdKey(merchantID, requestID)).Bytes()
	if err != nil {
		return nil, err
	}
	var h Hold
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

func (s *Store) Release(ctx context.Context, merchantID, requestID string) (*Hold, error) {
	h, err := s.Get(ctx, merchantID, requestID)
	if err != nil {
		return nil, err
	}
	pipe := s.rdb.TxPipeline()
	pipe.Del(ctx, holdKey(merchantID, requestID))
	pipe.ZRem(ctx, expiryIndexKey(merchantID, h.CustomerID, h.Currency), requestID)
	pipe.HDel(ctx, amountIndexKey(merchantID, h.CustomerID, h.Currency), requestID)
	_, err = pipe.Exec(ctx)
	return h, err
}

func (s *Store) ActiveAmount(ctx context.Context, merchantID, customerID, currency string) (int64, error) {
	if s == nil || s.rdb == nil {
		return 0, fmt.Errorf("hold store: redis not configured")
	}
	now := time.Now().UTC()
	res, err := activeScript.Run(ctx, s.rdb, []string{
		expiryIndexKey(merchantID, customerID, currency),
		amountIndexKey(merchantID, customerID, currency),
	}, now.UnixMilli()).Int64()
	if err != nil {
		return 0, err
	}
	return res, nil
}

var placeScript = redis.NewScript(`
local existing = redis.call("GET", KEYS[1])
if existing then
  return {1, tonumber(redis.call("HGET", KEYS[3], ARGV[1]) or "0")}
end
local expired = redis.call("ZRANGEBYSCORE", KEYS[2], "-inf", ARGV[2])
for _, member in ipairs(expired) do
  redis.call("ZREM", KEYS[2], member)
  redis.call("HDEL", KEYS[3], member)
end
local vals = redis.call("HVALS", KEYS[3])
local total = 0
for _, value in ipairs(vals) do
  total = total + tonumber(value)
end
local estimate = tonumber(ARGV[5])
local capacity = tonumber(ARGV[6])
if total + estimate > capacity then
  return {0, total}
end
redis.call("SET", KEYS[1], ARGV[7], "PX", ARGV[4])
redis.call("ZADD", KEYS[2], ARGV[3], ARGV[1])
redis.call("HSET", KEYS[3], ARGV[1], ARGV[5])
redis.call("PEXPIRE", KEYS[2], ARGV[8])
redis.call("PEXPIRE", KEYS[3], ARGV[8])
return {1, total + estimate}
`)

var activeScript = redis.NewScript(`
local expired = redis.call("ZRANGEBYSCORE", KEYS[1], "-inf", ARGV[1])
for _, member in ipairs(expired) do
  redis.call("ZREM", KEYS[1], member)
  redis.call("HDEL", KEYS[2], member)
end
local vals = redis.call("HVALS", KEYS[2])
local total = 0
for _, value in ipairs(vals) do
  total = total + tonumber(value)
end
return total
`)

func holdKey(merchantID, requestID string) string {
	return "openrails:hold:" + strings.TrimSpace(merchantID) + ":" + strings.TrimSpace(requestID)
}

func expiryIndexKey(merchantID, customerID, currency string) string {
	return "openrails:holds:" + strings.TrimSpace(merchantID) + ":" + strings.TrimSpace(customerID) + ":" + strings.TrimSpace(currency) + ":expires"
}

func amountIndexKey(merchantID, customerID, currency string) string {
	return "openrails:holds:" + strings.TrimSpace(merchantID) + ":" + strings.TrimSpace(customerID) + ":" + strings.TrimSpace(currency) + ":amounts"
}

func toInt64(v any) (int64, error) {
	switch x := v.(type) {
	case int64:
		return x, nil
	case int:
		return int64(x), nil
	case string:
		return strconv.ParseInt(x, 10, 64)
	case []byte:
		return strconv.ParseInt(string(x), 10, 64)
	default:
		return 0, fmt.Errorf("unexpected redis integer type %T", v)
	}
}
