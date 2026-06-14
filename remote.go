package openrails

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// remote is the HTTP implementation of Client, ported from go-client/client.go
// (#338). It talks to a standalone OpenRails over the service-token /
// service-JWT-authenticated /v1/service/* routes.
type remote struct {
	baseURL  string
	currency string
	client   *http.Client
	timeout  time.Duration
	// tokenFn mints the per-call Bearer (e.g. a host-signed AuthKit service JWT,
	// #411, or an OpenRails-issued service token). It is the SOLE credential; a
	// mint failure errors the call so the problem surfaces instead of being
	// masked.
	tokenFn func(context.Context) (string, error)
}

// RemoteOption configures NewRemote.
type RemoteOption func(*remote)

// WithHTTPClient injects a transport (tests, custom TLS/conn pooling). When
// unset a client bounded by the configured timeout is created.
func WithHTTPClient(hc *http.Client) RemoteOption {
	return func(r *remote) { r.client = hc }
}

// WithCurrency sets the default currency applied to Authorize/Balance/window
// requests when the request leaves it empty (#476). Empty itself defaults to
// "USD" server-side.
func WithCurrency(currency string) RemoteOption {
	return func(r *remote) { r.currency = strings.TrimSpace(currency) }
}

// WithTokenProvider supplies the per-call Bearer minting function. REQUIRED for
// any authenticated deployment: without it every call fails with a descriptive
// error (the mintless tokenFn pattern from go-client, #411).
func WithTokenProvider(fn func(context.Context) (string, error)) RemoteOption {
	return func(r *remote) { r.tokenFn = fn }
}

// WithTimeout bounds EVERY hot-path call. A short value is the point — a slow
// OpenRails must not stall the request hot path; on timeout the fail-policy
// decides (ErrUnreachable). Defaults to 2s.
func WithTimeout(d time.Duration) RemoteOption {
	return func(r *remote) { r.timeout = d }
}

// NewRemote builds the HTTP-backed Client against a standalone OpenRails.
func NewRemote(baseURL string, opts ...RemoteOption) Client {
	r := &remote{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		timeout: 2 * time.Second,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(r)
		}
	}
	if r.client == nil {
		r.client = &http.Client{Timeout: r.timeout}
	}
	return r
}

// bearer mints the credential for the next call. There is no fallback; a mint
// failure or empty token errors the call so the issue surfaces.
func (c *remote) bearer(ctx context.Context) (string, error) {
	if c.tokenFn == nil {
		return "", fmt.Errorf("openrails: no token provider configured (WithTokenProvider)")
	}
	tok, err := c.tokenFn(ctx)
	if err != nil {
		return "", fmt.Errorf("openrails: mint token: %w", err)
	}
	if strings.TrimSpace(tok) == "" {
		return "", fmt.Errorf("openrails: token provider returned empty token")
	}
	return strings.TrimSpace(tok), nil
}

// Admit implements Client. Verdict statuses (200 OK, 402 money, 403 gated, 429
// throughput) all carry the AdmitResponse body and are returned as a decision
// with a nil error — matching the embedded transport, where a deny is
// (Allowed=false, nil). Handler: ServiceAdmit (service_admission.go).
func (c *remote) Admit(ctx context.Context, req AdmitRequest) (*AdmitResponse, error) {
	status, raw, err := c.doRaw(ctx, http.MethodPost, "/v1/service/admit", req)
	if err != nil {
		return nil, err
	}
	verdict := status == http.StatusOK ||
		status == http.StatusPaymentRequired ||
		status == http.StatusForbidden ||
		status == http.StatusTooManyRequests
	if verdict && !isErrorEnvelope(raw) {
		var out AdmitResponse
		if derr := json.Unmarshal(raw, &out); derr != nil {
			return nil, fmt.Errorf("openrails: decode admit response: %w", derr)
		}
		return &out, nil
	}
	return nil, statusErrorFromBody(status, raw)
}

// Authorize implements Client (handler ServiceAuthorizeCredits).
func (c *remote) Authorize(ctx context.Context, req AuthorizeRequest) (*AuthorizeResponse, error) {
	if req.Currency == "" {
		req.Currency = c.currency
	}
	var out AuthorizeResponse
	if err := c.do(ctx, http.MethodPost, "/v1/service/credits/authorize", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// HoldCredits implements Client (handler ServiceHoldCredits).
func (c *remote) HoldCredits(ctx context.Context, req HoldCreditsRequest) (*CreditHold, error) {
	var out CreditHold
	err := c.do(ctx, http.MethodPost, "/v1/service/credits/hold", map[string]any{
		"customer_id": customerIDString(req.CustomerID),
		"actor":       req.Actor,
		"currency":    req.Currency,
		"amount":      req.Amount,
		"source":      req.Source,
		"source_id":   req.SourceID,
		"expires_at":  req.ExpiresAt.Unix(),
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// CaptureHold implements Client (handler ServiceCaptureHold).
func (c *remote) CaptureHold(ctx context.Context, req CaptureHoldRequest) (*CreditTransaction, error) {
	var out CreditTransaction
	path := "/v1/service/credits/holds/" + req.HoldID.String() + "/capture"
	if err := c.do(ctx, http.MethodPost, path, map[string]any{"amount": req.Amount}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReleaseHold implements Client (handler ServiceReleaseHold).
func (c *remote) ReleaseHold(ctx context.Context, holdID uuid.UUID) error {
	return c.do(ctx, http.MethodPost, "/v1/service/credits/holds/"+holdID.String()+"/release", map[string]any{}, nil)
}

// WithdrawCredits implements Client (handler ServiceWithdrawCredits).
func (c *remote) WithdrawCredits(ctx context.Context, req WithdrawCreditsRequest) (*CreditTransaction, error) {
	var sourceID any
	if req.SourceID != nil {
		sourceID = req.SourceID.String()
	}
	var out CreditTransaction
	err := c.do(ctx, http.MethodPost, "/v1/service/credits/withdraw", map[string]any{
		"customer_id": customerIDString(req.CustomerID),
		"actor":       req.Actor,
		"currency":    req.Currency,
		"amount":      req.Amount,
		"source":      req.Source,
		"source_id":   sourceID,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DepositCredits implements Client (handler ServiceDepositCredits).
func (c *remote) DepositCredits(ctx context.Context, req DepositCreditsRequest) (*CreditTransaction, error) {
	var sourceID any
	if req.SourceID != nil {
		sourceID = req.SourceID.String()
	}
	body := map[string]any{
		"customer_id": customerIDString(req.CustomerID),
		"actor":       req.Actor,
		"currency":    req.Currency,
		"amount":      req.Amount,
		"source":      req.Source,
		"source_id":   sourceID,
	}
	if req.ExpiresAt != nil {
		body["expires_at"] = req.ExpiresAt.Unix()
	}
	if strings.TrimSpace(req.Description) != "" {
		body["description"] = req.Description
	}
	var out CreditTransaction
	if err := c.do(ctx, http.MethodPost, "/v1/service/credits/deposit", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// captureBody is the POST /v1/service/credits/holds/:id/capture body. The wire
// field is "amount" (the handler binds it as REQUIRED).
type captureBody struct {
	Amount    int64          `json:"amount"`
	EventType string         `json:"event_type,omitempty"`
	Resource  string         `json:"resource,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	Source    string         `json:"source,omitempty"`
	SourceID  string         `json:"source_id,omitempty"`
}

// Capture implements Client (handler ServiceCaptureHold). Idempotent on the
// reservation. A nil error means OpenRails accepted the capture.
func (c *remote) Capture(ctx context.Context, reservationID string, capturedMicros int64, usage *CaptureUsage) error {
	if strings.TrimSpace(reservationID) == "" {
		return fmt.Errorf("openrails: capture requires reservation_id")
	}
	body := captureBody{Amount: capturedMicros}
	if usage != nil && strings.TrimSpace(usage.EventType) != "" {
		body.EventType = usage.EventType
		body.Resource = usage.Resource
		body.Metadata = usage.Metadata
		body.Source = usage.Source
		body.SourceID = usage.SourceID
	}
	path := "/v1/service/credits/holds/" + url.PathEscape(strings.TrimSpace(reservationID)) + "/capture"
	return c.do(ctx, http.MethodPost, path, body, nil)
}

// Release implements Client (handler ServiceReleaseHold). Idempotent on the
// reservation. Used when the work fails after a successful authorize/admit.
func (c *remote) Release(ctx context.Context, reservationID string) error {
	if strings.TrimSpace(reservationID) == "" {
		return fmt.Errorf("openrails: release requires reservation_id")
	}
	path := "/v1/service/credits/holds/" + url.PathEscape(strings.TrimSpace(reservationID)) + "/release"
	return c.do(ctx, http.MethodPost, path, nil, nil)
}

// Balance implements Client (handler ServiceGetCreditsBalance).
func (c *remote) Balance(ctx context.Context, customerID string) (*BalanceResponse, error) {
	q := url.Values{}
	q.Set("customer_id", strings.TrimSpace(customerID))
	if c.currency != "" {
		q.Set("currency", c.currency)
	}
	var out BalanceResponse
	if err := c.do(ctx, http.MethodGet, "/v1/service/credits/balance?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCreditAccount implements Client (handler ServiceGetCreditsBalance).
func (c *remote) GetCreditAccount(ctx context.Context, customerID, currency string) (*CreditAccount, error) {
	q := url.Values{}
	q.Set("customer_id", strings.TrimSpace(customerID))
	q.Set("currency", normalizeCurrency(currency))
	var out CreditAccount
	if err := c.do(ctx, http.MethodGet, "/v1/service/credits/balance?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetCreditAccountSettings implements Client (handler
// ServiceSetCreditAccountSettings), then re-reads the account snapshot.
func (c *remote) SetCreditAccountSettings(ctx context.Context, customerID, currency string, in AccountSettingsInput) (*CreditAccount, error) {
	body := map[string]any{
		"customer_id": strings.TrimSpace(customerID),
		"currency":    normalizeCurrency(currency),
	}
	addAccountSettingFields(body, in)
	if err := c.do(ctx, http.MethodPut, "/v1/service/credits/account-settings", body, nil); err != nil {
		return nil, err
	}
	return c.GetCreditAccount(ctx, customerID, currency)
}

// ListCreditTransactions implements Client (handler
// ServiceListCustomerCreditTransactions) and passes the canonical JSON
// through.
func (c *remote) ListCreditTransactions(ctx context.Context, customerID, currency string, limit int) (json.RawMessage, error) {
	q := url.Values{}
	q.Set("customer_id", strings.TrimSpace(customerID))
	q.Set("currency", normalizeCurrency(currency))
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	var out json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/v1/service/credits/transactions?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// UsageRollup implements Client (handler ServiceUsageRollup).
func (c *remote) UsageRollup(ctx context.Context, customerID string, from, to time.Time, groupBy string) ([]UsageRollupRow, error) {
	var resp struct {
		Rows []UsageRollupRow `json:"rows"`
	}
	body := map[string]any{
		"customer_id": customerID,
		"from":        from.UTC().Unix(),
		"to":          to.UTC().Unix(),
		"group_by":    groupBy,
	}
	if err := c.do(ctx, http.MethodPost, "/v1/service/credits/usage/rollup", body, &resp); err != nil {
		return nil, err
	}
	return resp.Rows, nil
}

// BudgetCheck implements Client (handler ServiceBudgetCheck).
func (c *remote) BudgetCheck(ctx context.Context, tenantSubjectID, actorID string, windows []BudgetWindowInput, requestedMicros int64) ([]BudgetWindow, error) {
	if windows == nil {
		windows = []BudgetWindowInput{}
	}
	var resp struct {
		Windows []BudgetWindow `json:"windows"`
	}
	body := map[string]any{
		"customer_id":      tenantSubjectID,
		"actor":            actorID,
		"windows":          windows,
		"requested_micros": requestedMicros,
	}
	if err := c.do(ctx, http.MethodPost, "/v1/service/budget/check", body, &resp); err != nil {
		return nil, err
	}
	return resp.Windows, nil
}

// BudgetStatus implements Client (handler ServiceGetBudget).
func (c *remote) BudgetStatus(ctx context.Context, tenantSubjectID, actorID, tier string) ([]BudgetWindow, error) {
	q := url.Values{}
	q.Set("customer_id", strings.TrimSpace(tenantSubjectID))
	if actorID != "" {
		q.Set("actor", actorID)
	}
	if tier != "" {
		q.Set("tier", tier)
	}
	var resp struct {
		Windows []BudgetWindow `json:"windows"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/service/budget?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	return resp.Windows, nil
}

// SetTierPolicy implements Client (handler ServiceSetTierPolicy).
func (c *remote) SetTierPolicy(ctx context.Context, tenantSubjectID string, in TierPolicyInput) error {
	body := map[string]any{
		"customer_id":        strings.TrimSpace(tenantSubjectID),
		"tier":               in.Tier,
		"windows":            in.Windows,
		"entitled_resources": in.EntitledResources,
		"budget_windows":     in.BudgetWindows,
		"queue_limits":       in.QueueLimits,
	}
	if in.MaxConcurrentHeldMicros != 0 {
		body["max_concurrent_held_micros"] = in.MaxConcurrentHeldMicros
	}
	if in.MaxSingleChargeMicros != 0 {
		body["max_single_charge_micros"] = in.MaxSingleChargeMicros
	}
	return c.do(ctx, http.MethodPut, "/v1/service/tier-policies", body, nil)
}

// SetTierSchedule implements Client (handler ServiceSetTierSchedule, #476).
func (c *remote) SetTierSchedule(ctx context.Context, tenantSubjectID string, schedule []TierScheduleRung) error {
	body := map[string]any{
		"customer_id": strings.TrimSpace(tenantSubjectID),
		"schedule":    schedule,
	}
	return c.do(ctx, http.MethodPut, "/v1/service/tier-schedules", body, nil)
}

// GetTier implements Client (handler ServiceGetTier, #477).
func (c *remote) GetTier(ctx context.Context, tenantSubjectID string) (string, error) {
	q := url.Values{}
	q.Set("customer_id", strings.TrimSpace(tenantSubjectID))
	var resp struct {
		Tier string `json:"tier"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/service/tier?"+q.Encode(), nil, &resp); err != nil {
		return "", err
	}
	return resp.Tier, nil
}

// SetSubjectBudgetPolicy implements Client (handler ServiceSetSubjectBudgetPolicy, #473).
func (c *remote) SetSubjectBudgetPolicy(ctx context.Context, tenantSubjectID string, in SubjectBudgetPolicyInput) error {
	body := map[string]any{
		"customer_id": strings.TrimSpace(tenantSubjectID),
		"scope":       in.Scope,
		"role_id":     in.RoleID,
		"windows":     in.Windows,
	}
	return c.do(ctx, http.MethodPut, "/v1/service/budget-policies/subject", body, nil)
}

// SetPlatformBudgetPolicy implements Client (handler ServiceSetPlatformBudgetPolicy, #473).
func (c *remote) SetPlatformBudgetPolicy(ctx context.Context, tenantSubjectID string, in PlatformBudgetPolicyInput) error {
	body := map[string]any{
		"customer_id": strings.TrimSpace(tenantSubjectID),
		"scope":       in.Scope,
		"windows":     in.Windows,
	}
	return c.do(ctx, http.MethodPut, "/v1/service/budget-policies/platform", body, nil)
}

// SubjectBudgetPolicies implements Client (handler ServiceGetSubjectBudgetPolicies, #473).
func (c *remote) SubjectBudgetPolicies(ctx context.Context, tenantSubjectID string) ([]SubjectBudgetPolicy, error) {
	q := url.Values{}
	q.Set("customer_id", strings.TrimSpace(tenantSubjectID))
	var resp struct {
		Policies []SubjectBudgetPolicy `json:"policies"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/service/budget-policies/subject?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	return resp.Policies, nil
}

// OpenWindow implements Client (handler ServiceOpenCreditWindow, #335).
func (c *remote) OpenWindow(ctx context.Context, req OpenWindowRequest) (*CreditWindow, error) {
	if req.Currency == "" {
		req.Currency = c.currency
	}
	var out CreditWindow
	if err := c.do(ctx, http.MethodPost, "/v1/service/credits/windows", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SettleWindowItems implements Client (handler ServiceSettleCreditWindows).
func (c *remote) SettleWindowItems(ctx context.Context, items []WindowSettleItem) ([]WindowSettleResult, error) {
	var out struct {
		Items []WindowSettleResult `json:"items"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/service/credits/settle", map[string]any{"items": items}, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// RefillWindow implements Client (handler ServiceRefillCreditWindow).
func (c *remote) RefillWindow(ctx context.Context, windowID uuid.UUID, amountMicros, ttlSeconds int64) (*CreditWindow, error) {
	body := map[string]any{"amount": amountMicros, "ttl_seconds": ttlSeconds}
	var out CreditWindow
	if err := c.do(ctx, http.MethodPost, "/v1/service/credits/windows/"+windowID.String()+"/refill", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CloseWindow implements Client (handler ServiceCloseCreditWindow).
func (c *remote) CloseWindow(ctx context.Context, windowID uuid.UUID) (*CreditWindow, error) {
	var out CreditWindow
	if err := c.do(ctx, http.MethodPost, "/v1/service/credits/windows/"+windowID.String()+"/close", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AdmitBatch implements Client (handler ServiceAdmitBatch, #335). The batch
// itself answers 200 with positional per-item verdicts; batch-level validation
// (empty / oversized) is the server's, so both transports reject identically.
func (c *remote) AdmitBatch(ctx context.Context, items []AdmitRequest) ([]AdmitBatchVerdict, error) {
	var out struct {
		Items []AdmitBatchVerdict `json:"items"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/service/admit/batch", map[string]any{"items": items}, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// ListActiveEntitlements implements Client (handler
// ServiceGetExternalSubjectEntitlements, entitlements.go).
func (c *remote) ListActiveEntitlements(ctx context.Context, issuer string, subjects []string, at time.Time) (map[string][]EntitlementRecord, error) {
	body := map[string]any{
		"issuer":   strings.TrimSpace(issuer),
		"subjects": subjects,
	}
	if !at.IsZero() {
		body["at"] = at.UTC().Format(time.RFC3339)
	}
	var out map[string][]EntitlementRecord
	if err := c.do(ctx, http.MethodPost, "/v1/service/customers/by-external-subject/entitlements", body, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string][]EntitlementRecord{}
	}
	return out, nil
}

// ResourceRevenueDaily implements Client (handler ServiceResourceRevenue).
func (c *remote) ResourceRevenueDaily(ctx context.Context, resource string, fromUnix, toUnix int64) (*ResourceRevenueResponse, error) {
	body := map[string]any{
		"resource": strings.TrimSpace(resource),
		"from":     fromUnix,
		"to":       toUnix,
	}
	var out ResourceRevenueResponse
	if err := c.do(ctx, http.MethodPost, "/v1/service/credits/usage/resource-revenue", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func addAccountSettingFields(body map[string]any, in AccountSettingsInput) {
	if in.BillingMode != nil {
		body["billing_mode"] = *in.BillingMode
	}
	if in.MaxSpendPerDayMicros != nil {
		body["max_spend_per_day_micros"] = *in.MaxSpendPerDayMicros
	}
	if in.MaxSpendPerMonthMicros != nil {
		body["max_spend_per_month_micros"] = *in.MaxSpendPerMonthMicros
	}
	if in.MaxOutstandingOwedMicros != nil {
		body["max_outstanding_owed_micros"] = *in.MaxOutstandingOwedMicros
	}
	if in.LowBalanceThreshold != nil {
		body["low_balance_threshold_micros"] = *in.LowBalanceThreshold
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

// normalizeCurrency defaults an empty currency to "USD" (the money ledger's
// DefaultCurrency, #476); a non-empty value passes through unchanged so #475
// qualified custom-credit codes survive (the ledger validates them).
func normalizeCurrency(currency string) string {
	currency = strings.TrimSpace(currency)
	if currency == "" {
		return "USD"
	}
	return currency
}

func customerIDString(payer *CustomerID) string {
	if payer == nil || payer.IsZero() {
		return ""
	}
	return payer.UUID().String()
}

// errorEnvelope mirrors the standalone error response
// (pkg/api.SimpleErrorResponse): {"error":{"type","code","message"}}. Some
// legacy paths also emit top-level {code,message,error}.
type errorEnvelope struct {
	Error *struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func isErrorEnvelope(raw []byte) bool {
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	return json.Unmarshal(raw, &probe) == nil && len(probe.Error) > 0
}

// statusErrorFromBody maps a non-2xx response onto the canonical StatusError
// (errors.go), the remote half of the bidirectional error contract.
func statusErrorFromBody(status int, raw []byte) error {
	var code, message string
	var env errorEnvelope
	if err := json.Unmarshal(raw, &env); err == nil {
		switch {
		case env.Error != nil:
			code, message = env.Error.Code, env.Error.Message
		case env.Code != "" || env.Message != "":
			code, message = env.Code, env.Message
		}
	}
	if code == "" && message == "" {
		message = strings.TrimSpace(string(raw))
	}
	if status >= 500 {
		// 5xx is server-side fault: also unreachable for fail-policy purposes
		// (matches go-client behavior; the embedded transport maps the same
		// failures to ErrInternal only — there is no wire to lose in-process).
		return newStatusError(status, code, message, ErrUnreachable)
	}
	return newStatusError(status, code, message)
}

// doRaw issues a single authed request and returns (status, body) for 2xx and
// the verdict statuses the caller wants to interpret; the caller decides what
// is an error. Transport failures wrap ErrUnreachable.
func (c *remote) doRaw(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var raw []byte
	if body != nil {
		var merr error
		raw, merr = json.Marshal(body)
		if merr != nil {
			return 0, nil, fmt.Errorf("openrails: marshal request: %w", merr)
		}
	}
	bearer, berr := c.bearer(ctx)
	if berr != nil {
		return 0, nil, berr
	}
	var rdr io.Reader
	if raw != nil {
		rdr = bytes.NewReader(raw)
	}
	req, rerr := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if rerr != nil {
		return 0, nil, fmt.Errorf("openrails: build request: %w", rerr)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/json")
	if raw != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: read response: %v", ErrUnreachable, err)
	}
	return resp.StatusCode, out, nil
}

// do issues a single authed request, mapping any non-2xx onto the canonical
// StatusError. out may be nil when no body is expected.
func (c *remote) do(ctx context.Context, method, path string, body, out any) error {
	status, raw, err := c.doRaw(ctx, method, path, body)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return statusErrorFromBody(status, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("openrails: decode response: %w", err)
		}
	}
	return nil
}
