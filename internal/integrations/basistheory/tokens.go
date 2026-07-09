package basistheory

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CardDetails is BT's card enrichment block (masked; never a PAN).
type CardDetails struct {
	Bin             string `json:"bin"`
	Last4           string `json:"last4"`
	Brand           string `json:"brand"`
	Funding         string `json:"funding"`
	ExpirationMonth int    `json:"expiration_month"`
	ExpirationYear  int    `json:"expiration_year"`
}

// TokenIntent is a short-lived (24h TTL) collection intent. The CVC lives on
// the INTENT and is NOT moved to the token on conversion — CIT-with-CVC
// charges must reference the intent (`{{ token_intent: … }}`).
type TokenIntent struct {
	ID          string       `json:"id"`
	TenantID    string       `json:"tenant_id"`
	Type        string       `json:"type"`
	Fingerprint string       `json:"fingerprint"`
	Card        *CardDetails `json:"card"`
	ExpiresAt   time.Time    `json:"expires_at"`
	CreatedAt   time.Time    `json:"created_at"`
}

// CardToken is a durable stored card token (container /pci/high/).
type CardToken struct {
	ID          string       `json:"id"`
	TenantID    string       `json:"tenant_id"`
	Type        string       `json:"type"`
	Fingerprint string       `json:"fingerprint"`
	Card        *CardDetails `json:"card"`
	CreatedAt   time.Time    `json:"created_at"`
	ExpiresAt   *time.Time   `json:"expires_at"`
}

// CardData is raw card input for SERVER-SIDE token-intent creation. Production
// checkout never sends a PAN through OpenRails (browser -> BT Elements, SAQ A);
// this exists for the integration-test leg only (BT test tenants accept
// arbitrary PANs).
type CardData struct {
	Number          string `json:"number"`
	ExpirationMonth int    `json:"expiration_month"`
	ExpirationYear  int    `json:"expiration_year"`
	CVC             string `json:"cvc,omitempty"`
}

// CreateCardTokenIntent creates a card token intent server-side (test leg; see
// CardData). POST /token-intents, permission token-intent:create.
func (c *Client) CreateCardTokenIntent(ctx context.Context, data CardData, idempotencyKey string) (*TokenIntent, error) {
	if strings.TrimSpace(data.Number) == "" {
		return nil, errors.New("basistheory: card number is required")
	}
	var out TokenIntent
	req := map[string]any{"type": "card", "data": data}
	if err := c.doJSON(ctx, http.MethodPost, "/token-intents", req, idempotencyKey, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetTokenIntent reads a token intent. A >24h-old (expired) intent is a loud
// 404 (IsNotFound), never a fabricated fallback.
func (c *Client) GetTokenIntent(ctx context.Context, id string) (*TokenIntent, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("basistheory: token intent id is required")
	}
	var out TokenIntent
	if err := c.doJSON(ctx, http.MethodGet, "/token-intents/"+url.PathEscape(id), nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ConvertOpts controls intent -> durable-token conversion.
type ConvertOpts struct {
	// IdempotencyKey derives from the durable intent row (#674) so an executor
	// replay converts once.
	IdempotencyKey string
	// Deduplicate returns the EXISTING token on fingerprint match instead of
	// minting a duplicate (deduplicateToken: true).
	Deduplicate bool
	Metadata    map[string]string
}

// ConvertTokenIntent converts a token intent into a durable card token
// (POST /tokens with token_intent_id). The CVC is NOT carried over (BT
// contract); conversion must happen in-request post-approval, with the
// token-intent.converted webhook as crash-window reconciliation.
func (c *Client) ConvertTokenIntent(ctx context.Context, intentID string, opts ConvertOpts) (*CardToken, error) {
	intentID = strings.TrimSpace(intentID)
	if intentID == "" {
		return nil, errors.New("basistheory: token intent id is required")
	}
	req := map[string]any{"token_intent_id": intentID}
	if opts.Deduplicate {
		req["deduplicate_token"] = true
	}
	if len(opts.Metadata) > 0 {
		req["metadata"] = opts.Metadata
	}
	var out CardToken
	if err := c.doJSON(ctx, http.MethodPost, "/tokens", req, opts.IdempotencyKey, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetToken(ctx context.Context, id string) (*CardToken, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("basistheory: token id is required")
	}
	var out CardToken
	if err := c.doJSON(ctx, http.MethodGet, "/tokens/"+url.PathEscape(id), nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) DeleteToken(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("basistheory: token id is required")
	}
	return c.doJSON(ctx, http.MethodDelete, "/tokens/"+url.PathEscape(id), nil, "", nil)
}
