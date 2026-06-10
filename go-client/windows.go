package openrails

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Prepaid credit windows (#335). A window is one bulk hold the caller admits
// requests against locally: open reserves ~N requests' worth of a payer's
// funds (a $0 payer is denied on open — no optimistic approval anywhere);
// SettleWindow flushes CROSS-PAYER batches of actuals (idempotent per
// request_id, so re-sent flushes never double-charge); RefillWindow extends
// funds + TTL off the hot path; CloseWindow releases the unsettled remainder.
// An abandoned window's remainder auto-releases at expiry server-side.

// OpenWindowRequest is the body for POST /v1/service/credits/windows.
type OpenWindowRequest struct {
	PayerTenantID string `json:"tenant_subject_id"`
	Actor         string `json:"actor"`
	CreditType    string `json:"credit_type,omitempty"`
	AmountMicros  int64  `json:"amount"`
	TTLSeconds    int64  `json:"ttl_seconds,omitempty"`
}

// CreditWindow is the window snapshot returned by open/refill/close.
type CreditWindow struct {
	WindowID      uuid.UUID `json:"window_id"`
	PayerTenantID uuid.UUID `json:"tenant_subject_id"`
	HeldMicros    int64     `json:"held_amount"`
	SettledMicros int64     `json:"settled_amount"`
	Status        string    `json:"status"` // open | closed | expired
	ExpiresAt     time.Time `json:"expires_at"`
}

// OpenWindow opens a prepaid window: a REAL hold — the funds leave the payer's
// available balance now. ErrInsufficientCredits on a payer who can't cover it.
func (c *Client) OpenWindow(ctx context.Context, req OpenWindowRequest) (*CreditWindow, error) {
	if c == nil {
		return nil, fmt.Errorf("openrails: nil client")
	}
	if req.CreditType == "" {
		req.CreditType = c.creditType
	}
	var out CreditWindow
	if err := c.do(ctx, http.MethodPost, "/v1/service/credits/windows", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WindowSettleUsage carries the analytics attribution recorded with a settled
// item (no second debit) — the window analogue of CaptureUsage.
type WindowSettleUsage struct {
	EventType string         `json:"event_type"`
	Resource  string         `json:"resource,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// WindowSettleItem is one settled actual. Items in one SettleWindow call may
// span windows AND payers (the cross-payer flush). RequestID is the per-item
// idempotency key.
type WindowSettleItem struct {
	WindowID     uuid.UUID          `json:"window_id"`
	RequestID    string             `json:"request_id"`
	AmountMicros int64              `json:"amount"`
	Actor        string             `json:"actor,omitempty"`
	Usage        *WindowSettleUsage `json:"usage,omitempty"`
}

// WindowSettleResult is one per-item settle outcome. OK with Replayed=true is
// an idempotent replay (already settled, not charged again). Error is one of
// window_not_found | window_not_open | window_exceeded | invalid_item |
// insufficient_credits | internal_error.
type WindowSettleResult struct {
	WindowID      uuid.UUID  `json:"window_id"`
	RequestID     string     `json:"request_id"`
	OK            bool       `json:"ok"`
	Replayed      bool       `json:"replayed,omitempty"`
	Error         string     `json:"error,omitempty"`
	TransactionID *uuid.UUID `json:"transaction_id,omitempty"`
}

// SettleWindow flushes a cross-payer batch of settled actuals. The call
// succeeds with per-item results — one item's failure never fails the flush —
// and is safe to re-send wholesale (per-item idempotency on request_id).
func (c *Client) SettleWindow(ctx context.Context, items []WindowSettleItem) ([]WindowSettleResult, error) {
	if c == nil {
		return nil, fmt.Errorf("openrails: nil client")
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("openrails: settle requires items")
	}
	var out struct {
		Items []WindowSettleResult `json:"items"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/service/credits/settle", map[string]any{"items": items}, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// RefillWindow extends an open window: amountMicros > 0 reserves more funds
// (ErrInsufficientCredits applies exactly like open), ttlSeconds > 0 pushes
// expires_at out. At least one must be positive.
func (c *Client) RefillWindow(ctx context.Context, windowID uuid.UUID, amountMicros, ttlSeconds int64) (*CreditWindow, error) {
	if c == nil {
		return nil, fmt.Errorf("openrails: nil client")
	}
	if windowID == uuid.Nil {
		return nil, fmt.Errorf("openrails: refill requires window id")
	}
	body := map[string]any{}
	if amountMicros > 0 {
		body["amount"] = amountMicros
	}
	if ttlSeconds > 0 {
		body["ttl_seconds"] = ttlSeconds
	}
	var out CreditWindow
	if err := c.do(ctx, http.MethodPost, "/v1/service/credits/windows/"+windowID.String()+"/refill", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CloseWindow releases the window's unsettled remainder back to the payer's
// available balance. Idempotent: re-closing returns the closed window.
func (c *Client) CloseWindow(ctx context.Context, windowID uuid.UUID) (*CreditWindow, error) {
	if c == nil {
		return nil, fmt.Errorf("openrails: nil client")
	}
	if windowID == uuid.Nil {
		return nil, fmt.Errorf("openrails: close requires window id")
	}
	var out CreditWindow
	if err := c.do(ctx, http.MethodPost, "/v1/service/credits/windows/"+windowID.String()+"/close", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AdmitBatchVerdict is one per-item verdict from POST /v1/service/admit/batch.
// Status is the HTTP-equivalent status the single /admit route would have
// returned for this item (200/402/403/429/4xx/5xx); Result is the full
// admission decision when one was reached.
type AdmitBatchVerdict struct {
	Status int            `json:"status"`
	Error  string         `json:"error,omitempty"`
	Result *AdmitResponse `json:"result,omitempty"`
}

// Allowed reports whether this item was admitted.
func (v AdmitBatchVerdict) Allowed() bool {
	return v.Status == http.StatusOK && v.Result != nil && v.Result.Allowed
}

// AdmitBatch performs one cross-payer batch admission (#335): N admit items
// (mixed payers) in ONE hop with per-item verdicts — the cold-payer companion
// to windows. Per-item isolation: one item's deny or error never fails the
// batch; the returned slice is positionally aligned with items. Idempotent per
// item on RequestID like the single admit.
func (c *Client) AdmitBatch(ctx context.Context, items []AdmitRequest) ([]AdmitBatchVerdict, error) {
	if c == nil {
		return nil, fmt.Errorf("openrails: nil client")
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("openrails: admit batch requires items")
	}
	for i := range items {
		if strings.TrimSpace(items[i].CreditType) == "" {
			items[i].CreditType = c.creditType
		}
	}
	var out struct {
		Items []AdmitBatchVerdict `json:"items"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/service/admit/batch", map[string]any{"items": items}, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}
