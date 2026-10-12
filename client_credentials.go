package openrails

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
)

// ClientCredentialsConfig obtains merchant access tokens from a trusted OAuth
// issuer using the client_credentials grant and client_secret_basic authentication.
// The issuer and its application's merchant role must already be registered with
// OpenRails. This does not authorize customer assertions.
type ClientCredentialsConfig struct {
	TokenURL     string
	ClientID     string
	ClientSecret string
	// Resource is the OpenRails resource identifier (RFC 8707).
	Resource string
	// Scopes defaults to openrails:merchant.
	Scopes []string
	// HTTPClient supplies TLS and transport configuration. Nil uses a 30-second
	// client. Each acquisition also obeys its caller's context.
	HTTPClient *http.Client
}

// NewClientCredentialsTokenProvider returns a concurrent-safe RemoteCredential.Token
// callback. It obtains a token on demand, caches it until shortly before expiry,
// and obtains a fresh client-credentials token when needed. It performs no I/O
// during construction. Tokens without expires_in are used once, never cached.
func NewClientCredentialsTokenProvider(cfg ClientCredentialsConfig) (func(context.Context) (string, error), error) {
	endpoint, err := url.Parse(cfg.TokenURL)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") {
		return nil, fmt.Errorf("openrails: a valid OAuth token URL is required")
	}
	if endpoint.Scheme != "https" && endpoint.Hostname() != "localhost" && endpoint.Hostname() != "127.0.0.1" && endpoint.Hostname() != "::1" {
		return nil, fmt.Errorf("openrails: OAuth token URL requires HTTPS except on loopback")
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, fmt.Errorf("openrails: OAuth client ID and secret are required")
	}
	scopes := strings.Join(cfg.Scopes, " ")
	if scopes == "" {
		scopes = billing.ScopeMerchant
	}
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {scopes}}
	if cfg.Resource != "" {
		form.Set("resource", cfg.Resource)
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	// Token requests must never redirect a client secret to another endpoint.
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client = &copyClient
	gate := make(chan struct{}, 1)
	var token string
	var renewAt time.Time
	return func(ctx context.Context) (string, error) {
		select {
		case gate <- struct{}{}:
			defer func() { <-gate }()
		case <-ctx.Done():
			return "", ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if token != "" && time.Now().Before(renewAt) {
			return token, nil
		}
		started := time.Now()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenURL, strings.NewReader(form.Encode()))
		if err != nil {
			return "", fmt.Errorf("openrails: build OAuth token request: %w", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		req.SetBasicAuth(url.QueryEscape(cfg.ClientID), url.QueryEscape(cfg.ClientSecret))
		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("openrails: obtain OAuth token: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			// Do not include issuer response bodies: they can echo secrets.
			return "", fmt.Errorf("openrails: OAuth token endpoint answered HTTP %d", resp.StatusCode)
		}
		var result struct {
			AccessToken string `json:"access_token"`
			TokenType   string `json:"token_type"`
			ExpiresIn   int64  `json:"expires_in"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
			return "", fmt.Errorf("openrails: decode OAuth token response: %w", err)
		}
		if strings.TrimSpace(result.AccessToken) == "" || !strings.EqualFold(result.TokenType, "Bearer") {
			return "", fmt.Errorf("openrails: OAuth endpoint returned no Bearer access token")
		}
		if result.ExpiresIn < 0 || result.ExpiresIn > int64((time.Duration(1<<63-1))/time.Second) {
			return "", fmt.Errorf("openrails: OAuth endpoint returned an invalid token lifetime")
		}
		token = result.AccessToken
		lifetime := time.Duration(result.ExpiresIn) * time.Second
		margin := min(lifetime/10, 30*time.Second)
		renewAt = started.Add(lifetime - margin)
		return token, nil
	}, nil
}
