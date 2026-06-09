// Package openrails is a dependency-light client for the standalone OpenRails
// service API.
//
// The client talks directly to OpenRails over its service-authenticated public
// credit routes and performs the atomic estimate -> authorize+hold ->
// capture/release flow:
//
//	POST /v1/service/credits/authorize         (atomic authorize + hold, idempotent on request_id)
//	POST /v1/service/credits/holds/:id/capture (settle the hold at the actual amount)
//	POST /v1/service/credits/holds/:id/release (release the hold, no charge)
//	GET  /v1/service/credits/balance           (balance snapshot)
//
// Auth is a single OpenRails-issued service token (Bearer) holding
// openrails:credits:spend (+ openrails:credits:write). The `tenant_subject_id`
// is the OpenRails subject being billed; the `invoker_id` is the principal in
// canonical form (service-token:<key_id> | user:<id> | <issuer>:<sub>) — see
// InvokerFor.
package openrails

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrUnreachable wraps any transport/timeout failure talking to OpenRails on the
// hot path. The caller's fail-policy (FailOpen vs FailClosed, #248) keys off this
// sentinel: a clean deny (allowed=false) is NOT an ErrUnreachable and is always
// honored regardless of policy.
var (
	ErrUnreachable         = errors.New("openrails: unreachable")
	ErrInsufficientCredits = errors.New("insufficient_credits")
	ErrCreditTypeInactive  = errors.New("credit_type_inactive")
)

// PayerTenantID is the OpenRails tenant-subject UUID a charge is billed to.
type PayerTenantID uuid.UUID

func (id PayerTenantID) UUID() uuid.UUID { return uuid.UUID(id) }
func (id PayerTenantID) String() string  { return uuid.UUID(id).String() }
func (id PayerTenantID) IsZero() bool    { return uuid.UUID(id) == uuid.Nil }

// AuthorizeRequest is the body for POST /v1/service/credits/authorize. It is the
// atomic authorize+hold: OpenRails checks the payer's balance and, if allowed,
// places a hold for EstimateCents in a single idempotent call keyed on RequestID.
type AuthorizeRequest struct {
	PayerTenantID string `json:"tenant_subject_id"`
	InvokerID     string `json:"invoker_id"`
	CreditType    string `json:"credit_type,omitempty"`
	EstimateCents int64  `json:"estimate_cents"`
	RequestID     string `json:"request_id"`
}

// AuthorizeResponse is the OpenRails decision. Allowed=false with a DenyCode is a
// definitive deny (insufficient funds, frozen org, etc.); ReservationID is the
// hold handle to later capture or release.
type AuthorizeResponse struct {
	Allowed             bool   `json:"allowed"`
	DenyCode            string `json:"deny_code"`
	AvailableCents      int64  `json:"available_cents"`
	OutstandingCents    int64  `json:"outstanding_cents"`
	RemainingTodayCents int64  `json:"remaining_today_cents"`
	ReservationID       string `json:"reservation_id"`
}

// HoldCreditsRequest reserves credits for an estimate.
type HoldCreditsRequest struct {
	PayerTenantID *PayerTenantID
	InvokerID     string
	CreditType    string
	Amount        int64
	Source        string
	SourceID      string
	ExpiresAt     time.Time
}

// CreditHold is the reservation returned by a successful hold.
type CreditHold struct {
	ID        uuid.UUID
	InvokerID string
	Amount    int64
	Source    string
	SourceID  string
	Status    string
	ExpiresAt time.Time
	Captured  *int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CaptureHoldRequest settles a hold at the actual amount.
type CaptureHoldRequest struct {
	HoldID uuid.UUID
	Amount int64
}

// WithdrawCreditsRequest debits credits directly.
type WithdrawCreditsRequest struct {
	PayerTenantID *PayerTenantID
	InvokerID     string
	CreditType    string
	Amount        int64
	Source        string
	SourceID      *uuid.UUID
}

// CreditTransaction is the ledger row returned by capture/withdraw.
type CreditTransaction struct {
	ID              uuid.UUID
	PayerTenantID   uuid.UUID
	InvokerID       string
	CreditTypeID    uuid.UUID
	Amount          int64
	BalanceAfter    *int64
	TransactionType string
	Status          string
	Authorized      *int64
	Captured        *int64
	Source          string
	SourceID        *string
	ExpiresAt       *time.Time
	Description     *string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// AdmitRequest is the body for POST /v1/service/admit — the UNIFIED admission
// verdict (#403/#404). It folds rate-limit/quota/concurrency (throughput),
// rolling money-budget windows, suspension/blocklist/endpoint gating, AND the
// credit hold into ONE call, returning ONE reservation for later
// capture/release. This is the single hot-path money+limit verdict that replaces
// the legacy tensorhub broker admit (Path B): gen-orch sizes the cost from the
// in-process tensorhub pricing, then calls this once.
//
// Tier/Model select the throughput + budget policy + endpoint gating; Amounts is
// the per-request throughput consumption (e.g. {"request":1}). EstimateCents is
// the upper-bound charge to hold. A zero EstimateCents runs the limit checks
// without placing a money hold.
type AdmitRequest struct {
	PayerTenantID string           `json:"tenant_subject_id"`
	InvokerID     string           `json:"invoker_id"`
	Tier          string           `json:"tier,omitempty"`
	Model         string           `json:"model,omitempty"`
	Amounts       map[string]int64 `json:"amounts,omitempty"`
	CreditType    string           `json:"credit_type,omitempty"`
	EstimateCents int64            `json:"estimate_cents"`
	RequestID     string           `json:"request_id"`
	Source        string           `json:"source,omitempty"`
	ExpiresAt     *int64           `json:"expires_at,omitempty"`
	// TenantThroughput is the per-TENANT fixed-window throughput policy OpenRails
	// should enforce on this invoke (#404) — tensorhub no longer rate-limits the
	// invoke path locally.
	TenantThroughput []AdmitThroughputWindow `json:"tenant_throughput,omitempty"`
}

// AdmitThroughputWindow is one tenant fixed-window throughput limit (#404):
// at most Max units of Unit per WindowSeconds (Unit defaults to "request").
type AdmitThroughputWindow struct {
	Unit          string `json:"unit,omitempty"`
	WindowSeconds int64  `json:"window_seconds"`
	Max           int64  `json:"max"`
}

// AdmitResponse is the unified verdict. Allowed=false carries a BlockedBy axis
// ("throughput" | "money" | "budget" | "blocked" | "suspended" | "unverified" |
// "endpoint") and, on a money deny, a DenyCode. ReservationID is the placed
// credit hold to capture/release; BudgetReservationID is the rolling-budget
// reservation (settled implicitly with the hold).
type AdmitResponse struct {
	Allowed             bool   `json:"allowed"`
	BlockedBy           string `json:"blocked_by"`
	BlockedUnit         string `json:"blocked_unit"`
	DenyCode            string `json:"deny_code"`
	RetryAfterSeconds   int64  `json:"retry_after_seconds"`
	ReservationID       string `json:"reservation_id"`
	BudgetReservationID string `json:"budget_reservation_id"`
}

// Admit performs the unified admission check + atomic hold (#403/#404). It is
// idempotent on req.RequestID. A transport/timeout error wraps as ErrUnreachable
// so the caller's fail-policy decides; a clean deny is (Allowed=false, nil).
func (c *Client) Admit(ctx context.Context, req AdmitRequest) (*AdmitResponse, error) {
	if c == nil {
		return nil, fmt.Errorf("openrails: nil client")
	}
	if req.CreditType == "" {
		req.CreditType = c.creditType
	}
	var out AdmitResponse
	if err := c.do(ctx, http.MethodPost, "/v1/service/admit", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CaptureRequest settles a hold at the actual (final) amount. CapturedCents may be
// less than the held estimate; OpenRails releases the difference.
//
// The wire field is "amount": OpenRails' capture handler (serviceCaptureRequest)
// binds `amount` as REQUIRED, so this MUST serialize to "amount" or every capture
// 400s. (Unit tests use a fake that echoed the old "captured_cents" name, which is
// why this contract drift wasn't caught there — the deployed-stack e2e is.)
type CaptureRequest struct {
	CapturedCents int64 `json:"amount"`

	// Usage analytics (#311/#410): when EventType is set, OpenRails also appends
	// a usage_event linked to this capture (no second debit) so the platform's
	// /budget-usage + revenue analytics are served from OpenRails. EventType is
	// the metered model (owner/endpoint); Metadata carries string grouping dims.
	EventType string         `json:"event_type,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	Source    string         `json:"source,omitempty"`
	SourceID  string         `json:"source_id,omitempty"`
}

// CaptureUsage carries the analytics dimensions recorded alongside a capture so
// OpenRails can serve per-endpoint/function/tier/user spend (#410). Nil = no
// usage event (a plain capture).
type CaptureUsage struct {
	EventType string
	Metadata  map[string]any
	Source    string
	SourceID  string
}

// BalanceResponse is the GET /v1/service/credits/balance snapshot.
type BalanceResponse struct {
	AvailableCents      int64 `json:"available_cents"`
	OutstandingCents    int64 `json:"outstanding_cents"`
	RemainingTodayCents int64 `json:"remaining_today_cents"`
}

// CreditAccount is the OpenRails service balance/policy snapshot.
type CreditAccount struct {
	PayerTenantID        string `json:"tenant_subject_id"`
	CreditType           string `json:"credit_type"`
	BillingMode          string `json:"billing_mode"`
	BalanceCents         int64  `json:"balance_cents"`
	HeldCents            int64  `json:"held_cents"`
	AvailableCents       int64  `json:"available_cents"`
	OutstandingOwedCents int64  `json:"outstanding_owed_cents"`
}

// AccountSettingsInput patches an OpenRails credit account policy.
type AccountSettingsInput struct {
	BillingMode             *string `json:"billing_mode,omitempty"`
	MaxSpendPerDayCents     *int64  `json:"max_spend_per_day_cents,omitempty"`
	MaxSpendPerMonthCents   *int64  `json:"max_spend_per_month_cents,omitempty"`
	MaxOutstandingOwedCents *int64  `json:"max_outstanding_owed_cents,omitempty"`
	LowBalanceThreshold     *int64  `json:"low_balance_threshold_cents,omitempty"`
	AutoTopupEnabled        *bool   `json:"auto_topup_enabled,omitempty"`
	AutoTopupAmountCents    *int64  `json:"auto_topup_amount_cents,omitempty"`
	AutoTopupPaymentMethod  *string `json:"auto_topup_payment_method_id,omitempty"`
	DefaultCreditExpiryDays *int    `json:"default_credit_expiry_days,omitempty"`
	HardStopOnBreach        *bool   `json:"hard_stop_on_breach,omitempty"`
	AlertThresholdPct       *int    `json:"alert_threshold_pct,omitempty"`
}

// UsageRollupRow is one grouped spend bucket from OpenRails.
type UsageRollupRow struct {
	Key         string `json:"key"`
	EventCount  int64  `json:"event_count"`
	TotalAmount int64  `json:"total_amount"`
}

// BudgetWindowInput is a caller-supplied rolling budget window sent to
// OpenRails. The host owns the policy; OpenRails owns the spend actuals.
type BudgetWindowInput struct {
	Key             string `json:"key"`
	WindowSeconds   int64  `json:"window_seconds"`
	LimitMillicents int64  `json:"limit_millicents"`
}

// BudgetWindow is one computed window from POST /v1/service/budget/check.
type BudgetWindow struct {
	Key               string `json:"key"`
	Limit             int64  `json:"limit"`
	Used              int64  `json:"used"`
	Reserved          int64  `json:"reserved"`
	Remaining         int64  `json:"remaining"`
	ResetAfterSeconds int64  `json:"reset_after_seconds"`
	Allowed           bool   `json:"allowed"`
}

// Client is a thin HTTP client for the OpenRails service routes.
type Client struct {
	baseURL    string
	creditType string
	client     *http.Client
	// tokenFn mints the per-call Bearer: a tensorhub-signed AuthKit service JWT
	// (#411, sub=service:tensorhub-runtime, aud=openrails). It is the SOLE
	// credential — there is no OAT fallback (#411 [8]); a mint failure errors the
	// call so the problem surfaces instead of being masked.
	tokenFn func(context.Context) (string, error)
}

// Config configures a Client.
type Config struct {
	// BaseURL is the OpenRails base URL (e.g. https://openrails.internal). Required.
	BaseURL string
	// CreditType is the default credit_type sent on authorize (optional).
	CreditType string
	// Timeout bounds EVERY hot-path call (authorize/capture/release/balance). A
	// short value is the point — a slow OpenRails must not stall the request hot
	// path; on timeout the fail-policy decides. Defaults to 2s when <= 0.
	Timeout time.Duration
	// HTTPClient lets callers/tests inject a transport. When nil a client with
	// Timeout is created.
	HTTPClient *http.Client
	// TokenProvider supplies the per-call Bearer — a tensorhub-minted AuthKit
	// service JWT (#411). REQUIRED: it is the only credential (OATs were retired
	// for first-party OpenRails calls in #411 [8]).
	TokenProvider func(context.Context) (string, error)
}

// New builds a Client from cfg. Returns an error when BaseURL or TokenProvider is
// missing — the credit path must never run against a half-configured backend.
func New(cfg Config) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("openrails: base url required")
	}
	if cfg.TokenProvider == nil {
		return nil, fmt.Errorf("openrails: token provider required (service-JWT auth, #411)")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: timeout}
	}
	return &Client{
		baseURL:    base,
		creditType: strings.TrimSpace(cfg.CreditType),
		client:     hc,
		tokenFn:    cfg.TokenProvider,
	}, nil
}

// bearer mints the credential for the next call: a tensorhub-signed AuthKit
// service JWT (#411). It is the SOLE credential — no OAT fallback ([8]); a mint
// failure or empty token errors the call so the issue surfaces.
func (c *Client) bearer(ctx context.Context) (string, error) {
	if c.tokenFn == nil {
		return "", fmt.Errorf("openrails: no service-JWT token provider configured")
	}
	tok, err := c.tokenFn(ctx)
	if err != nil {
		return "", fmt.Errorf("openrails: mint service JWT: %w", err)
	}
	if strings.TrimSpace(tok) == "" {
		return "", fmt.Errorf("openrails: service-JWT token provider returned empty token")
	}
	return strings.TrimSpace(tok), nil
}

// Authorize performs the atomic authorize+hold. It is idempotent on req.RequestID
// (a retry returns the same reservation). A transport/timeout error is wrapped as
// ErrUnreachable so the caller's fail-policy can decide; a clean OpenRails deny
// comes back as (resp.Allowed == false, nil error).
func (c *Client) Authorize(ctx context.Context, req AuthorizeRequest) (*AuthorizeResponse, error) {
	if c == nil {
		return nil, fmt.Errorf("openrails: nil client")
	}
	if req.CreditType == "" {
		req.CreditType = c.creditType
	}
	var out AuthorizeResponse
	if err := c.do(ctx, http.MethodPost, "/v1/service/credits/authorize", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// HoldCredits reserves credits through the legacy hold route used by the
// Tensorhub API surface.
func (c *Client) HoldCredits(ctx context.Context, req HoldCreditsRequest) (*CreditHold, error) {
	var out CreditHold
	err := c.do(ctx, http.MethodPost, "/v1/service/credits/hold", map[string]any{
		"tenant_subject_id": payerTenantIDString(req.PayerTenantID),
		"invoker_id":        req.InvokerID,
		"credit_type":       req.CreditType,
		"amount":            req.Amount,
		"source":            req.Source,
		"source_id":         req.SourceID,
		"expires_at":        req.ExpiresAt.Unix(),
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Capture settles the hold reservationID at capturedCents. Idempotent on the
// reservation. A nil error means OpenRails accepted the capture.
func (c *Client) Capture(ctx context.Context, reservationID string, capturedCents int64, usage *CaptureUsage) error {
	if c == nil {
		return fmt.Errorf("openrails: nil client")
	}
	if strings.TrimSpace(reservationID) == "" {
		return fmt.Errorf("openrails: capture requires reservation_id")
	}
	req := CaptureRequest{CapturedCents: capturedCents}
	if usage != nil && strings.TrimSpace(usage.EventType) != "" {
		req.EventType = usage.EventType
		req.Metadata = usage.Metadata
		req.Source = usage.Source
		req.SourceID = usage.SourceID
	}
	path := "/v1/service/credits/holds/" + reservationID + "/capture"
	return c.do(ctx, http.MethodPost, path, req, nil)
}

// CaptureHold settles a hold and returns the OpenRails ledger transaction.
func (c *Client) CaptureHold(ctx context.Context, req CaptureHoldRequest) (*CreditTransaction, error) {
	var out CreditTransaction
	path := "/v1/service/credits/holds/" + req.HoldID.String() + "/capture"
	if err := c.do(ctx, http.MethodPost, path, map[string]any{"amount": req.Amount}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Release frees the hold reservationID without charging. Idempotent on the
// reservation. Used when the work fails after a successful authorize+hold.
func (c *Client) Release(ctx context.Context, reservationID string) error {
	if c == nil {
		return fmt.Errorf("openrails: nil client")
	}
	if strings.TrimSpace(reservationID) == "" {
		return fmt.Errorf("openrails: release requires reservation_id")
	}
	path := "/v1/service/credits/holds/" + reservationID + "/release"
	return c.do(ctx, http.MethodPost, path, nil, nil)
}

// ReleaseHold frees a hold without charging.
func (c *Client) ReleaseHold(ctx context.Context, holdID uuid.UUID) error {
	return c.do(ctx, http.MethodPost, "/v1/service/credits/holds/"+holdID.String()+"/release", map[string]any{}, nil)
}

// WithdrawCredits debits credits directly.
func (c *Client) WithdrawCredits(ctx context.Context, req WithdrawCreditsRequest) (*CreditTransaction, error) {
	var sourceID any
	if req.SourceID != nil {
		sourceID = req.SourceID.String()
	}
	var out CreditTransaction
	err := c.do(ctx, http.MethodPost, "/v1/service/credits/withdraw", map[string]any{
		"tenant_subject_id": payerTenantIDString(req.PayerTenantID),
		"invoker_id":        req.InvokerID,
		"credit_type":       req.CreditType,
		"amount":            req.Amount,
		"source":            req.Source,
		"source_id":         sourceID,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// EnsureCreditType idempotently creates the Tensorhub credit type when needed.
func (c *Client) EnsureCreditType(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "tensorhub_api_credits"
	}
	displayName := strings.ReplaceAll(name, "_", " ")
	return c.do(ctx, http.MethodPost, "/v1/service/credit-types", map[string]any{
		"name":           name,
		"display_name":   displayName,
		"unit":           "USD",
		"decimal_places": 2,
	}, nil)
}

// Balance returns the payer's balance snapshot.
func (c *Client) Balance(ctx context.Context, payerTenantID string) (*BalanceResponse, error) {
	if c == nil {
		return nil, fmt.Errorf("openrails: nil client")
	}
	q := url.Values{}
	q.Set("tenant_subject_id", strings.TrimSpace(payerTenantID))
	if c.creditType != "" {
		q.Set("credit_type", c.creditType)
	}
	var out BalanceResponse
	if err := c.do(ctx, http.MethodGet, "/v1/service/credits/balance?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCreditAccount reads a payer org's balance + policy snapshot.
func (c *Client) GetCreditAccount(ctx context.Context, payerTenantID, creditType string) (*CreditAccount, error) {
	q := url.Values{}
	q.Set("tenant_subject_id", strings.TrimSpace(payerTenantID))
	q.Set("credit_type", normalizeCreditType(creditType))
	var out CreditAccount
	if err := c.do(ctx, http.MethodGet, "/v1/service/credits/balance?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetCreditAccountSettings upserts an org's billing account settings and returns
// the refreshed account snapshot.
func (c *Client) SetCreditAccountSettings(ctx context.Context, payerTenantID, creditType string, in AccountSettingsInput) (*CreditAccount, error) {
	body := map[string]any{
		"tenant_subject_id": strings.TrimSpace(payerTenantID),
		"credit_type":       normalizeCreditType(creditType),
	}
	addAccountSettingFields(body, in)
	if err := c.do(ctx, http.MethodPut, "/v1/service/credits/account-settings", body, nil); err != nil {
		return nil, err
	}
	return c.GetCreditAccount(ctx, payerTenantID, creditType)
}

// ListCreditTransactions reads an org's usage/transaction history from
// OpenRails and passes the canonical JSON through.
func (c *Client) ListCreditTransactions(ctx context.Context, payerTenantID, creditType string, limit int) (json.RawMessage, error) {
	q := url.Values{}
	q.Set("tenant_subject_id", strings.TrimSpace(payerTenantID))
	q.Set("credit_type", normalizeCreditType(creditType))
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	var out json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/v1/service/credits/transactions?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// UsageRollup returns grouped OpenRails usage over [from, to).
func (c *Client) UsageRollup(ctx context.Context, payerTenantID string, from, to time.Time, groupBy string) ([]UsageRollupRow, error) {
	var resp struct {
		Rows []UsageRollupRow `json:"rows"`
	}
	body := map[string]any{
		"tenant_subject_id": payerTenantID,
		"from":              from.UTC().Unix(),
		"to":                to.UTC().Unix(),
		"group_by":          groupBy,
	}
	if err := c.do(ctx, http.MethodPost, "/v1/service/credits/usage/rollup", body, &resp); err != nil {
		return nil, err
	}
	return resp.Rows, nil
}

// BudgetCheck asks OpenRails to compute rolling budget usage for host-owned
// policy windows.
func (c *Client) BudgetCheck(ctx context.Context, tenantSubjectID, invokerID string, windows []BudgetWindowInput, requestedMillicents int64) ([]BudgetWindow, error) {
	if windows == nil {
		windows = []BudgetWindowInput{}
	}
	var resp struct {
		Windows []BudgetWindow `json:"windows"`
	}
	body := map[string]any{
		"tenant_subject_id":    tenantSubjectID,
		"invoker_id":           invokerID,
		"windows":              windows,
		"requested_millicents": requestedMillicents,
	}
	if err := c.do(ctx, http.MethodPost, "/v1/service/budget/check", body, &resp); err != nil {
		return nil, err
	}
	return resp.Windows, nil
}

// EndpointRevenueDailyRow is one day's revenue (millicents) for an endpoint.
type EndpointRevenueDailyRow struct {
	Date             string `json:"date"`
	AmountMillicents int64  `json:"amount_millicents"`
}

// EndpointRevenueResponse is the per-endpoint revenue rollup (#410).
type EndpointRevenueResponse struct {
	RevenueMillicents int64                     `json:"revenue_millicents"`
	Daily             []EndpointRevenueDailyRow `json:"daily"`
}

// EndpointRevenueDaily returns per-day revenue for an endpoint (by usage_event
// endpoint_name) from OpenRails (#410) — backs tensorhub endpoint analytics.
func (c *Client) EndpointRevenueDaily(ctx context.Context, endpointName string, fromUnix, toUnix int64) (*EndpointRevenueResponse, error) {
	if c == nil {
		return nil, fmt.Errorf("openrails: nil client")
	}
	body := map[string]any{
		"endpoint_name": strings.TrimSpace(endpointName),
		"from":          fromUnix,
		"to":            toUnix,
	}
	var out EndpointRevenueResponse
	if err := c.do(ctx, http.MethodPost, "/v1/service/credits/usage/endpoint-revenue", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func addAccountSettingFields(body map[string]any, in AccountSettingsInput) {
	if in.BillingMode != nil {
		body["billing_mode"] = *in.BillingMode
	}
	if in.MaxSpendPerDayCents != nil {
		body["max_spend_per_day_cents"] = *in.MaxSpendPerDayCents
	}
	if in.MaxSpendPerMonthCents != nil {
		body["max_spend_per_month_cents"] = *in.MaxSpendPerMonthCents
	}
	if in.MaxOutstandingOwedCents != nil {
		body["max_outstanding_owed_cents"] = *in.MaxOutstandingOwedCents
	}
	if in.LowBalanceThreshold != nil {
		body["low_balance_threshold_cents"] = *in.LowBalanceThreshold
	}
	if in.AutoTopupEnabled != nil {
		body["auto_topup_enabled"] = *in.AutoTopupEnabled
	}
	if in.AutoTopupAmountCents != nil {
		body["auto_topup_amount_cents"] = *in.AutoTopupAmountCents
	}
	if in.AutoTopupPaymentMethod != nil {
		body["auto_topup_payment_method_id"] = *in.AutoTopupPaymentMethod
	}
	if in.DefaultCreditExpiryDays != nil {
		body["default_credit_expiry_days"] = *in.DefaultCreditExpiryDays
	}
	if in.HardStopOnBreach != nil {
		body["hard_stop_on_breach"] = *in.HardStopOnBreach
	}
	if in.AlertThresholdPct != nil {
		body["alert_threshold_pct"] = *in.AlertThresholdPct
	}
}

func normalizeCreditType(creditType string) string {
	creditType = strings.TrimSpace(creditType)
	if creditType == "" {
		return "tensorhub_api_credits"
	}
	return creditType
}

func payerTenantIDString(payer *PayerTenantID) string {
	if payer == nil || payer.IsZero() {
		return ""
	}
	return payer.UUID().String()
}

func openRailsErrorCode(raw []byte) string {
	var decoded struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Error   any    `json:"error"`
	}
	if err := json.Unmarshal(raw, &decoded); err == nil {
		if code := strings.TrimSpace(decoded.Code); code != "" {
			return code
		}
		if msg := strings.TrimSpace(decoded.Message); msg != "" {
			return msg
		}
		if s, ok := decoded.Error.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return strings.TrimSpace(string(raw))
}

// do issues a single service-JWT-authed request (#411). Transport/timeout
// failures are wrapped as ErrUnreachable; a non-2xx response is a plain error
// (the body is included for diagnostics). out may be nil when no body is expected.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var raw []byte
	if body != nil {
		var merr error
		raw, merr = json.Marshal(body)
		if merr != nil {
			return fmt.Errorf("openrails: marshal request: %w", merr)
		}
	}
	bearer, berr := c.bearer(ctx)
	if berr != nil {
		return berr
	}
	var rdr io.Reader
	if raw != nil {
		rdr = bytes.NewReader(raw)
	}
	req, rerr := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if rerr != nil {
		return fmt.Errorf("openrails: build request: %w", rerr)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/json")
	if raw != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		// 5xx is treated as unreachable (server-side fault → fail-policy applies);
		// 4xx is a definitive client/contract error and is NOT fail-policy-able.
		if resp.StatusCode >= 500 {
			return fmt.Errorf("%w: status=%d body=%s", ErrUnreachable, resp.StatusCode, strings.TrimSpace(string(snippet)))
		}
		switch openRailsErrorCode(snippet) {
		case "insufficient_credits":
			return ErrInsufficientCredits
		case "credit_type_inactive":
			return ErrCreditTypeInactive
		}
		return fmt.Errorf("openrails: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("openrails: decode response: %w", err)
		}
	}
	return nil
}
