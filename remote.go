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

// WithCurrency sets the client-level currency used by Balance and by requests
// that leave their currency empty. Empty currency is rejected by service routes.
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
// abuse) all carry the AdmitResponse body and are returned as a decision with a
// nil error — matching the embedded transport, where a deny is (Allowed=false,
// nil). Handler: ServiceAdmit (service_admission.go).
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

// CaptureHold implements Client (handler ServiceCaptureHold).
func (c *remote) CaptureHold(ctx context.Context, req CaptureHoldRequest) (*CreditTransaction, error) {
	var out CreditTransaction
	path := "/v1/service/credits/holds/" + url.PathEscape(strings.TrimSpace(req.RequestID)) + "/capture"
	if err := c.do(ctx, http.MethodPost, path, map[string]any{"amount": req.Amount}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReleaseHold implements Client (handler ServiceReleaseHold).
func (c *remote) ReleaseHold(ctx context.Context, requestID string) error {
	return c.do(ctx, http.MethodPost, "/v1/service/credits/holds/"+url.PathEscape(strings.TrimSpace(requestID))+"/release", map[string]any{}, nil)
}

// WithdrawCredits implements Client (handler ServiceWithdrawCredits).
func (c *remote) WithdrawCredits(ctx context.Context, req WithdrawCreditsRequest) (*CreditTransaction, error) {
	currency := normalizeCurrency(req.Currency)
	if currency == "" {
		currency = normalizeCurrency(c.currency)
	}
	var sourceID any
	if req.SourceID != nil {
		sourceID = req.SourceID.String()
	}
	var out CreditTransaction
	err := c.do(ctx, http.MethodPost, "/v1/service/credits/withdraw", map[string]any{
		"customer_id": customerIDString(req.CustomerID),
		"invoker":     req.Invoker,
		"currency":    currency,
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
	currency := normalizeCurrency(req.Currency)
	if currency == "" {
		currency = normalizeCurrency(c.currency)
	}
	var sourceID any
	if req.SourceID != nil {
		sourceID = req.SourceID.String()
	}
	body := map[string]any{
		"customer_id": customerIDString(req.CustomerID),
		"invoker":     req.Invoker,
		"currency":    currency,
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
// request_id. A nil error means OpenRails accepted the capture.
func (c *remote) Capture(ctx context.Context, requestID string, capturedAmount int64, usage *CaptureUsage) error {
	if strings.TrimSpace(requestID) == "" {
		return fmt.Errorf("openrails: capture requires request_id")
	}
	body := captureBody{Amount: capturedAmount}
	if usage != nil && strings.TrimSpace(usage.EventType) != "" {
		body.EventType = usage.EventType
		body.Resource = usage.Resource
		body.Metadata = usage.Metadata
		body.Source = usage.Source
		body.SourceID = usage.SourceID
	}
	path := "/v1/service/credits/holds/" + url.PathEscape(strings.TrimSpace(requestID)) + "/capture"
	return c.do(ctx, http.MethodPost, path, body, nil)
}

// Release implements Client (handler ServiceReleaseHold). Idempotent on the
// request_id. Used when the work fails after a successful authorize/admit.
func (c *remote) Release(ctx context.Context, requestID string) error {
	if strings.TrimSpace(requestID) == "" {
		return fmt.Errorf("openrails: release requires request_id")
	}
	path := "/v1/service/credits/holds/" + url.PathEscape(strings.TrimSpace(requestID)) + "/release"
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
func (c *remote) UsageRollup(ctx context.Context, customerID, currency string, from, to time.Time, groupBy string) ([]UsageRollupRow, error) {
	var resp struct {
		Rows []UsageRollupRow `json:"rows"`
	}
	body := map[string]any{
		"customer_id": customerID,
		"currency":    normalizeCurrency(currency),
		"from":        from.UTC().Unix(),
		"to":          to.UTC().Unix(),
		"group_by":    groupBy,
	}
	if err := c.do(ctx, http.MethodPost, "/v1/service/credits/usage/rollup", body, &resp); err != nil {
		return nil, err
	}
	return resp.Rows, nil
}

// BudgetStatus implements Client (handler ServiceGetBudget).
func (c *remote) BudgetStatus(ctx context.Context, tenantSubjectID, invokerID, currency, tier string) ([]BudgetWindow, error) {
	q := url.Values{}
	q.Set("customer_id", strings.TrimSpace(tenantSubjectID))
	q.Set("currency", normalizeCurrency(currency))
	if invokerID != "" {
		q.Set("invoker", invokerID)
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

// SetTierSpendCaps implements Client (handler ServiceSetTierSpendCaps).
func (c *remote) SetTierSpendCaps(ctx context.Context, tenantSubjectID string, in TierSpendCapInput) error {
	body := map[string]any{
		"customer_id":     strings.TrimSpace(tenantSubjectID),
		"tier":            in.Tier,
		"budget_windows":  in.BudgetWindows,
		"policy_currency": in.PolicyCurrency,
	}
	if len(in.BadSpendWindows) > 0 {
		body["bad_spend_windows"] = in.BadSpendWindows
	}
	return c.do(ctx, http.MethodPut, "/v1/service/tier-spend-caps", body, nil)
}

// SetTierSchedule implements Client (handler ServiceSetTierSchedule, #476).
func (c *remote) SetTierSchedule(ctx context.Context, tenantSubjectID, currency string, schedule []TierScheduleRung) error {
	body := map[string]any{
		"customer_id": strings.TrimSpace(tenantSubjectID),
		"currency":    strings.TrimSpace(currency),
		"schedule":    schedule,
	}
	return c.do(ctx, http.MethodPut, "/v1/service/tier-schedules", body, nil)
}

// SetMerchantConfiguration implements Client.
func (c *remote) SetMerchantConfiguration(ctx context.Context, in MerchantConfigurationInput) error {
	ws := make([]map[string]any, 0, len(in.DelegatedInvokerWastedSpendWindows))
	for _, w := range in.DelegatedInvokerWastedSpendWindows {
		ws = append(ws, map[string]any{
			"key":            w.Key,
			"window_seconds": w.WindowSeconds,
			"limit":          w.Limit,
			"currency":       w.Currency,
			"cadence":        w.Cadence,
		})
	}
	body := map[string]any{
		"delegated_invoker_wasted_spend_windows": ws,
	}
	return c.do(ctx, http.MethodPut, "/v1/service/merchant-configuration", body, nil)
}

// GetTier implements Client (handler ServiceGetTier, #477).
func (c *remote) GetTier(ctx context.Context, tenantSubjectID, currency string) (string, error) {
	q := url.Values{}
	q.Set("customer_id", strings.TrimSpace(tenantSubjectID))
	q.Set("currency", strings.TrimSpace(currency))
	var resp struct {
		Currency string `json:"currency"`
		Tier     string `json:"tier"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/service/tier?"+q.Encode(), nil, &resp); err != nil {
		return "", err
	}
	return resp.Tier, nil
}

// ReportWastedSpend implements Client (handler ServiceReportWastedSpend, #488).
func (c *remote) ReportWastedSpend(ctx context.Context, report WastedSpendReport) (*WastedSpendResponse, error) {
	body := map[string]any{
		"customer_id":  strings.TrimSpace(report.CustomerID),
		"invoker":      report.Invoker,
		"invoker_type": report.InvokerType,
		"currency":     report.Currency,
		"amount":       report.Amount,
		"source":       report.Source,
		"source_id":    report.SourceID,
		"reason":       report.Reason,
	}
	var out WastedSpendResponse
	if err := c.do(ctx, http.MethodPost, "/v1/service/wasted-spend", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AbuseUsage implements Client (handler ServiceAbuseUsage, #488).
func (c *remote) AbuseUsage(ctx context.Context, tenantSubjectID, invoker, currency, tier string) (*AbuseUsageResponse, error) {
	q := url.Values{}
	q.Set("customer_id", strings.TrimSpace(tenantSubjectID))
	q.Set("currency", normalizeCurrency(currency))
	if invoker != "" {
		q.Set("invoker", invoker)
	}
	if tier != "" {
		q.Set("tier", tier)
	}
	var out AbuseUsageResponse
	if err := c.do(ctx, http.MethodGet, "/v1/service/abuse-usage?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetCreditLimit implements Client (handler ServiceSetCreditLimit, #489).
func (c *remote) SetCreditLimit(ctx context.Context, tenantSubjectID, currency string, creditLimit int64) error {
	body := map[string]any{
		"customer_id":         strings.TrimSpace(tenantSubjectID),
		"currency":            normalizeCurrency(currency),
		"credit_limit_amount": creditLimit,
	}
	return c.do(ctx, http.MethodPut, "/v1/service/credit-limit", body, nil)
}

// GetCreditLimit implements Client (handler ServiceGetCreditLimit, #489).
func (c *remote) GetCreditLimit(ctx context.Context, tenantSubjectID, currency string) (int64, error) {
	q := url.Values{}
	q.Set("customer_id", strings.TrimSpace(tenantSubjectID))
	q.Set("currency", normalizeCurrency(currency))
	var resp struct {
		Currency          string `json:"currency"`
		CreditLimitAmount int64  `json:"credit_limit_amount"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/service/credit-limit?"+q.Encode(), nil, &resp); err != nil {
		return 0, err
	}
	return resp.CreditLimitAmount, nil
}

// SetSubjectSpendCaps implements Client (handler ServiceSetSubjectSpendCaps, #473).
func (c *remote) SetSubjectSpendCaps(ctx context.Context, tenantSubjectID string, in SubjectSpendCapInput) error {
	scopeKey := strings.TrimSpace(in.ScopeKey)
	if scopeKey == "" {
		scopeKey = strings.TrimSpace(in.RoleID)
	}
	body := map[string]any{
		"customer_id": strings.TrimSpace(tenantSubjectID),
		"scope":       in.Scope,
		"scope_key":   scopeKey,
		"role_id":     in.RoleID,
		"windows":     in.Windows,
	}
	return c.do(ctx, http.MethodPut, "/v1/service/spend-caps/subject", body, nil)
}

// SetPlatformSpendCaps implements Client (handler ServiceSetPlatformSpendCaps, #473).
func (c *remote) SetPlatformSpendCaps(ctx context.Context, tenantSubjectID string, in PlatformSpendCapInput) error {
	body := map[string]any{
		"customer_id": strings.TrimSpace(tenantSubjectID),
		"scope":       in.Scope,
		"windows":     in.Windows,
	}
	return c.do(ctx, http.MethodPut, "/v1/service/spend-caps/platform", body, nil)
}

// SubjectSpendCaps implements Client (handler ServiceGetSubjectSpendCaps, #473).
func (c *remote) SubjectSpendCaps(ctx context.Context, tenantSubjectID string) ([]SubjectSpendCap, error) {
	q := url.Values{}
	q.Set("customer_id", strings.TrimSpace(tenantSubjectID))
	var resp struct {
		Policies []SubjectSpendCap `json:"policies"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/service/spend-caps/subject?"+q.Encode(), nil, &resp); err != nil {
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
func (c *remote) RefillWindow(ctx context.Context, windowID uuid.UUID, amount, ttlSeconds int64) (*CreditWindow, error) {
	body := map[string]any{"amount": amount, "ttl_seconds": ttlSeconds}
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
func (c *remote) ResourceRevenueDaily(ctx context.Context, resource, currency string, fromUnix, toUnix int64) (*ResourceRevenueResponse, error) {
	body := map[string]any{
		"resource": strings.TrimSpace(resource),
		"currency": normalizeCurrency(currency),
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
	if in.MaxSpendPerDay != nil {
		body["max_spend_per_day"] = *in.MaxSpendPerDay
	}
	if in.MaxSpendPerMonth != nil {
		body["max_spend_per_month"] = *in.MaxSpendPerMonth
	}
	if in.MaxOutstandingOwedAmount != nil {
		body["max_outstanding_owed_amount"] = *in.MaxOutstandingOwedAmount
	}
	if in.LowBalanceThreshold != nil {
		body["low_balance_threshold"] = *in.LowBalanceThreshold
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

// normalizeCurrency preserves non-empty currency/unit codes and lets the service
// reject missing values consistently.
func normalizeCurrency(currency string) string {
	return strings.TrimSpace(currency)
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
