// Package captcha verifies provider-neutral siteverify tokens and tracks captcha challenges.
package captcha

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/abusestate"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/shared/httpx"
)

const (
	// TokenHeader is the request header clients use to submit a captcha response token.
	TokenHeader = "X-Captcha-Token"
)

// CardAttackModeSubject is the challenge subject set while merchantID is under
// a card-testing attack: every request to its captcha buckets must solve a
// captcha. It is apart from per-user/per-IP subjects (one solve never clears
// it) and per merchant (one merchant's declines never challenge another's).
func CardAttackModeSubject(merchantID uuid.UUID) string {
	return "__card_attack_mode__:" + merchantID.String()
}

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
	// verifyURLOverride points in-package tests at an httptest server; the URL
	// is otherwise hardcoded per provider.
	verifyURLOverride string
}

// ChallengeStore tracks challenged subjects in the process's abuse state. One
// per process. Subjects are rate-limit identities such as "ip:203.0.113.1" or
// "user:abc".
type ChallengeStore struct {
	state *abusestate.Store
}

// NewVerifier returns a provider-neutral captcha verifier when captcha is enabled.
func NewVerifier(cfg *config.CaptchaConfig, client *http.Client) Verifier {
	if !config.CaptchaEnabled(cfg) {
		return nil
	}

	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}

	return &siteVerifyVerifier{cfg: cfg, client: client}
}

func (v *siteVerifyVerifier) verifyURL() string {
	if v.verifyURLOverride != "" {
		return v.verifyURLOverride
	}
	return config.CaptchaVerifyURL(v.cfg)
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

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, v.verifyURL(), strings.NewReader(form.Encode()))
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
	// Cap the upstream body even though siteverify is normally a trusted provider:
	// defence-in-depth against a compromised/MITM'd endpoint exhausting memory.
	if err := httpx.DecodeJSONLimited(resp.Body, 0, &payload); err != nil {
		return nil, fmt.Errorf("decode captcha verify response: %w", err)
	}

	result := &VerifyResult{
		Success:    payload.Success,
		Score:      payload.Score,
		Action:     payload.Action,
		Hostname:   payload.Hostname,
		ErrorCodes: payload.ErrorCodes,
	}

	if result.Success && payload.Score != nil && *payload.Score < config.CaptchaMinScore {
		result.Success = false
		result.ErrorCodes = append(result.ErrorCodes, "low-score")
	}
	if result.Success && config.CaptchaProvider(v.cfg) == config.CaptchaProviderRecaptchaV3 && strings.TrimSpace(payload.Action) != config.CaptchaAction {
		result.Success = false
		result.ErrorCodes = append(result.ErrorCodes, "action-mismatch")
	}

	return result, nil
}

// NewChallengeStore creates a captcha challenge store over state.
func NewChallengeStore(state *abusestate.Store) *ChallengeStore {
	return &ChallengeStore{state: state}
}

// IsChallenged reports whether subject currently requires captcha solving.
func (s *ChallengeStore) IsChallenged(ctx context.Context, subject string) bool {
	return s != nil && s.state.Held(ctx, challengeKey(subject)) > 0
}

// MarkChallenged records that subject must solve captcha for ttl (15 minutes
// when not positive).
func (s *ChallengeStore) MarkChallenged(ctx context.Context, subject string, ttl time.Duration) {
	if s == nil {
		return
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	s.state.Hold(ctx, challengeKey(subject), ttl)
}

// ClearChallenged removes the captcha challenge marker for subject.
func (s *ChallengeStore) ClearChallenged(ctx context.Context, subject string) {
	if s == nil {
		return
	}
	if err := s.state.Release(ctx, challengeKey(subject)); err != nil {
		log.WithError(err).WithField("subject", subject).Warn("captcha: the challenge is cleared in this process only")
	}
}

func challengeKey(subject string) string {
	return "captcha:challenge:" + subject
}

// ShouldApply reports whether captcha escalation is enabled for a rate-limit bucket.
func ShouldApply(cfg *config.CaptchaConfig, bucket string) bool {
	bucket = strings.ToLower(strings.TrimSpace(bucket))
	if !config.CaptchaEnabled(cfg) || bucket == "" || bucket == "webhook" {
		return false
	}

	for _, allowed := range config.CaptchaChallengeBuckets() {
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
	return threshold * config.CaptchaExtremeMultiplier
}
