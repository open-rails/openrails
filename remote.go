package openrails

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/pkg/merchant"
)

// Client executes the same typed billing operations over an HTTP or in-process
// transport. Applications may define narrow interfaces for the methods they use.
type Client struct {
	ProductAccess *ProductAccessService
	baseURL       string
	merchantID    MerchantID
	ownCatalog    bool
	catalogOwner  string
	currency      string
	client        *http.Client
	timeout       time.Duration
	// tokenFn mints the per-call Bearer (e.g. a host-signed AuthKit service JWT,
	// #411, or an OpenRails-issued API key). It is the SOLE credential; a
	// mint failure errors the call so the problem surfaces instead of being
	// masked.
	tokenFn func(context.Context) (string, error)
	// setupErr records invalid static options until construction validates them.
	setupErr error
}

// ClientOption configures NewRemote.
type ClientOption func(*Client)

// WithOwnCatalog selects creator-catalog endpoints for the catalog methods.
// It grants no authority and carries no owner identity: the server's Gate must
// resolve a verified Subject and the corresponding owner permissions.
func WithOwnCatalog() ClientOption {
	return func(c *Client) { c.ownCatalog = true }
}

func (c *Client) catalogPath() string {
	if c.ownCatalog {
		return "/v1/catalog"
	}
	return "/v1/merchant/catalog"
}

// WithMerchantID binds this client to one immutable merchant UUID. The server
// checks the binding against the authenticated merchant before executing a
// command. It is an assertion, never authority to select another merchant.
func WithMerchantID(id MerchantID) ClientOption {
	return func(c *Client) {
		if id.IsZero() {
			c.setupErr = fmt.Errorf("openrails: merchant ID must not be zero")
			return
		}
		c.merchantID = id
	}
}

// MerchantID returns the merchant this client is bound to, or zero when a
// remote client relies on its credential's merchant.
func (c *Client) MerchantID() MerchantID { return c.merchantID }

// WithHTTPClient injects a transport (custom TLS or connection pooling). When
// unset, the client uses Go's default HTTP transport. An explicit WithTimeout
// still bounds each request independently of this client's Timeout setting.
func WithHTTPClient(hc *http.Client) ClientOption {
	return func(r *Client) { r.client = hc }
}

// WithCurrency sets the client-level currency used by Balance and by requests
// that leave their currency empty. Empty currency is rejected by service routes.
func WithCurrency(currency string) ClientOption {
	return func(r *Client) { r.currency = normalizeCurrency(currency) }
}

// WithTokenProvider supplies the per-call Bearer minting function. REQUIRED for
// any authenticated deployment: without it every call fails with a descriptive
// error (the mintless tokenFn pattern from go-client, #411).
func WithTokenProvider(fn func(context.Context) (string, error)) ClientOption {
	return func(r *Client) { r.tokenFn = fn }
}

// WithAPIKey authenticates every call with a static OpenRails API key — sugar
// over WithTokenProvider for the blessed static-credential case. An empty key
// fails each call with a descriptive error instead of erroring at construction
// (the mintless tokenFn pattern).
func WithAPIKey(key string) ClientOption {
	key = strings.TrimSpace(key)
	return func(c *Client) {
		if key == "" {
			c.setupErr = fmt.Errorf("openrails: WithAPIKey requires a nonempty key")
			return
		}
		c.tokenFn = func(context.Context) (string, error) { return key, nil }
	}
}

// WithTimeout adds a per-request budget without extending a caller's earlier
// deadline. It applies in both embedded and remote modes, including when a
// custom HTTP client is supplied. By default the caller context and transport
// own deadlines; a non-positive value leaves that behavior unchanged.
func WithTimeout(d time.Duration) ClientOption {
	return func(r *Client) { r.timeout = d }
}

// NewRemote builds the client for standalone or SaaS HTTP. It validates static
// configuration without I/O; Verify checks live credentials and reachability.
func NewRemote(baseURL string, opts ...ClientOption) (*Client, error) {
	r := &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
	}
	if err := validateBaseURL(r.baseURL); err != nil {
		return nil, err
	}
	for _, opt := range opts {
		if opt != nil {
			opt(r)
		}
	}
	if r.setupErr != nil {
		return nil, r.setupErr
	}
	if r.tokenFn == nil {
		return nil, fmt.Errorf("openrails: token provider is required")
	}
	if r.client == nil {
		r.client = &http.Client{}
	}
	r.ProductAccess = &ProductAccessService{client: r}
	return r, nil
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
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(baseURL, "#") {
		return fmt.Errorf("openrails: base URL must not contain credentials, query or fragment")
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
func (c *Client) Verify(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/v1/merchant/settings", nil, nil)
}

// invalidErr builds the canonical client-side "bad request" error so errors.Is
// matches ErrInvalid identically to the embedded transport (#338).
func invalidErr(msg string) error {
	return &StatusError{Status: http.StatusBadRequest, ErrorDetails: ErrorDetails{Type: "invalid_request_error", Code: "invalid_param", Message: msg}}
}

// requireID trims a caller-supplied identifier and refuses a blank one with
// the server's invalid_param refusal before any I/O, so embedded and remote
// callers observe the same error. Dot segments name nothing and would be
// rewritten by path cleaning.
func requireID(field, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "." || value == ".." {
		return "", invalidErr(field + " is required")
	}
	return value, nil
}

// pathID is requireID escaped as one URL path segment.
func pathID(field, value string) (string, error) {
	value, err := requireID(field, value)
	if err != nil {
		return "", err
	}
	return url.PathEscape(value), nil
}

// requireUUID is requireID for a plain UUID identifier: the zero UUID names nothing.
func requireUUID(field string, id uuid.UUID) (string, error) {
	if id == uuid.Nil {
		return "", invalidErr(field + " is required")
	}
	return id.String(), nil
}

// wireID is any typed identifier of the id family (ids.go).
type wireID interface {
	IsZero() bool
	String() string
}

// requireTypedID is requireID for a typed identifier: the zero id names
// nothing; the result is the id's wire spelling, safe as one path segment.
func requireTypedID(field string, id wireID) (string, error) {
	if id.IsZero() {
		return "", invalidErr(field + " is required")
	}
	return id.String(), nil
}

// bearer mints the credential for the next call. There is no fallback; a mint
// failure or empty token errors the call so the issue surfaces.
func (c *Client) bearer(ctx context.Context) (string, error) {
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

// DepositCredits implements Client (handler ServiceDepositCredits).
func (c *Client) DepositCredits(ctx context.Context, req DepositCreditsRequest) (*CreditTransaction, error) {
	currency := normalizeCurrency(req.Currency)
	if currency == "" {
		currency = normalizeCurrency(c.currency)
	}
	req.Currency = currency
	var out CreditTransaction
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/credits/deposit", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetDeposit implements Client (handler ServiceGetDeposit, or#906). A key that
// never committed returns an error matching ErrNotFound.
func (c *Client) GetDeposit(ctx context.Context, customerID CustomerID, sourceID string) (*CreditTransaction, error) {
	customer, err := requireTypedID("customer_id", customerID)
	if err != nil {
		return nil, err
	}
	// A deposit source is an opaque idempotency key carried in the query,
	// so dot strings are valid keys, not traversal components.
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return nil, invalidErr("source_id is required")
	}
	q := url.Values{}
	q.Set("customer_id", customer)
	q.Set("source_id", sourceID)
	var out CreditTransaction
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/credits/deposit?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Capture settles an original admission. Exact retries return the original
// receipt; changed amount or usage terms return ErrIdempotencyKeyReused.
func (c *Client) Capture(ctx context.Context, requestID string, capturedAmount int64, usage *CaptureUsage) (*CaptureReceipt, error) {
	if strings.TrimSpace(requestID) == "" {
		return nil, invalidErr("request_id is required")
	}
	body := CaptureRequest{Amount: &capturedAmount}
	if usage != nil {
		body.CaptureUsage = *usage
	}
	path := admissionActionPath(requestID, "capture")
	var out CaptureReceipt
	if err := c.do(ctx, http.MethodPost, path, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// admissionActionPath preserves arbitrary request IDs as one URL segment.
// PathEscape leaves whole dot segments intact; ServeMux would clean those away.
func admissionActionPath(requestID, action string) string {
	segment := url.PathEscape(strings.TrimSpace(requestID))
	if segment == "." || segment == ".." {
		segment = strings.ReplaceAll(segment, ".", "%2E")
	}
	return "/v1/merchant/admissions/" + segment + "/" + action
}

// Release implements Client (handler ServiceReleaseHold). Idempotent on the
// request_id. Used when the work fails after a successful authorize/admit.
func (c *Client) Release(ctx context.Context, requestID string) error {
	if strings.TrimSpace(requestID) == "" {
		return invalidErr("request_id is required")
	}
	path := admissionActionPath(requestID, "release")
	return c.do(ctx, http.MethodPost, path, nil, nil)
}

// ExtendHold moves a live hold's deadline to expiresAt. ErrNotFound means the
// hold was captured, released or lapsed; re-admit instead.
func (c *Client) ExtendHold(ctx context.Context, requestID string, expiresAt time.Time) error {
	if strings.TrimSpace(requestID) == "" {
		return invalidErr("request_id is required")
	}
	if expiresAt.IsZero() {
		return invalidErr("expires_at is required")
	}
	path := admissionActionPath(requestID, "extend")
	body := map[string]any{"expires_at": expiresAt.UTC().Format(time.RFC3339Nano)}
	return c.do(ctx, http.MethodPost, path, body, nil)
}

// Balance implements Client (handler ServiceGetCreditsBalance).
func (c *Client) Balance(ctx context.Context, customerID CustomerID) (*BalanceResponse, error) {
	customer, err := requireTypedID("customer_id", customerID)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("customer_id", customer)
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
func (c *Client) GetCreditAccount(ctx context.Context, customerID CustomerID, currency string) (*CreditAccount, error) {
	customer, err := requireTypedID("customer_id", customerID)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("customer_id", customer)
	q.Set("currency", normalizeCurrency(currency))
	var out CreditAccount
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/credits/balance?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UsageRollup implements Client (handler ServiceUsageRollup).
func (c *Client) UsageRollup(ctx context.Context, customerID CustomerID, currency string, from, to time.Time, groupBy string) ([]UsageRollupRow, error) {
	customer, err := requireTypedID("customer_id", customerID)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Rows []UsageRollupRow `json:"rows"`
	}
	body := map[string]any{
		"customer_id": customer,
		"currency":    normalizeCurrency(currency),
		"from":        from.UTC().Format(time.RFC3339Nano),
		"to":          to.UTC().Format(time.RFC3339Nano),
		"group_by":    groupBy,
	}
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/usage/rollup", body, &resp); err != nil {
		return nil, err
	}
	return resp.Rows, nil
}

// GetTrustLevel implements Client (handler ServiceGetTrustLevel, #477).
func (c *Client) GetTrustLevel(ctx context.Context, customerID CustomerID, currency string) (string, error) {
	customer, err := requireTypedID("customer_id", customerID)
	if err != nil {
		return "", err
	}
	q := url.Values{}
	q.Set("customer_id", customer)
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
func (c *Client) ReportWastedSpend(ctx context.Context, report WastedSpendReport) (*WastedSpendResponse, error) {
	var out WastedSpendResponse
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/wasted-spend", report, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RecordUsage implements Client (handler ServiceRecordUsage, #797).
func (c *Client) RecordUsage(ctx context.Context, report UsageReport) error {
	currency := normalizeCurrency(report.Currency)
	if currency == "" {
		currency = normalizeCurrency(c.currency)
	}
	report.Currency = currency

	return c.do(ctx, http.MethodPost, "/v1/merchant/usage/report", report, nil)
}

// SetCreditLimit implements Client (handler ServiceSetCreditLimit, #489).
func (c *Client) SetCreditLimit(ctx context.Context, customerID CustomerID, currency string, creditLimit int64) error {
	if _, err := requireTypedID("customer_id", customerID); err != nil {
		return err
	}
	body := CreditLimitRequest{CustomerID: customerID, Currency: normalizeCurrency(currency), CreditLimitAmount: creditLimit}
	return c.do(ctx, http.MethodPut, "/v1/merchant/credit-limit", body, nil)
}

// GetCreditLimit implements Client (handler ServiceGetCreditLimit, #489).
func (c *Client) GetCreditLimit(ctx context.Context, customerID CustomerID, currency string) (int64, error) {
	customer, err := requireTypedID("customer_id", customerID)
	if err != nil {
		return 0, err
	}
	q := url.Values{}
	q.Set("customer_id", customer)
	q.Set("currency", normalizeCurrency(currency))
	var resp struct {
		CreditLimitAmount int64 `json:"credit_limit_amount,string"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/credit-limit?"+q.Encode(), nil, &resp); err != nil {
		return 0, err
	}
	return resp.CreditLimitAmount, nil
}

// Admit is the single-request form of AdmitBatch on every transport.
func (c *Client) Admit(ctx context.Context, request AdmitRequest) (*AdmitResponse, error) {
	verdicts, err := c.AdmitBatch(ctx, []AdmitRequest{request})
	if err != nil {
		return nil, err
	}
	if len(verdicts) != 1 {
		return nil, fmt.Errorf("openrails: admission returned %d verdicts for one request", len(verdicts))
	}
	if verdicts[0].Result != nil {
		return verdicts[0].Result, nil
	}
	if verdicts[0].Error != nil {
		return nil, &StatusError{Status: verdicts[0].Status, ErrorDetails: *verdicts[0].Error}
	}
	return nil, fmt.Errorf("openrails: admission returned neither a decision nor an error")
}

// AdmitBatch implements Client (handler ServiceAdmitBatch, #335). The batch
// itself answers 200 with positional per-item verdicts; batch-level validation
// (empty / oversized) is the server's, so both transports reject identically.
func (c *Client) AdmitBatch(ctx context.Context, items []AdmitRequest) ([]AdmitBatchVerdict, error) {
	var out struct {
		Items []AdmitBatchVerdict `json:"items"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/admissions", struct {
		Items []AdmitRequest `json:"items"`
	}{Items: items}, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// GetMerchantSettings reads the merchant settings document.
func (c *Client) GetMerchantSettings(ctx context.Context) (*MerchantSettings, error) {
	var out MerchantSettings
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/settings", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetMerchantSettings replaces merchant-owned settings in one validated document.
func (c *Client) SetMerchantSettings(ctx context.Context, settings MerchantSettings) error {
	return c.do(ctx, http.MethodPut, "/v1/merchant/settings", settings, nil)
}

// GetCustomerBillingPolicy reads the explicit assignment, not the resolved tier/default policy.
func (c *Client) GetCustomerBillingPolicy(ctx context.Context, customerID CustomerID) (*CustomerBillingPolicyAssignment, error) {
	path, err := customerPath(customerID)
	if err != nil {
		return nil, err
	}
	var out CustomerBillingPolicyAssignment
	if err := c.do(ctx, http.MethodGet, path+"/billing-policy", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetCustomerBillingPolicy assigns a declared policy; nil explicitly clears the
// assignment and restores tier/default inheritance. It never creates a customer.
func (c *Client) SetCustomerBillingPolicy(ctx context.Context, customerID CustomerID, policyName *string) (*CustomerBillingPolicyAssignment, error) {
	path, err := customerPath(customerID)
	if err != nil {
		return nil, err
	}
	body := struct {
		PolicyName *string `json:"policy_name"`
	}{PolicyName: policyName}
	var out CustomerBillingPolicyAssignment
	if err := c.do(ctx, http.MethodPut, path+"/billing-policy", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetCustomerSpendDelegations replaces the customer's complete delegation
// document over the machine-authenticated merchant surface.
func (c *Client) SetCustomerSpendDelegations(ctx context.Context, customerID CustomerID, delegations []SpendDelegationInput) error {
	path, err := customerPath(customerID)
	if err != nil {
		return err
	}
	path += "/spend-delegations"
	return c.do(ctx, http.MethodPut, path, map[string]any{"delegations": delegations}, nil)
}

// SetCustomerSpendDelegation atomically upserts one customer delegation.
func (c *Client) SetCustomerSpendDelegation(ctx context.Context, customerID CustomerID, delegation SpendDelegationInput) error {
	path, err := customerPath(customerID)
	if err != nil {
		return err
	}
	path += "/spend-delegations:upsert"
	return c.do(ctx, http.MethodPut, path, delegation, nil)
}

// DeleteCustomerSpendDelegation revokes exactly one delegation (or#911); a
// missing grant returns ErrNotFound.
func (c *Client) DeleteCustomerSpendDelegation(ctx context.Context, customerID CustomerID, scope, scopeKey string) error {
	path, err := customerPath(customerID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(scope) == "" || strings.TrimSpace(scopeKey) == "" {
		return invalidErr("scope and scope_key required")
	}
	path += "/spend-delegations/" + url.PathEscape(strings.TrimSpace(scope)) +
		"/" + url.PathEscape(strings.TrimSpace(scopeKey))
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// ListActiveEntitlements returns active records for up to 500 subjects, keyed
// by every requested subject after trim and dedupe; unknown subjects map to an
// empty slice. A zero at means now.
func (c *Client) ListActiveEntitlements(ctx context.Context, subjects []CustomerID, at time.Time) (map[CustomerID][]EntitlementRecord, error) {
	body := map[string]any{
		"subjects": subjects,
	}
	if !at.IsZero() {
		body["at"] = at.UTC().Format(time.RFC3339Nano)
	}
	var out map[CustomerID][]EntitlementRecord
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/customers/entitlements:batch", body, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[CustomerID][]EntitlementRecord{}
	}
	return out, nil
}

// ListEntitlements implements Client as the single-subject form of
// ListActiveEntitlements.
func (c *Client) ListEntitlements(ctx context.Context, subject CustomerID, at time.Time) ([]EntitlementRecord, error) {
	if subject.IsZero() {
		return nil, invalidErr("subject is required")
	}
	out, err := c.ListActiveEntitlements(ctx, []CustomerID{subject}, at)
	if err != nil {
		return nil, err
	}
	return out[subject], nil
}

// HasEntitlement implements Client by checking the single-subject entitlement
// list returned from /v1/merchant/customers/entitlements:batch.
func (c *Client) HasEntitlement(ctx context.Context, subject CustomerID, entitlement string, at time.Time) (bool, error) {
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
func (c *Client) ListCustomersWithEntitlement(ctx context.Context, entitlement string, at time.Time) ([]CustomerID, error) {
	entitlement = strings.TrimSpace(entitlement)
	if entitlement == "" {
		return nil, invalidErr("entitlement is required")
	}
	base := "/v1/merchant/entitlements/" + url.PathEscape(entitlement) + "/customers?limit=1000"
	if !at.IsZero() {
		base += "&at=" + url.QueryEscape(at.UTC().Format(time.RFC3339Nano))
	}
	var all []CustomerID
	cursor := ""
	for {
		path := base
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var out struct {
			Customers  []CustomerID `json:"customers"`
			NextCursor string       `json:"next_cursor"`
			HasMore    bool         `json:"has_more"`
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

// ResourceRevenueDaily implements Client (handler ServiceResourceRevenue).
func (c *Client) ResourceRevenueDaily(ctx context.Context, resource, currency string, from, to time.Time) (*ResourceRevenueResponse, error) {
	body := map[string]any{
		"resource": strings.TrimSpace(resource),
		"currency": normalizeCurrency(currency),
		"from":     from.UTC().Format(time.RFC3339Nano),
		"to":       to.UTC().Format(time.RFC3339Nano),
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
	currency = strings.TrimSpace(currency)
	if strings.ContainsAny(currency, ":/") {
		return currency // a qualified unit keeps its exact spelling
	}
	return strings.ToUpper(currency)
}

// statusErrorFromBody decodes the one canonical error envelope. Foreign proxy
// responses retain a bounded diagnostic excerpt but never gain a machine code.
func statusErrorFromBody(status int, raw []byte) error {
	var envelope struct {
		Error *ErrorDetails `json:"error"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&envelope); err == nil && envelope.Error != nil {
		var extra any
		if decoder.Decode(&extra) == io.EOF {
			return &StatusError{Status: status, ErrorDetails: *envelope.Error}
		}
	}
	return &StatusError{Status: status, ErrorDetails: ErrorDetails{Message: excerptErrorBody(raw)}}
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

type clientResponse struct {
	status int
	header http.Header
	body   []byte
}

// doRaw issues a single authed request and returns (response, body) for 2xx and
// the verdict statuses the caller wants to interpret; the caller decides what
// is an error. Transport failures wrap ErrUnreachable.
func (c *Client) doRaw(ctx context.Context, method, path string, body any, headers http.Header) (*clientResponse, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("openrails: marshal request: %w", err)
		}
		rdr = bytes.NewReader(raw)
		headers = headers.Clone()
		if headers == nil {
			headers = make(http.Header)
		}
		headers.Set("Content-Type", "application/json")
	}
	var response *clientResponse
	err := c.withHTTPResponse(ctx, method, path, rdr, headers, func(resp *http.Response) error {
		out, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
		if err != nil {
			return fmt.Errorf("%w: read response: %w", ErrUnreachable, err)
		}
		if len(out) > 1<<20 {
			return fmt.Errorf("%w: response exceeds 1 MiB", ErrUnreachable)
		}
		response = &clientResponse{status: resp.StatusCode, header: resp.Header, body: out}
		return nil
	})
	return response, err
}

// withHTTPResponse gives JSON and streamed archive operations identical merchant
// assertions, credentials, cancellation and timeouts. consume owns response
// decoding, but cannot outlive the request or leak its body.
func (c *Client) withHTTPResponse(ctx context.Context, method, path string, rdr io.Reader, headers http.Header, consume func(*http.Response) error) error {
	if c.catalogOwner != "" && path != "/v1/catalog" && !strings.HasPrefix(path, "/v1/catalog/") {
		return &StatusError{Status: http.StatusForbidden, ErrorDetails: ErrorDetails{Type: "invalid_request_error", Code: "permission_denied", Message: "catalog-scoped clients only support catalog operations"}}
	}
	expectedMerchant := c.merchantID
	if pinned, ok := merchant.FromContext(ctx); ok && !pinned.IsZero() {
		// A merchant on the caller's context is never a selection. Against a
		// bound client it must agree with the construction-time binding
		// (#772); an unbound remote client forwards it as the assertion the
		// server verifies against the credential's authority.
		if !expectedMerchant.IsZero() && pinned != expectedMerchant {
			return &StatusError{Status: http.StatusConflict, ErrorDetails: ErrorDetails{Type: "invalid_request_error", Code: "resource_conflict",
				Message: fmt.Sprintf("openrails: call pinned to merchant %s but client is bound to merchant %s", pinned, expectedMerchant)}}
		}
		expectedMerchant = pinned
	}
	if !expectedMerchant.IsZero() {
		// The local transport uses this construction-time binding; HTTP
		// servers resolve authority independently and verify the header.
		ctx = merchant.WithID(ctx, expectedMerchant)
	}
	// Enforce the per-call timeout via a context deadline so it holds even when
	// the host injected its own http.Client (which may have no Timeout). Only
	// shorten, never extend, an existing caller deadline.
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	bearer, berr := c.bearer(ctx)
	if berr != nil {
		return berr
	}
	req, rerr := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if rerr != nil {
		return fmt.Errorf("openrails: build request: %w", rerr)
	}
	for name, values := range headers {
		req.Header[name] = append([]string(nil), values...)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	if c.catalogOwner != "" {
		req.Header.Set("OpenRails-Catalog-Owner", base64.RawURLEncoding.EncodeToString([]byte(c.catalogOwner)))
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	if !expectedMerchant.IsZero() {
		req.Header.Set(merchant.BindingHeader, expectedMerchant.String())
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := consume(resp); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	return nil
}

// do issues a single authed request, mapping any non-2xx onto the canonical
// StatusError. out may be nil when no body is expected.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	return c.doWithHeaders(ctx, method, path, body, out, nil)
}

func (c *Client) doWithHeaders(ctx context.Context, method, path string, body, out any, headers http.Header) error {
	response, err := c.doResponse(ctx, method, path, body, headers)
	if err != nil {
		return err
	}
	if out != nil {
		decoder := json.NewDecoder(bytes.NewReader(response.body))
		decoder.UseNumber()
		if err := decoder.Decode(out); err != nil {
			return fmt.Errorf("%w: decode response: %w", ErrUnreachable, err)
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return fmt.Errorf("%w: response must contain one JSON value", ErrUnreachable)
		}
	}
	return nil
}

// doResponse preserves the complete typed error contract for operations whose
// successful status distinguishes durable acceptance from final completion.
func (c *Client) doResponse(ctx context.Context, method, path string, body any, headers http.Header) (*clientResponse, error) {
	response, err := c.doRaw(ctx, method, path, body, headers)
	if err != nil {
		return nil, err
	}
	if response.status < 200 || response.status >= 300 {
		failure := statusErrorFromBody(response.status, response.body).(*StatusError)
		if failure.RequestID == "" {
			failure.RequestID = response.header.Get("X-Request-ID")
		}
		failure.RetryAfter = response.header.Get("Retry-After")
		return nil, failure
	}
	return response, nil
}
