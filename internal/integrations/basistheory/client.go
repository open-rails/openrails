// Package basistheory is the single wire-level choke point for every outbound
// HTTP call OpenRails makes to the Basis Theory API (#795). Mirrors the
// nmi/stripeapi posture: readonly enforcement lives in the transport, so a
// mutating request under mode=readonly fails locally with ErrProviderReadOnly
// before any bytes hit the network; reads pass through.
//
// Retry safety: token/network-token writes stamp BT-IDEMPOTENCY-KEY (results
// cached 24h server-side). The ephemeral PROXY does NOT support idempotency —
// charge-retry safety rests on the durable intents log (#674), NMI duplicate
// detection (430), and the orderid verify leg. Never blind-retry an ambiguous
// proxy outcome.
package basistheory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/shared/httpx"
)

const (
	DefaultBaseURL = "https://api.basistheory.com"
	// ProdWebhookKeyURL / TestWebhookKeyURL are BT's CDN-published RSA public
	// keys for webhook signature verification (no shared secret exists).
	ProdWebhookKeyURL = "https://cdn.basistheory.com/keys/webhooks.v1.key"
	TestWebhookKeyURL = "https://cdn.basistheory.com/keys/webhooks.v1-test.key"

	// DefaultTimeout bounds one BT round-trip. The proxy's destination timeout
	// is a fixed 25s on BT's side; 30s gives the proxy leg room to answer.
	DefaultTimeout = 30 * time.Second

	apiKeyHeader         = "BT-API-KEY"
	idempotencyKeyHeader = "BT-IDEMPOTENCY-KEY"
)

// ErrProviderReadOnly is returned for every mutating BT request when provider
// writes are blocked (mode=readonly). http.Client wraps transport errors in
// *url.Error, which unwraps, so errors.Is works on the returned error.
var ErrProviderReadOnly = errors.New("basistheory: provider writes are blocked (mode=readonly)")

// Config builds a Client. APIKey is the PRIVATE application key (permissions:
// proxy:invoke, token*, token-intent*, network-token*, account-updater:job:*).
// The public application key is checkout-page config, never used here; the
// management key is operator-only and never stored in the engine.
type Config struct {
	APIKey        string
	BaseURL       string // "" = DefaultBaseURL
	WebhookKeyURL string // "" = ProdWebhookKeyURL; tests inject a local URL
	ReadOnly      bool
	Timeout       time.Duration     // <= 0 = DefaultTimeout
	Transport     http.RoundTripper // test seam under the guard; nil = default
	// Outbound governs PROVIDER-SUPPLIED urls (the account-updater job's
	// upload_url / download_url). The zero value is the strict production
	// policy; tests pass httpx.Policy{Allow: httpx.AllowLoopback}.
	Outbound httpx.Policy
}

type Client struct {
	apiKey        string
	baseURL       string
	webhookKeyURL string
	readOnly      bool
	httpClient    *http.Client
	verifier      *WebhookVerifier
	outbound      httpx.Policy
}

func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("basistheory: api key is required")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	keyURL := strings.TrimSpace(cfg.WebhookKeyURL)
	if keyURL == "" {
		keyURL = ProdWebhookKeyURL
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{
		apiKey:        strings.TrimSpace(cfg.APIKey),
		baseURL:       baseURL,
		webhookKeyURL: keyURL,
		readOnly:      cfg.ReadOnly,
		httpClient: &http.Client{
			Timeout:   timeout,
			Transport: &guardTransport{readOnly: cfg.ReadOnly, base: cfg.Transport},
		},
		verifier: NewWebhookVerifier(keyURL),
		outbound: cfg.Outbound,
	}, nil
}

func (c *Client) ReadOnly() bool { return c != nil && c.readOnly }

// guardTransport rejects mutating methods before the network when readOnly.
type guardTransport struct {
	base     http.RoundTripper
	readOnly bool
}

func (t *guardTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.readOnly {
		switch req.Method {
		case http.MethodGet, http.MethodHead:
		default:
			return nil, ErrProviderReadOnly
		}
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// APIError is a parsed non-2xx BT API envelope (RFC-7807-ish).
type APIError struct {
	Status int
	Title  string
	Detail string
	Errors map[string][]string
	Raw    []byte
}

func (e *APIError) Error() string {
	msg := strings.TrimSpace(e.Title)
	if d := strings.TrimSpace(e.Detail); d != "" {
		if msg != "" {
			msg += ": "
		}
		msg += d
	}
	if msg == "" {
		msg = strings.TrimSpace(string(e.Raw))
	}
	return fmt.Sprintf("basistheory: api error (status %d) %s", e.Status, msg)
}

// IsNotFound reports whether err is a BT 404 (e.g. an expired token intent —
// intents live 24h and then genuinely do not exist; #651: surface it loudly).
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

// doJSON sends one JSON API request. idempotencyKey stamps BT-IDEMPOTENCY-KEY
// when non-empty (write endpoints only; the proxy path never uses doJSON).
func (c *Client) doJSON(ctx context.Context, method, path string, body any, idempotencyKey string, out any) error {
	if c == nil {
		return errors.New("basistheory: client not initialized")
	}
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("basistheory: encode request: %w", err)
		}
		reqBody = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set(apiKeyHeader, c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set(idempotencyKeyHeader, idempotencyKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("basistheory: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{Status: resp.StatusCode, Raw: raw}
		var envelope struct {
			Title  string              `json:"title"`
			Detail string              `json:"detail"`
			Errors map[string][]string `json:"errors"`
		}
		if json.Unmarshal(raw, &envelope) == nil {
			apiErr.Title, apiErr.Detail, apiErr.Errors = envelope.Title, envelope.Detail, envelope.Errors
		}
		return apiErr
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("basistheory: decode response: %w", err)
		}
	}
	return nil
}

// VerifyWebhook verifies a BT webhook delivery (BT-SIGNATURE /
// BT-SIGNATURE-VERSION, RSA-PSS SHA-256) against the client's configured key
// URL (cached after first fetch).
func (c *Client) VerifyWebhook(body []byte, sigB64, sigVersion string) error {
	if c == nil || c.verifier == nil {
		return errors.New("basistheory: webhook verifier not initialized")
	}
	return c.verifier.Verify(body, sigB64, sigVersion)
}
