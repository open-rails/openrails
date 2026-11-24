package basistheory

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Webhook signature verification: RSA SHA-256 with PSS padding over the raw
// body, headers BT-SIGNATURE (base64) + BT-SIGNATURE-VERSION, public key
// published on BT's CDN (prod + test keys) — there is no shared secret. The
// key URL is injectable so tests verify against a locally generated keypair.

const (
	SignatureHeader        = "BT-SIGNATURE"
	SignatureVersionHeader = "BT-SIGNATURE-VERSION"
	// SupportedSignatureVersion matches the CDN key generation (webhooks.v1*).
	SupportedSignatureVersion = "v1"
)

var (
	ErrWebhookSignatureMissing = errors.New("basistheory: webhook signature missing")
	ErrWebhookSignatureInvalid = errors.New("basistheory: webhook signature invalid")
	ErrWebhookVersionUnknown   = errors.New("basistheory: unsupported webhook signature version")
)

// WebhookVerifier verifies deliveries against one published public key,
// fetched once and cached. A signature the key rejects refetches it, so a BT
// key rotation self-heals without a restart: one fetch at a time, at most
// every RefetchEvery, and the cached key stays until a new one parses.
type WebhookVerifier struct {
	KeyURL     string
	HTTPClient *http.Client // nil = 10s-timeout default
	// RefetchEvery bounds key fetches after the first; zero is 5 minutes.
	RefetchEvery time.Duration

	mu        sync.Mutex
	key       *rsa.PublicKey
	fetchedAt time.Time
	fetching  chan struct{}
}

func NewWebhookVerifier(keyURL string) *WebhookVerifier {
	return &WebhookVerifier{KeyURL: keyURL}
}

func (v *WebhookVerifier) client() *http.Client {
	if v.HTTPClient != nil {
		return v.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (v *WebhookVerifier) fetchKey(ctx context.Context) (*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.KeyURL, nil)
	if err != nil {
		return nil, fmt.Errorf("basistheory: build webhook key request: %w", err)
	}
	resp, err := v.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("basistheory: fetch webhook key: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("basistheory: fetch webhook key: status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("basistheory: read webhook key: %w", err)
	}
	return ParsePublicKey(raw)
}

// ParsePublicKey parses a PEM (PKIX or PKCS1) RSA public key.
func ParsePublicKey(raw []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("basistheory: webhook key is not PEM")
	}
	if pub, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		rsaPub, ok := pub.(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("basistheory: webhook key is not RSA")
		}
		return rsaPub, nil
	}
	if rsaPub, err := x509.ParsePKCS1PublicKey(block.Bytes); err == nil {
		return rsaPub, nil
	}
	return nil, errors.New("basistheory: webhook key is not a parseable RSA public key")
}

// Verify checks sigB64 (BT-SIGNATURE) over body under sigVersion
// (BT-SIGNATURE-VERSION). On signature mismatch the key is refetched ONCE
// (rotation self-heal) before failing.
func (v *WebhookVerifier) Verify(ctx context.Context, body []byte, sigB64, sigVersion string) error {
	if v == nil || strings.TrimSpace(v.KeyURL) == "" {
		return errors.New("basistheory: webhook verifier not configured")
	}
	sigB64 = strings.TrimSpace(sigB64)
	if sigB64 == "" {
		return ErrWebhookSignatureMissing
	}
	if version := strings.TrimSpace(sigVersion); version != "" && !strings.EqualFold(version, SupportedSignatureVersion) {
		return fmt.Errorf("%w: %q", ErrWebhookVersionUnknown, version)
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("%w: signature is not base64", ErrWebhookSignatureInvalid)
	}
	digest := sha256.Sum256(body)

	v.mu.Lock()
	key := v.key
	v.mu.Unlock()
	if key != nil && verifyPSS(key, digest[:], sig) == nil {
		return nil
	}
	fresh, err := v.refresh(ctx, key)
	switch {
	case err != nil && key == nil:
		return err
	case err != nil, fresh == key, verifyPSS(fresh, digest[:], sig) != nil:
		return ErrWebhookSignatureInvalid
	}
	return nil
}

// firstFetchRetry spaces fetches while no key is cached.
const firstFetchRetry = 10 * time.Second

// refresh is the key after stale was rejected: one another caller already
// fetched, or a new fetch when none ran within RefetchEvery. Concurrent
// callers wait for the one fetch; a fetch that fails or does not parse keeps
// the cached key.
func (v *WebhookVerifier) refresh(ctx context.Context, stale *rsa.PublicKey) (*rsa.PublicKey, error) {
	every := v.RefetchEvery
	if every <= 0 {
		every = 5 * time.Minute
	}
	for {
		v.mu.Lock()
		key, fetching := v.key, v.fetching
		switch {
		case key != stale:
			v.mu.Unlock()
			return key, nil
		case fetching != nil:
			v.mu.Unlock()
			select {
			case <-fetching:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case key != nil && time.Since(v.fetchedAt) < every:
			v.mu.Unlock()
			return key, nil
		case key == nil && time.Since(v.fetchedAt) < firstFetchRetry:
			v.mu.Unlock()
			return nil, errors.New("basistheory: webhook key unavailable; it is fetched again shortly")
		}
		done := make(chan struct{})
		v.fetching, v.fetchedAt = done, time.Now()
		v.mu.Unlock()
		// The fetch serves every waiting caller, so no one caller cancels it.
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		fresh, err := v.fetchKey(fctx)
		cancel()
		v.mu.Lock()
		if err == nil {
			v.key = fresh
		}
		key, v.fetching = v.key, nil
		v.mu.Unlock()
		close(done)
		if err != nil && key == nil {
			return nil, err
		}
		return key, nil
	}
}

func verifyPSS(key *rsa.PublicKey, digest, sig []byte) error {
	return rsa.VerifyPSS(key, crypto.SHA256, digest, sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthAuto, Hash: crypto.SHA256})
}

// Event type strings OpenRails consumes (exact, doc-verified).
const (
	EventTokenCreated         = "token.created"
	EventTokenUpdated         = "token.updated"
	EventTokenExpired         = "token.expired"
	EventTokenDeleted         = "token.deleted"
	EventTokenPropertyExpired = "token.property.expired"
	EventTokenIntentConverted = "token-intent.converted"
	EventNetworkTokenCreated  = "network-token.created"
	EventNetworkTokenUpdated  = "network-token.updated"
	EventNetworkTokenDeleted  = "network-token.deleted"
)

// Event is the BT webhook envelope. Data is kept raw; per-event bodies decode
// what they consume and tolerate extra fields.
type Event struct {
	ID          string          `json:"id"`
	TenantID    string          `json:"tenant_id"`
	Type        string          `json:"type"`
	DeliveredAt *time.Time      `json:"delivered_at"`
	Data        json.RawMessage `json:"data"`
}

// TokenEventData covers token.* events (deleted carries id only) and
// token.property.expired (property_name).
type TokenEventData struct {
	Token struct {
		ID           string `json:"id"`
		Type         string `json:"type"`
		PropertyName string `json:"property_name"`
	} `json:"token"`
}

// TokenIntentConvertedData carries both sides of a conversion.
type TokenIntentConvertedData struct {
	TokenIntent struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	} `json:"token_intent"`
	Token struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	} `json:"token"`
}

// NetworkTokenEventData covers network-token.* events.
type NetworkTokenEventData struct {
	NetworkToken struct {
		ID            string `json:"id"`
		TenantID      string `json:"tenant_id"`
		Status        string `json:"status"`
		TokenID       string `json:"token_id"`
		TokenIntentID string `json:"token_intent_id"`
	} `json:"network_token"`
}
