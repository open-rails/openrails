package oidc

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// JWKSCache fetches and caches RSA public keys from an OIDC issuer.
type JWKSCache struct {
	mu          sync.RWMutex
	issuer      string
	jwksURL     string
	keys        map[string]*rsa.PublicKey // kid -> key
	nextRefresh time.Time
	ttl         time.Duration
	client      *http.Client
}

func NewJWKSCache(issuer string) *JWKSCache {
	return &JWKSCache{
		issuer: issuer,
		keys:   make(map[string]*rsa.PublicKey),
		ttl:    15 * time.Minute,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// GetKey returns the RSA public key for a given kid, refreshing JWKS as needed.
func (c *JWKSCache) GetKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	if kid == "" {
		return nil, errors.New("missing kid in JWT header")
	}

	c.mu.RLock()
	key := c.keys[kid]
	refreshAt := c.nextRefresh
	c.mu.RUnlock()
	if key != nil && time.Now().Before(refreshAt) {
		return key, nil
	}

	// Refresh JWKS (or discover -> refresh) under write lock
	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check after acquiring lock
	if k := c.keys[kid]; k != nil && time.Now().Before(c.nextRefresh) {
		return k, nil
	}

	// Ensure we have jwksURL by discovery if necessary
	if c.jwksURL == "" {
		if err := c.discover(ctx); err != nil {
			return nil, err
		}
	}

	if err := c.fetchJWKS(ctx); err != nil {
		return nil, err
	}

	if k := c.keys[kid]; k != nil {
		return k, nil
	}
	return nil, fmt.Errorf("kid %q not found in JWKS", kid)
}

func (c *JWKSCache) discover(ctx context.Context) error {
	wellKnown := strings.TrimRight(c.issuer, "/") + "/.well-known/openid-configuration"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, wellKnown, nil)
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("oidc discovery failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oidc discovery http %d", resp.StatusCode)
	}
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("decode discovery: %w", err)
	}
	if doc.JWKSURI == "" {
		return errors.New("jwks_uri not present in discovery doc")
	}
	c.jwksURL = doc.JWKSURI
	return nil
}

func (c *JWKSCache) fetchJWKS(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.jwksURL, nil)
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks http %d", resp.StatusCode)
	}
	var jwks struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
			Alg string `json:"alg"`
			Use string `json:"use"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return fmt.Errorf("decode jwks: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey, len(jwks.Keys))
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" || k.N == "" || k.E == "" || k.Kid == "" {
			continue
		}
		nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		n := new(big.Int).SetBytes(nBytes)
		var eInt int
		for _, b := range eBytes {
			eInt = eInt<<8 + int(b)
		}
		keys[k.Kid] = &rsa.PublicKey{N: n, E: eInt}
	}
	if len(keys) == 0 {
		return errors.New("no RSA keys in JWKS")
	}
	c.keys = keys
	c.nextRefresh = time.Now().Add(c.ttl)
	return nil
}
