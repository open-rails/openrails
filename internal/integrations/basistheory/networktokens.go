package basistheory

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Network tokens (VTS/MDES via BT's TR-TSP). Provisioning is idempotent per
// PAN per tenant (_extras.deduplicated). NT auto-update survives card reissue
// ONLY on the network-token object — the underlying card token's FPAN is never
// rewritten (FPAN refresh is the batch Account Updater's job).

// NT statuses.
const (
	NetworkTokenActive    = "active"
	NetworkTokenInactive  = "inactive"
	NetworkTokenSuspended = "suspended"
	NetworkTokenDeleted   = "deleted" // local vocabulary; BT deletes return 204
)

// Non-retryable NT provisioning error codes (sandbox-documented vocabulary).
const (
	NTCardNotEligible      = "CARD_NOT_ELIGIBLE"
	NTCardNotAllowed       = "CARD_NOT_ALLOWED"
	NTCardDeclined         = "CARD_DECLINED"
	NTIssuerDeclined       = "ISSUER_DECLINED"
	NTProvisionDataExpired = "PROVISION_DATA_EXPIRED"
	NTCardVerificationFail = "CARD_VERIFICATION_FAILED"
)

type NetworkTokenRequest struct {
	// Exactly one source: TokenID or TokenIntentID (raw-PAN `data` source is
	// PCI-DSS-only and deliberately not modeled).
	TokenID       string
	TokenIntentID string
	// CardholderName populates cardholder_info.name (required inside the block
	// when the block is sent; improves issuer approval odds).
	CardholderName string
	// IdempotencyKey derives from the durable intent row; PAN-level dedup is
	// server-side regardless.
	IdempotencyKey string
}

type NetworkTokenCard struct {
	Bin             string `json:"bin"`
	Last4           string `json:"last4"`
	Brand           string `json:"brand"`
	Funding         string `json:"funding"`
	ExpirationMonth int    `json:"expiration_month"`
	ExpirationYear  int    `json:"expiration_year"`
}

type NetworkToken struct {
	ID            string            `json:"id"`
	TenantID      string            `json:"tenant_id"`
	Status        string            `json:"status"`
	PAR           string            `json:"par"`
	NetworkToken  *NetworkTokenCard `json:"network_token"`
	Card          *NetworkTokenCard `json:"card"` // source-card snapshot
	TokenID       string            `json:"token_id"`
	TokenIntentID string            `json:"token_intent_id"`
	CreatedAt     time.Time         `json:"created_at"`
	Extras        struct {
		Deduplicated bool `json:"deduplicated"`
	} `json:"_extras"`
}

// Cryptogram is short-lived and single-use: regenerate on every retry. CIT
// charges require it; MITs ride the network transaction identifier instead.
type Cryptogram struct {
	Cryptogram string `json:"cryptogram"`
	ECI        string `json:"eci"`
}

func (c *Client) CreateNetworkToken(ctx context.Context, req NetworkTokenRequest) (*NetworkToken, error) {
	tokenID := strings.TrimSpace(req.TokenID)
	intentID := strings.TrimSpace(req.TokenIntentID)
	if (tokenID == "") == (intentID == "") {
		return nil, errors.New("basistheory: network token requires exactly one of token_id or token_intent_id")
	}
	body := map[string]any{}
	if tokenID != "" {
		body["token_id"] = tokenID
	} else {
		body["token_intent_id"] = intentID
	}
	if name := strings.TrimSpace(req.CardholderName); name != "" {
		body["cardholder_info"] = map[string]any{"name": name}
	}
	var out NetworkToken
	if err := c.doJSON(ctx, http.MethodPost, "/network-tokens", body, req.IdempotencyKey, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetNetworkToken(ctx context.Context, id string) (*NetworkToken, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("basistheory: network token id is required")
	}
	var out NetworkToken
	if err := c.doJSON(ctx, http.MethodGet, "/network-tokens/"+url.PathEscape(id), nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CreateCryptogram(ctx context.Context, networkTokenID string) (*Cryptogram, error) {
	networkTokenID = strings.TrimSpace(networkTokenID)
	if networkTokenID == "" {
		return nil, errors.New("basistheory: network token id is required")
	}
	var out Cryptogram
	// No idempotency key: cryptograms are single-use by design.
	if err := c.doJSON(ctx, http.MethodPost, "/network-tokens/"+url.PathEscape(networkTokenID)+"/cryptogram", nil, "", &out); err != nil {
		return nil, err
	}
	if strings.TrimSpace(out.Cryptogram) == "" {
		return nil, errors.New("basistheory: cryptogram response missing cryptogram value")
	}
	return &out, nil
}

func (c *Client) SuspendNetworkToken(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("basistheory: network token id is required")
	}
	return c.doJSON(ctx, http.MethodPut, "/network-tokens/"+url.PathEscape(id)+"/suspend", nil, "", nil)
}

func (c *Client) ResumeNetworkToken(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("basistheory: network token id is required")
	}
	return c.doJSON(ctx, http.MethodPut, "/network-tokens/"+url.PathEscape(id)+"/resume", nil, "", nil)
}

func (c *Client) DeleteNetworkToken(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("basistheory: network token id is required")
	}
	return c.doJSON(ctx, http.MethodDelete, "/network-tokens/"+url.PathEscape(id), nil, "", nil)
}
