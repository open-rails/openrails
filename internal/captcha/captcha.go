// Package captcha verifies provider-neutral siteverify tokens and tracks captcha challenges.
package captcha

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	redis "github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
)

const (
	// TokenHeader is the request header clients use to submit a captcha response token.
	TokenHeader = "X-Captcha-Token"

	maxMemoryEntries = 10_000
)

// Verifier validates captcha response tokens against a provider siteverify endpoint.
type Verifier interface {
	Verify(ctx context.Context, req VerifyRequest) (*VerifyResult, error)
}

// VerifyRequest describes a captcha token verification attempt.
type VerifyRequest struct {
	Token    string
	RemoteIP string
	Bucket   string
}

// VerifyResult contains the normalized siteverify response.
type VerifyResult struct {
	Success    bool
	Score      *float64
	Action     string
	Hostname   string
	ErrorCodes []string
}

type siteVerifyVerifier struct {
	cfg    *config.CaptchaConfig
	client *http.Client
}

// ChallengeStore tracks challenged and solved IP/bucket pairs with Redis and in-memory fallback.
type ChallengeStore struct {
	rdb        *redis.Client
	mu         sync.Mutex
	challenged map[string]time.Time
}

// NewVerifier returns a provider-neutral captcha verifier when captcha is enabled.
func NewVerifier(cfg *config.CaptchaConfig, client *http.Client) Verifier {
	if cfg == nil || !cfg.Enabled {
		return nil
	}

	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}

	return &siteVerifyVerifier{cfg: cfg, client: client}
}

func (v *siteVerifyVerifier) Verify(ctx context.Context, req VerifyRequest) (*VerifyResult, error) {
	if v == nil || v.cfg == nil {
		return nil, fmt.Errorf("captcha verifier is not configured")
	}

	token := strings.TrimSpace(req.Token)
	if token == "" {
		return &VerifyResult{Success: false, ErrorCodes: []string{"missing-input-response"}}, nil
	}

	form := url.Values{}
	form.Set("secret", strings.TrimSpace(v.cfg.SecretKey))
	form.Set("response", token)
	if remoteIP := strings.TrimSpace(req.RemoteIP); remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, v.cfg.EffectiveVerifyURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create captcha verify request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := v.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("verify captcha: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("captcha verify status %d", resp.StatusCode)
	}

	var payload struct {
		Success    bool     `json:"success"`
		Score      *float64 `json:"score"`
		Action     string   `json:"action"`
		Hostname   string   `json:"hostname"`
		ErrorCodes []string `json:"error-codes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode captcha verify response: %w", err)
	}

	result := &VerifyResult{
		Success:    payload.Success,
		Score:      payload.Score,
		Action:     payload.Action,
		Hostname:   payload.Hostname,
		ErrorCodes: payload.ErrorCodes,
	}

	if result.Success && payload.Score != nil && *payload.Score < v.cfg.EffectiveMinScore() {
		result.Success = false
		result.ErrorCodes = append(result.ErrorCodes, "low-score")
	}

	return result, nil
}

// NewChallengeStore creates a captcha challenge store.
func NewChallengeStore(rdb *redis.Client) *ChallengeStore {
	return &ChallengeStore{
		rdb:        rdb,
		challenged: make(map[string]time.Time),
	}
}

// IsChallenged reports whether ip currently requires captcha solving.
func (s *ChallengeStore) IsChallenged(ctx context.Context, ip string) (bool, error) {
	return s.exists(ctx, s.challengeRedisKey(ip), s.memoryKey(ip), s.challenged)
}

// MarkChallenged records that ip must solve captcha.
func (s *ChallengeStore) MarkChallenged(ctx context.Context, ip string, ttl time.Duration) error {
	return s.set(ctx, s.challengeRedisKey(ip), s.memoryKey(ip), s.challenged, ttl)
}

// ClearChallenged removes the captcha challenge marker for ip.
func (s *ChallengeStore) ClearChallenged(ctx context.Context, ip string) error {
	return s.del(ctx, s.challengeRedisKey(ip), s.memoryKey(ip), s.challenged)
}

func (s *ChallengeStore) exists(ctx context.Context, redisKey, memoryKey string, memory map[string]time.Time) (bool, error) {
	if s == nil {
		return false, nil
	}
	if s.rdb != nil {
		n, err := s.rdb.Exists(ctx, redisKey).Result()
		if err == nil && n > 0 {
			return true, nil
		}
		if err != nil {
			log.WithError(err).Warn("captcha redis lookup failed; using in-memory store")
		}
		// If Redis misses after a prior fallback write, memory may still contain
		// the authoritative challenge for this process.
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	expiresAt, ok := memory[memoryKey]
	if !ok {
		return false, nil
	}
	if time.Now().After(expiresAt) {
		delete(memory, memoryKey)
		return false, nil
	}
	return true, nil
}

func (s *ChallengeStore) set(ctx context.Context, redisKey, memoryKey string, memory map[string]time.Time, ttl time.Duration) error {
	if s == nil {
		return nil
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	if s.rdb != nil {
		if err := s.rdb.Set(ctx, redisKey, "1", ttl).Err(); err != nil {
			log.WithError(err).Warn("captcha redis set failed; using in-memory store")
		}
	}
	now := time.Now()
	s.mu.Lock()
	s.pruneLocked(memory, now)
	memory[memoryKey] = now.Add(ttl)
	s.mu.Unlock()
	return nil
}

func (s *ChallengeStore) del(ctx context.Context, redisKey, memoryKey string, memory map[string]time.Time) error {
	if s == nil {
		return nil
	}
	if s.rdb != nil {
		if err := s.rdb.Del(ctx, redisKey).Err(); err != nil {
			log.WithError(err).Warn("captcha redis delete failed")
		}
	}
	s.mu.Lock()
	delete(memory, memoryKey)
	s.mu.Unlock()
	return nil
}

func (s *ChallengeStore) challengeRedisKey(ip string) string {
	return "captcha:challenge:" + ip
}

func (s *ChallengeStore) memoryKey(ip string) string {
	return ip
}

func (s *ChallengeStore) pruneLocked(memory map[string]time.Time, now time.Time) {
	for key, expiresAt := range memory {
		if now.After(expiresAt) {
			delete(memory, key)
		}
	}
	for len(memory) >= maxMemoryEntries {
		var oldestKey string
		var oldest time.Time
		for key, expiresAt := range memory {
			if oldestKey == "" || expiresAt.Before(oldest) {
				oldestKey = key
				oldest = expiresAt
			}
		}
		if oldestKey == "" {
			return
		}
		delete(memory, oldestKey)
	}
}

// ShouldApply reports whether captcha escalation is enabled for a rate-limit bucket.
func ShouldApply(cfg *config.CaptchaConfig, bucket string) bool {
	bucket = strings.ToLower(strings.TrimSpace(bucket))
	if cfg == nil || !cfg.Enabled || bucket == "" || bucket == "webhook" {
		return false
	}

	for _, allowed := range cfg.EffectiveChallengeBuckets() {
		if bucket == allowed {
			return true
		}
	}

	return false
}

// ExtremeThreshold returns the request count where rate-limit violations escalate to captcha.
func ExtremeThreshold(limit *config.RateLimit, cfg *config.CaptchaConfig) int {
	threshold := 0
	if limit != nil {
		threshold = limit.RequestsPerMinute
	}
	if threshold <= 0 {
		threshold = 60
	}
	return threshold * cfg.EffectiveExtremeMultiplier()
}
