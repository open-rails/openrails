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

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchant"
)

// Client executes the same typed billing operations over an HTTP or in-process
// transport. Applications may define narrow interfaces for the methods they use.
type Client struct {
	// engine is the in-process engine behind New; nil for NewRemote.
	engine                *engine.Engine
	MerchantConfiguration *MerchantConfigurationClient
	baseURL               string
	merchantID            billing.MerchantID
	merchantSlug          string
	catalogOwner          string
	// derived marks a Client made from another (With, ForCatalogOwner): it
	// shares the engine and transport and does not own their lifecycle.
	derived bool
	client  *http.Client
	timeout time.Duration
	// tokenFn mints the per-call Bearer (a host-signed AuthKit service JWT or
	// an OpenRails-issued API key). It is the sole credential; a mint failure
	// fails the call.
	tokenFn      func(context.Context) (string, error)
	credentialFn func(context.Context, CredentialTarget) (string, error)
	// setupErr records invalid static options until construction validates them.
	setupErr error
}

// ClientOption configures a Client: New, NewRemote and Client.With take them.
type ClientOption func(*Client)

// WithMerchantID supplies an immutable default merchant UUID. Request options
// may override it, but the server still enforces credential and runtime scope.
func WithMerchantID(id billing.MerchantID) ClientOption {
	return func(c *Client) {
		if id.IsZero() {
			c.setupErr = fmt.Errorf("openrails: merchant ID must not be zero")
			return
		}
		c.merchantID = id
		c.merchantSlug = ""
	}
}

// MerchantID returns the Client's default UUID, or zero when its default is a
// slug or each operation supplies its own selector.
func (c *Client) MerchantID() billing.MerchantID { return c.merchantID }

// WithHTTPClient injects a transport (custom TLS or connection pooling). When
// unset, the client uses Go's default HTTP transport. An explicit WithTimeout
// still bounds each request independently of this client's Timeout setting.
func WithHTTPClient(hc *http.Client) ClientOption {
	return func(r *Client) { r.client = hc }
}

// WithTokenProvider supplies the function that mints the Bearer token for
// each call. Every client needs a credential: without one NewRemote fails. A
// mint failure fails the call.
func WithTokenProvider(fn func(context.Context) (string, error)) ClientOption {
	return func(r *Client) { r.tokenFn, r.credentialFn = fn, nil }
}

// WithAPIKey authenticates every call with a static OpenRails API key. An
// empty key fails construction.
func WithAPIKey(key string) ClientOption {
	key = strings.TrimSpace(key)
	return func(c *Client) {
		if key == "" {
			c.setupErr = fmt.Errorf("openrails: WithAPIKey requires a nonempty key")
			return
		}
		c.tokenFn = func(context.Context) (string, error) { return key, nil }
		c.credentialFn = nil
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
	if r.tokenFn == nil && r.credentialFn == nil {
		return nil, fmt.Errorf("openrails: token provider is required")
	}
	if r.client == nil {
		r.client = &http.Client{}
	}
	r.initResources()
	return r, nil
}

// With returns a client sharing c's transport, and an embedded client's engine,
// with opts applied: another default merchant, a timeout, or a customer's own
// credential (WithTokenProvider) over the in-process transport. c keeps the
// lifecycle: Close the Client that New or NewRemote returned, not this one.
func (c *Client) With(opts ...ClientOption) (*Client, error) {
	if c == nil {
		return nil, invalidErr("client is required")
	}
	derived := *c
	derived.derived = true
	derived.setupErr = nil
	for _, opt := range opts {
		if opt != nil {
			opt(&derived)
		}
	}
	if derived.setupErr != nil {
		return nil, derived.setupErr
	}
	derived.initResources()
	return &derived, nil
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

// Verify proves the server is reachable and the credential valid with one
// authenticated read of the merchant settings (so the credential needs
// merchant:settings:read). Constructors do no I/O; call it at boot to fail
// fast. A bad credential is billing.ErrUnauthorized; an unreachable server
// or a 5xx is billing.ErrUnreachable.
func (c *Client) Verify(ctx context.Context, requestOptions ...RequestOption) error {
	return c.do(ctx, http.MethodGet, "/v1/merchant/settings", nil, nil, requestOptions...)
}

// invalidErr builds the canonical client-side "bad request" error so errors.Is
// matches ErrInvalid identically to the embedded transport.
func invalidErr(msg string) error {
	return &billing.StatusError{Status: http.StatusBadRequest, ErrorDetails: billing.ErrorDetails{Type: "invalid_request_error", Code: billing.CodeInvalidParam, Message: msg}}
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
func (c *Client) bearer(ctx context.Context, target CredentialTarget) (string, error) {
	if c.tokenFn == nil && c.credentialFn == nil {
		return "", fmt.Errorf("openrails: no token provider configured (WithTokenProvider)")
	}
	var tok string
	var err error
	if c.credentialFn != nil {
		tok, err = c.credentialFn(ctx, target)
	} else {
		tok, err = c.tokenFn(ctx)
	}
	if err != nil {
		return "", fmt.Errorf("openrails: mint token: %w", err)
	}
	if strings.TrimSpace(tok) == "" {
		return "", fmt.Errorf("openrails: token provider returned empty token")
	}
	return strings.TrimSpace(tok), nil
}

// GetMerchantSettings reads the merchant settings document.
func (c *Client) GetMerchantSettings(ctx context.Context, requestOptions ...RequestOption) (*billing.MerchantSettings, error) {
	var out billing.MerchantSettings
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/settings", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetMerchantSettings replaces merchant-owned settings in one validated document.
func (c *Client) SetMerchantSettings(ctx context.Context, settings billing.MerchantSettings, requestOptions ...RequestOption) error {
	return c.do(ctx, http.MethodPut, "/v1/merchant/settings", settings, nil, requestOptions...)
}

// ListEntitlements returns the active entitlements of up to
// billing.MaxEntitlementLookupCustomers customers at params.At (zero: now).
// Every requested customer is present; one with none maps to an empty list.
func (c *Client) ListEntitlements(ctx context.Context, params billing.EntitlementListParams, requestOptions ...RequestOption) (*billing.EntitlementLookup, error) {
	if len(params.CustomerIDs) == 0 {
		return nil, invalidErr("customer_ids is required")
	}
	var out billing.EntitlementLookup
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/entitlements/lookup", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// HasEntitlement reports whether the customer holds entitlement at at (zero:
// now). It reads that one key.
func (c *Client) HasEntitlement(ctx context.Context, customerID billing.CustomerID, entitlement string, at time.Time, requestOptions ...RequestOption) (bool, error) {
	path, err := customerIDPath(customerID)
	if err != nil {
		return false, err
	}
	entitlement = strings.TrimSpace(entitlement)
	if entitlement == "" {
		return false, invalidErr("entitlement is required")
	}
	var out billing.EntitlementCheck
	if err := c.do(ctx, http.MethodPost, path+"/entitlements/check", billing.EntitlementCheckParams{Entitlements: []string{entitlement}, At: at}, &out, requestOptions...); err != nil {
		return false, err
	}
	return out.Entitlements[entitlement], nil
}

// ListEntitlementCustomers returns one page of the customers holding
// entitlement at params.At (zero: now), ordered by customer id.
func (c *Client) ListEntitlementCustomers(ctx context.Context, entitlement string, params billing.EntitlementCustomerListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.CustomerID], error) {
	entitlement = strings.TrimSpace(entitlement)
	if entitlement == "" {
		return nil, invalidErr("entitlement is required")
	}
	query := pageValues(nil, params.PageRequest)
	if !params.At.IsZero() {
		query.Set("at", params.At.UTC().Format(time.RFC3339Nano))
	}
	var out billing.ListPage[billing.CustomerID]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/entitlements/"+url.PathEscape(entitlement)+"/customers?"+query.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetEffectiveTier returns the tier the customer holds in a tier group; its
// Tier is nil when they hold none.
func (c *Client) GetEffectiveTier(ctx context.Context, customerID billing.CustomerID, group string, requestOptions ...RequestOption) (*billing.EffectiveTier, error) {
	path, err := customerIDPath(customerID)
	if err != nil {
		return nil, err
	}
	var out billing.EffectiveTier
	if err := c.do(ctx, http.MethodGet, path+"/tier?"+url.Values{"group": {group}}.Encode(), nil, &out, requestOptions...); err != nil {
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
		Error *billing.ErrorDetails `json:"error"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&envelope); err == nil && envelope.Error != nil {
		var extra any
		if decoder.Decode(&extra) == io.EOF {
			return &billing.StatusError{Status: status, ErrorDetails: *envelope.Error}
		}
	}
	return &billing.StatusError{Status: status, ErrorDetails: billing.ErrorDetails{Message: excerptErrorBody(raw)}}
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
func (c *Client) doRaw(ctx context.Context, method, path string, body any, headers http.Header, requestOptions ...RequestOption) (*clientResponse, error) {
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
			return fmt.Errorf("%w: read response: %w", billing.ErrUnreachable, err)
		}
		if len(out) > 1<<20 {
			return fmt.Errorf("%w: response exceeds 1 MiB", billing.ErrUnreachable)
		}
		response = &clientResponse{status: resp.StatusCode, header: resp.Header, body: out}
		return nil
	}, requestOptions...)
	return response, err
}

// withHTTPResponse gives JSON and streamed archive operations identical merchant
// assertions, credentials, cancellation and timeouts. consume owns response
// decoding, but cannot outlive the request or leak its body.
func (c *Client) withHTTPResponse(ctx context.Context, method, path string, rdr io.Reader, headers http.Header, consume func(*http.Response) error, requestOptions ...RequestOption) error {
	if c.catalogOwner != "" && path != "/v1/catalog" && !strings.HasPrefix(path, "/v1/catalog/") {
		return &billing.StatusError{Status: http.StatusForbidden, ErrorDetails: billing.ErrorDetails{Type: "invalid_request_error", Code: billing.CodeResourceAccessDenied, Message: "catalog-scoped clients only support catalog operations"}}
	}
	target, err := c.requestTarget(requestOptions)
	if err != nil {
		return err
	}
	expectedMerchant := target.MerchantID
	if pinned, ok := merchant.FromContext(ctx); ok && !pinned.IsZero() {
		// A caller context may assert an ID, but never select a merchant. A
		// slug cannot be compared with that assertion before server resolution.
		if target.MerchantSlug != "" {
			return invalidErr("merchant slug selection cannot be combined with an ambient merchant ID assertion")
		}
		if pinned != expectedMerchant {
			return &billing.StatusError{Status: http.StatusConflict, ErrorDetails: billing.ErrorDetails{Type: "invalid_request_error", Code: billing.CodeMerchantBindingMismatch,
				Message: fmt.Sprintf("openrails: call pinned to merchant %s but operation selects merchant %s", pinned, expectedMerchant)}}
		}
	}
	if !expectedMerchant.IsZero() {
		// The local transport uses this explicit request binding; HTTP
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
		return fmt.Errorf("%w: %w", billing.ErrUnreachable, err)
	}
	bearer, berr := c.bearer(ctx, target)
	if berr != nil {
		return berr
	}
	req, rerr := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if rerr != nil {
		return fmt.Errorf("openrails: build request: %w", rerr)
	}
	for name, values := range headers {
		// Extra metadata cannot provide another spelling of the target header.
		// Canonicalize all remaining names before replacing Authorization below.
		if strings.EqualFold(name, merchant.SelectorHeader) {
			continue
		}
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	if c.catalogOwner != "" {
		req.Header.Set("OpenRails-Catalog-Owner", base64.RawURLEncoding.EncodeToString([]byte(c.catalogOwner)))
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	// Exactly one selector: the server resolves it, authorizes the credential
	// for that merchant, and refuses a credential bound to another.
	req.Header.Set(merchant.SelectorHeader, merchant.Selector{Slug: target.MerchantSlug, ID: expectedMerchant}.String())
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", billing.ErrUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := consume(resp); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", billing.ErrUnreachable, err)
	}
	return nil
}

// do issues a single authed request, mapping any non-2xx onto the canonical
// StatusError. out may be nil when no body is expected.
func (c *Client) do(ctx context.Context, method, path string, body, out any, requestOptions ...RequestOption) error {
	return c.doWithHeaders(ctx, method, path, body, out, nil, requestOptions...)
}

func (c *Client) doWithHeaders(ctx context.Context, method, path string, body, out any, headers http.Header, requestOptions ...RequestOption) error {
	response, err := c.doResponse(ctx, method, path, body, headers, requestOptions...)
	if err != nil {
		return err
	}
	if out != nil {
		decoder := json.NewDecoder(bytes.NewReader(response.body))
		decoder.UseNumber()
		if err := decoder.Decode(out); err != nil {
			return fmt.Errorf("%w: decode response: %w", billing.ErrUnreachable, err)
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return fmt.Errorf("%w: response must contain one JSON value", billing.ErrUnreachable)
		}
	}
	return nil
}

// doResponse preserves the complete typed error contract for operations whose
// successful status distinguishes durable acceptance from final completion.
func (c *Client) doResponse(ctx context.Context, method, path string, body any, headers http.Header, requestOptions ...RequestOption) (*clientResponse, error) {
	response, err := c.doRaw(ctx, method, path, body, headers, requestOptions...)
	if err != nil {
		return nil, err
	}
	if response.status < 200 || response.status >= 300 {
		failure := statusErrorFromBody(response.status, response.body).(*billing.StatusError)
		if failure.RequestID == "" {
			failure.RequestID = response.header.Get("X-Request-ID")
		}
		failure.RetryAfter = response.header.Get("Retry-After")
		return nil, failure
	}
	return response, nil
}
