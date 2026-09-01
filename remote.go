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
	"unicode"
	"unicode/utf8"
)

// remote is the HTTP implementation of Client, ported from go-client/client.go
// (#338). It talks to a standalone OpenRails over service-credential
// authenticated /v1/merchant/* routes.
type remote struct {
	baseURL  string
	currency string
	client   *http.Client
	timeout  time.Duration
	// tokenFn mints the per-call Bearer (e.g. a host-signed AuthKit service JWT,
	// #411, or an OpenRails-issued API key). It is the SOLE credential; a
	// mint failure errors the call so the problem surfaces instead of being
	// masked.
	tokenFn func(context.Context) (string, error)
	// urlErr is the static base-URL validation result from NewRemote. The
	// constructor stays no-error (mintless pattern): an invalid URL fails each
	// call with this descriptive error instead.
	urlErr error
}

// RemoteOption configures NewRemote.
type RemoteOption func(*remote)

// WithHTTPClient injects a transport (tests, custom TLS/conn pooling). When
// unset a client bounded by the configured timeout is created. The per-call
// timeout (WithTimeout) is enforced via a per-request context deadline
// REGARDLESS of the injected client, so a custom client that omits
// http.Client.Timeout still cannot stall the hot path.
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

// WithAPIKey authenticates every call with a static OpenRails API key — sugar
// over WithTokenProvider for the blessed static-credential case. An empty key
// fails each call with a descriptive error instead of erroring at construction
// (the mintless tokenFn pattern).
func WithAPIKey(key string) RemoteOption {
	key = strings.TrimSpace(key)
	return WithTokenProvider(func(context.Context) (string, error) {
		if key == "" {
			return "", fmt.Errorf("openrails: WithAPIKey configured with an empty key")
		}
		return key, nil
	})
}

// WithTimeout bounds EVERY hot-path call via a per-request context deadline
// (independent of the http.Client, so WithHTTPClient cannot silently drop it).
// A short value is the point — a slow OpenRails must not stall the request hot
// path; on timeout the fail-policy decides (ErrUnreachable). A non-positive
// value disables the per-call deadline (rely on ctx / the client transport).
// Defaults to 2s.
func WithTimeout(d time.Duration) RemoteOption {
	return func(r *remote) { r.timeout = d }
}

// NewRemote builds the HTTP-backed Client against a standalone OpenRails. The
// constructor is I/O-free and never errors; a statically invalid base URL fails
// every call with a descriptive error (see remote.urlErr).
func NewRemote(baseURL string, opts ...RemoteOption) Client {
	r := &remote{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		timeout: 2 * time.Second,
	}
	r.urlErr = validateBaseURL(r.baseURL)
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

// validateBaseURL is the I/O-free static check on the configured base URL.
func validateBaseURL(baseURL string) error {
	if baseURL == "" {
		return fmt.Errorf("openrails: base URL is empty")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("openrails: invalid base URL %q: %w", baseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("openrails: invalid base URL %q: scheme must be http or https", baseURL)
	}
	if u.Host == "" {
		return fmt.Errorf("openrails: invalid base URL %q: missing host", baseURL)
	}
	return nil
}

// Verify is an authenticated readiness probe (the db.Ping pattern): one cheap
// authenticated GET that proves both reachability AND credential validity.
// Constructors stay I/O-free; hosts that want fail-fast-at-boot call Verify in
// main. It reads /v1/merchant/settings, so the credential needs the merchant
// settings:read permission (any merchant-owner API key has it). Errors map to
// the canonical sentinels: ErrUnauthorized (bad credential), ErrUnreachable
// (transport/5xx), etc.
func (c *remote) Verify(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/v1/merchant/settings", nil, nil)
}

// invalidErr builds the canonical client-side "bad request" error so errors.Is
// matches ErrInvalid identically to the embedded transport (#338).
func invalidErr(msg string) error {
	return NewStatusError(http.StatusBadRequest, "", msg)
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

func admitRequestBody(req AdmitRequest) map[string]any {
	body := map[string]any{
		"customer_id":      req.CustomerID,
		"estimated_amount": req.EstimatedAmount,
		"request_id":       req.RequestID,
	}
	if req.Invoker != "" {
		body["invoker"] = req.Invoker
	}
	if req.InvokerType != "" {
		body["invoker_type"] = req.InvokerType
	}
	if trustLevel := strings.TrimSpace(req.TrustLevel); trustLevel != "" {
		body["trust_level"] = trustLevel
	}
	if req.Resource != "" {
		body["resource"] = req.Resource
	}
	if req.Currency != "" {
		body["currency"] = req.Currency
	}
	if req.Source != "" {
		body["source"] = req.Source
	}
	if req.ExpiresAt != nil {
		body["expires_at"] = *req.ExpiresAt
	}
	if len(req.Roles) > 0 {
		body["roles"] = req.Roles
	}
	return body
}

// DepositCredits implements Client (handler ServiceDepositCredits).
func (c *remote) DepositCredits(ctx context.Context, req DepositCreditsRequest) (*CreditTransaction, error) {
	currency := normalizeCurrency(req.Currency)
	if currency == "" {
		currency = normalizeCurrency(c.currency)
	}
	body := map[string]any{
		"customer_id": customerIDString(req.CustomerID),
		"invoker":     req.Invoker,
		"currency":    currency,
		"amount":      req.Amount,
		"source":      req.Source,
		"source_id":   req.SourceID,
	}
	if req.ExpiresAt != nil {
		body["expires_at"] = req.ExpiresAt.Unix()
	}
	if strings.TrimSpace(req.Description) != "" {
		body["description"] = req.Description
	}
	var out CreditTransaction
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/credits/deposit", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetDeposit implements Client (handler ServiceGetDeposit, or#906). A key that
// never committed returns an error matching ErrNotFound.
func (c *remote) GetDeposit(ctx context.Context, customerID, sourceID string) (*CreditTransaction, error) {
	q := url.Values{}
	q.Set("customer_id", strings.TrimSpace(customerID))
	q.Set("source_id", strings.TrimSpace(sourceID))
	var out CreditTransaction
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/credits/deposit?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// captureBody is the POST /v1/merchant/admissions/:id/capture body. The wire
// field is "amount" (the handler binds it as REQUIRED). customer_id / currency /
// invoker are the ADDITIVE #676 fallback payer coordinates.
type captureBody struct {
	Amount     int64          `json:"amount"`
	CustomerID string         `json:"customer_id,omitempty"`
	Currency   string         `json:"currency,omitempty"`
	Invoker    string         `json:"invoker,omitempty"`
	EventType  string         `json:"event_type,omitempty"`
	Resource   string         `json:"resource,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	Source     string         `json:"source,omitempty"`
	SourceID   string         `json:"source_id,omitempty"`
}

// Capture implements Client (handler ServiceCaptureHold). Idempotent on the
// request_id UNCONDITIONALLY (or#907): the durable coordinate is composed from
// the request id and engine constants alone, so any retry dedupes and a
// changed-amount retry is refused with ErrIdempotencyKeyReused. A nil error
// means OpenRails accepted the capture.
func (c *remote) Capture(ctx context.Context, requestID string, capturedAmount int64, usage *CaptureUsage) error {
	if strings.TrimSpace(requestID) == "" {
		return invalidErr("capture requires request_id")
	}
	body := captureBody{Amount: capturedAmount}
	if usage != nil {
		// #676 fallback payer coordinates travel regardless of EventType.
		body.CustomerID = usage.CustomerID
		body.Currency = usage.Currency
		body.Invoker = usage.Invoker
	}
	if usage != nil && strings.TrimSpace(usage.EventType) != "" {
		body.EventType = usage.EventType
		body.Resource = usage.Resource
		body.Metadata = usage.Metadata
		body.Source = usage.Source
		body.SourceID = usage.SourceID
	}
	path := "/v1/merchant/admissions/" + url.PathEscape(strings.TrimSpace(requestID)) + "/capture"
	return c.do(ctx, http.MethodPost, path, body, nil)
}

// Release implements Client (handler ServiceReleaseHold). Idempotent on the
// request_id. Used when the work fails after a successful authorize/admit.
func (c *remote) Release(ctx context.Context, requestID string) error {
	if strings.TrimSpace(requestID) == "" {
		return invalidErr("release requires request_id")
	}
	path := "/v1/merchant/admissions/" + url.PathEscape(strings.TrimSpace(requestID)) + "/release"
	return c.do(ctx, http.MethodPost, path, nil, nil)
}

// ExtendHold implements Client (handler ServiceExtendHold).
func (c *remote) ExtendHold(ctx context.Context, requestID string, expiresAt time.Time) error {
	if strings.TrimSpace(requestID) == "" {
		return invalidErr("extend requires request_id")
	}
	if expiresAt.IsZero() {
		return invalidErr("extend requires expires_at")
	}
	path := "/v1/merchant/admissions/" + url.PathEscape(strings.TrimSpace(requestID)) + "/extend"
	body := map[string]any{"expires_at": expiresAt.Unix()}
	return c.do(ctx, http.MethodPost, path, body, nil)
}

// Balance implements Client (handler ServiceGetCreditsBalance).
func (c *remote) Balance(ctx context.Context, customerID string) (*BalanceResponse, error) {
	q := url.Values{}
	q.Set("customer_id", strings.TrimSpace(customerID))
	if c.currency != "" {
		q.Set("currency", c.currency)
	}
	var out BalanceResponse
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/credits/balance?"+q.Encode(), nil, &out); err != nil {
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
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/credits/balance?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
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
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/usage/rollup", body, &resp); err != nil {
		return nil, err
	}
	return resp.Rows, nil
}

// GetTrustLevel implements Client (handler ServiceGetTrustLevel, #477).
func (c *remote) GetTrustLevel(ctx context.Context, customerID, currency string) (string, error) {
	q := url.Values{}
	q.Set("customer_id", strings.TrimSpace(customerID))
	q.Set("currency", strings.TrimSpace(currency))
	var resp struct {
		Currency   string `json:"currency"`
		TrustLevel string `json:"trust_level"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/trust-level?"+q.Encode(), nil, &resp); err != nil {
		return "", err
	}
	return resp.TrustLevel, nil
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
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/wasted-spend", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RecordUsage implements Client (handler ServiceRecordUsage, #797).
func (c *remote) RecordUsage(ctx context.Context, report UsageReport) error {
	currency := normalizeCurrency(report.Currency)
	if currency == "" {
		currency = normalizeCurrency(c.currency)
	}
	body := map[string]any{
		"customer_id":      strings.TrimSpace(report.CustomerID),
		"invoker":          report.Invoker,
		"currency":         currency,
		"event_type":       report.EventType,
		"dimensions":       report.Dimensions,
		"amount":           report.Amount,
		"resource":         report.Resource,
		"metadata":         report.Metadata,
		"source":           report.Source,
		"source_id":        report.SourceID,
		"occurred_at_unix": report.OccurredAtUnix,
	}
	return c.do(ctx, http.MethodPost, "/v1/merchant/usage/report", body, nil)
}

// SetCreditLimit implements Client (handler ServiceSetCreditLimit, #489).
func (c *remote) SetCreditLimit(ctx context.Context, customerID, currency string, creditLimit int64) error {
	body := map[string]any{
		"customer_id":         strings.TrimSpace(customerID),
		"currency":            normalizeCurrency(currency),
		"credit_limit_amount": creditLimit,
	}
	return c.do(ctx, http.MethodPut, "/v1/merchant/credit-limit", body, nil)
}

// GetCreditLimit implements Client (handler ServiceGetCreditLimit, #489).
func (c *remote) GetCreditLimit(ctx context.Context, customerID, currency string) (int64, error) {
	q := url.Values{}
	q.Set("customer_id", strings.TrimSpace(customerID))
	q.Set("currency", normalizeCurrency(currency))
	var resp struct {
		CreditLimitAmount int64 `json:"credit_limit_amount"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/credit-limit?"+q.Encode(), nil, &resp); err != nil {
		return 0, err
	}
	return resp.CreditLimitAmount, nil
}

// AdmitBatch implements Client (handler ServiceAdmitBatch, #335). The batch
// itself answers 200 with positional per-item verdicts; batch-level validation
// (empty / oversized) is the server's, so both transports reject identically.
func (c *remote) AdmitBatch(ctx context.Context, items []AdmitRequest) ([]AdmitBatchVerdict, error) {
	bodies := make([]map[string]any, 0, len(items))
	for _, item := range items {
		bodies = append(bodies, admitRequestBody(item))
	}
	var out struct {
		Items []AdmitBatchVerdict `json:"items"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/admissions", map[string]any{"items": bodies}, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// GetMerchantSettings implements PolicySyncClient.
func (c *remote) GetMerchantSettings(ctx context.Context) (*MerchantSettings, error) {
	var out MerchantSettings
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/settings", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetMerchantSettings implements PolicySyncClient.
func (c *remote) SetMerchantSettings(ctx context.Context, settings MerchantSettings) error {
	return c.do(ctx, http.MethodPut, "/v1/merchant/settings", settings, nil)
}

// SetCustomerSpendDelegations implements PolicySyncClient over the
// machine-authenticated merchant surface. The /v1/customers counterpart is
// reserved for a customer-owned delegated browser principal.
func (c *remote) SetCustomerSpendDelegations(ctx context.Context, customerID string, delegations []SpendDelegationInput) error {
	if strings.TrimSpace(customerID) == "" {
		return invalidErr("customer_id required")
	}
	path := "/v1/merchant/customers/" + url.PathEscape(strings.TrimSpace(customerID)) + "/spend-delegations"
	return c.do(ctx, http.MethodPut, path, map[string]any{"delegations": delegations}, nil)
}

// SetCustomerSpendDelegation atomically upserts one customer delegation.
func (c *remote) SetCustomerSpendDelegation(ctx context.Context, customerID string, delegation SpendDelegationInput) error {
	if strings.TrimSpace(customerID) == "" {
		return invalidErr("customer_id required")
	}
	path := "/v1/merchant/customers/" + url.PathEscape(strings.TrimSpace(customerID)) + "/spend-delegations:upsert"
	return c.do(ctx, http.MethodPut, path, delegation, nil)
}

// DeleteCustomerSpendDelegation implements PolicySyncClient (handler
// ServiceDeleteCustomerSpendDelegation): single-grant revocation (or#911).
func (c *remote) DeleteCustomerSpendDelegation(ctx context.Context, customerID, scope, scopeKey string) error {
	if strings.TrimSpace(customerID) == "" {
		return invalidErr("customer_id required")
	}
	if strings.TrimSpace(scope) == "" || strings.TrimSpace(scopeKey) == "" {
		return invalidErr("scope and scope_key required")
	}
	path := "/v1/merchant/customers/" + url.PathEscape(strings.TrimSpace(customerID)) +
		"/spend-delegations/" + url.PathEscape(strings.TrimSpace(scope)) +
		"/" + url.PathEscape(strings.TrimSpace(scopeKey))
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// ListActiveEntitlements implements Client (handler
// ServiceGetExternalSubjectEntitlements, entitlements.go).
func (c *remote) ListActiveEntitlements(ctx context.Context, subjects []string, at time.Time) (map[string][]EntitlementRecord, error) {
	body := map[string]any{
		"subjects": subjects,
	}
	if !at.IsZero() {
		body["at"] = at.UTC().Format(time.RFC3339)
	}
	var out map[string][]EntitlementRecord
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/customers/entitlements:batch", body, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string][]EntitlementRecord{}
	}
	return out, nil
}

// ListEntitlements implements Client as the single-subject form of
// ListActiveEntitlements.
func (c *remote) ListEntitlements(ctx context.Context, subject string, at time.Time) ([]EntitlementRecord, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return nil, invalidErr("subject is required")
	}
	out, err := c.ListActiveEntitlements(ctx, []string{subject}, at)
	if err != nil {
		return nil, err
	}
	return out[subject], nil
}

// HasEntitlement implements Client by checking the single-subject entitlement
// list returned from /v1/merchant/customers/entitlements:batch.
func (c *remote) HasEntitlement(ctx context.Context, subject, entitlement string, at time.Time) (bool, error) {
	entitlement = strings.TrimSpace(entitlement)
	if entitlement == "" {
		return false, invalidErr("entitlement is required")
	}
	records, err := c.ListEntitlements(ctx, subject, at)
	if err != nil {
		return false, err
	}
	for _, rec := range records {
		if rec.Entitlement == entitlement {
			return true, nil
		}
	}
	return false, nil
}

// ListCustomersWithEntitlement implements Client (handler
// ServiceGetCustomersWithEntitlement). It walks the keyset-paginated reverse
// route to completion.
func (c *remote) ListCustomersWithEntitlement(ctx context.Context, entitlement string, at time.Time) ([]string, error) {
	entitlement = strings.TrimSpace(entitlement)
	if entitlement == "" {
		return nil, invalidErr("entitlement is required")
	}
	base := "/v1/merchant/entitlements/" + url.PathEscape(entitlement) + "/customers?limit=1000"
	if !at.IsZero() {
		base += "&at=" + url.QueryEscape(at.UTC().Format(time.RFC3339))
	}
	var all []string
	cursor := ""
	for {
		path := base
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var out struct {
			Customers  []string `json:"customers"`
			NextCursor string   `json:"next_cursor"`
			HasMore    bool     `json:"has_more"`
		}
		if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Customers...)
		if !out.HasMore || strings.TrimSpace(out.NextCursor) == "" {
			break
		}
		cursor = out.NextCursor
	}
	return all, nil
}

// ListProductAccess implements Client (handler ServiceGetUserProductAccess).
func (c *remote) ListProductAccess(ctx context.Context, subject string) ([]ProductAccessGrant, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return nil, invalidErr("subject is required")
	}
	var out []ProductAccessGrant
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/users/"+url.PathEscape(subject)+"/product-access", nil, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []ProductAccessGrant{}
	}
	return out, nil
}

// HasProductAccess implements Client (handler ServiceGetUserProductAccess with
// ?product_id=...).
func (c *remote) HasProductAccess(ctx context.Context, subject, productID string) (bool, error) {
	subject = strings.TrimSpace(subject)
	productID = strings.TrimSpace(productID)
	switch {
	case subject == "":
		return false, invalidErr("subject is required")
	case productID == "":
		return false, invalidErr("product_id is required")
	}
	path := "/v1/merchant/users/" + url.PathEscape(subject) + "/product-access?product_id=" + url.QueryEscape(productID)
	var out ProductAccessCheck
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return false, err
	}
	return out.HasAccess, nil
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
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/usage/resource-revenue", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
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

// errorEnvelope is THE OpenRails error response (pkg/api.ErrorResponse):
// {"error":{"type","code","message"}}. or#893 deleted the second, top-level
// {code,message} shape this used to also accept — one representation, and a
// body that isn't this one is a foreign payload, not a dialect.
type errorEnvelope struct {
	Error *struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// statusErrorFromBody maps a non-2xx response onto the canonical StatusError
// (errors.go), the remote half of the bidirectional error contract.
func statusErrorFromBody(status int, raw []byte) error {
	var code, message string
	var env errorEnvelope
	if err := json.Unmarshal(raw, &env); err == nil && env.Error != nil {
		code, message = env.Error.Code, env.Error.Message
	}
	if code == "" && message == "" {
		// No recognized error envelope: the body is an opaque/foreign payload
		// (a proxy's HTML error page, an upstream stack trace, etc.). Surface a
		// short, single-line excerpt rather than dumping the whole body into the
		// error string — an unbounded body can be large, contain newlines/control
		// characters, or echo internal detail into the caller's logs.
		message = excerptErrorBody(raw)
	}
	if status >= 500 {
		// 5xx is server-side fault: also unreachable for fail-policy purposes
		// (matches go-client behavior; the embedded transport maps the same
		// failures to ErrInternal only — there is no wire to lose in-process).
		return newStatusError(status, code, message, ErrUnreachable)
	}
	return newStatusError(status, code, message)
}

// maxErrorMessageBytes bounds the excerpt taken from an unrecognized (non-
// envelope) error body before it becomes a StatusError message.
const maxErrorMessageBytes = 512

// excerptErrorBody turns an opaque error body into a safe, single-line excerpt:
// it strips control characters (collapsing runs of whitespace), trims, and
// truncates to maxErrorMessageBytes with an ellipsis. This keeps a foreign
// upstream payload (HTML page, stack trace) from polluting the caller's error
// strings/logs while preserving a useful hint.
func excerptErrorBody(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(len(raw))
	prevSpace := false
	for _, r := range string(raw) {
		if r == '\uFFFD' {
			continue
		}
		if r == '\n' || r == '\r' || r == '\t' || unicode.IsControl(r) || unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	msg := strings.TrimSpace(b.String())
	if len(msg) > maxErrorMessageBytes {
		// Truncate on a rune boundary.
		cut := maxErrorMessageBytes
		for cut > 0 && !utf8.RuneStart(msg[cut]) {
			cut--
		}
		msg = strings.TrimSpace(msg[:cut]) + "…"
	}
	return msg
}

// doRaw issues a single authed request and returns (status, body) for 2xx and
// the verdict statuses the caller wants to interpret; the caller decides what
// is an error. Transport failures wrap ErrUnreachable.
func (c *remote) doRaw(ctx context.Context, method, path string, body any) (int, []byte, error) {
	if c.urlErr != nil {
		return 0, nil, c.urlErr
	}
	var raw []byte
	if body != nil {
		var merr error
		raw, merr = json.Marshal(body)
		if merr != nil {
			return 0, nil, fmt.Errorf("openrails: marshal request: %w", merr)
		}
	}
	// Enforce the per-call timeout via a context deadline so it holds even when
	// the host injected its own http.Client (which may have no Timeout). Only
	// shorten, never extend, an existing caller deadline.
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
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
