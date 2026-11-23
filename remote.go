package openrails

import (
	"bytes"
	"context"
	"crypto"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	auth "github.com/open-rails/authkit/auth"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/http/inprocess"
	"github.com/open-rails/openrails/internal/merchant"
)

// Client is a handle on one merchant: every admin, catalog-edit,
// merchant-config and programmatic operation, acting on that merchant alone.
// Engine.Merchant makes one in process, Remote.Merchant over HTTP; a handle
// is cheap, safe for concurrent use and owns no lifecycle.
// Applications may define narrow interfaces for the methods they use.
type Client struct {
	// engine is the in-process engine behind an Engine's handle; nil otherwise.
	engine *engine.Engine
	// inprocess marks a handle over the in-process transport: its merchant
	// travels as a private context value and it sends no credential.
	inprocess bool
	// invoker is the person an in-process handle's calls act for (ActingFor);
	// nil is the host's own work.
	invoker *auth.Invoker
	baseURL string
	client  *http.Client
	timeout time.Duration
	// merchantID is the handle's merchant.
	merchantID billing.MerchantID
	// token mints the per-call Bearer of a remote handle. It is the sole
	// credential; a mint failure fails the call.
	token     func(context.Context) (string, error)
	assertion *AssertionKey
	// reads answers cached reads; nil reads everything live.
	reads                 *reads
	entitlementChecks     *entitlementCheckCache
	entitlementCacheScope string
	// uncached reads everything live (Uncached).
	uncached bool
}

// MerchantID is the handle's merchant.
func (c *Client) MerchantID() billing.MerchantID { return c.merchantID }

// ActingFor is the in-process handle whose calls act for invoker, the person
// signed in at the host, by issuer and subject as the host's identity
// provider verified them. Each call is then theirs, as their own HTTP call
// would be: their destructive-action limits and holds apply, and a route
// that needs a person, such as ClearDestructiveHold, admits them. A handle
// naming no one is the host's own work: audited, never counted or held. An
// issuer beginning "openrails:" or an empty name is refused at the call
// (host_principal_invalid). A remote handle refuses: its credential names
// its caller.
func (c *Client) ActingFor(invoker auth.Invoker) (*Client, error) {
	switch {
	case c == nil || c.client == nil:
		return nil, fmt.Errorf("openrails: a Client comes from Engine.Merchant or Remote.Merchant")
	case !c.inprocess:
		return nil, fmt.Errorf("openrails: ActingFor is for an in-process handle; a remote handle's credential names its caller")
	}
	d := c.derive()
	d.invoker = &invoker
	return d, nil
}

// Uncached is the handle reading everything from OpenRails: no catalog or
// configuration read and no entitlement check is answered from memory. Its
// answers still refresh the entitlement cache (CheckEntitlementsLive), and its
// writes drop what the shared caches hold for the merchant.
func (c *Client) Uncached() *Client {
	if c == nil {
		return nil
	}
	d := c.derive()
	d.uncached = true
	return d
}

// derive is a copy of the handle, sharing its transport and caches.
func (c *Client) derive() *Client {
	d := *c
	return &d
}

// RemoteCredential is how a remote handle proves itself: exactly one of
// APIKey and Token. A credential is bound to one merchant, the one its
// handle's calls act on.
type RemoteCredential struct {
	// APIKey is a static OpenRails API key.
	APIKey string
	// Token returns the Bearer for each call: a merchant-bound access token
	// the backend obtains from its issuer. It must be safe for concurrent
	// use; an error or an empty token fails the call.
	Token func(context.Context) (string, error)
	// Assertion signs customer tokens (CustomerToken, CustomerAssertion).
	// Nil leaves them unavailable.
	Assertion *AssertionKey
}

// AssertionKey is a merchant backend's signing key, registered with the
// server's AuthKit as the merchant's remote application.
type AssertionKey struct {
	// Issuer is the remote application's issuer: the assertions' iss.
	Issuer string
	// KeyID is the kid the key is registered under.
	KeyID string
	// Signer holds the private key: RSA (RS256), ECDSA P-256, P-384 or P-521
	// (ES256, ES384, ES512) or Ed25519 (EdDSA).
	Signer crypto.Signer
	// TokenURL is the token endpoint assertions are redeemed at, and their
	// aud: the server's AuthKit issuer followed by /oauth2/token. It is not
	// discovered.
	TokenURL string
}

// RemoteOptions configures NewRemote.
type RemoteOptions struct {
	// Credentials holds one credential per merchant the process acts on, by
	// merchant ID. Each call sends OpenRails-Expected-Merchant, so the server
	// refuses a misfiled credential before the operation runs, and each
	// success must name the merchant in OpenRails-Bound-Merchant.
	Credentials map[billing.MerchantID]RemoteCredential
	// HTTPClient is the transport (custom TLS or connection pooling). Nil
	// uses Go's default.
	HTTPClient *http.Client
	// Timeout bounds each call without extending the caller's deadline, also
	// through HTTPClient. Zero leaves the caller's context and the transport
	// in charge.
	Timeout time.Duration
	// ReadCache overrides how long catalog and configuration reads are
	// served from memory; nil keeps the defaults.
	ReadCache ReadCache
	// EntitlementCheckCache configures the entitlement check cache, on by
	// default for APIKey credentials.
	EntitlementCheckCache EntitlementCheckCacheOptions
}

// Remote is a standalone or hosted OpenRails server reached over HTTP, for
// the merchants the process holds credentials for. It has nothing that spans
// merchants: Merchant returns each one's handle.
type Remote struct {
	baseURL string
	client  *http.Client
	handles map[billing.MerchantID]*Client
	reads   *reads
}

// NewRemote builds the client of a standalone or hosted server, the one this
// module ships with. It validates static configuration without I/O; Ready
// checks reachability.
func NewRemote(baseURL string, opts RemoteOptions) (*Remote, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if err := validateBaseURL(baseURL); err != nil {
		return nil, err
	}
	if len(opts.Credentials) == 0 {
		return nil, fmt.Errorf("openrails: RemoteOptions.Credentials names no merchant")
	}
	r := &Remote{baseURL: baseURL, client: opts.HTTPClient, handles: make(map[billing.MerchantID]*Client, len(opts.Credentials))}
	if r.client == nil {
		r.client = &http.Client{}
	}
	entitlementChecks, err := newEntitlementCheckCache(opts.EntitlementCheckCache)
	if err != nil {
		return nil, err
	}
	var refused []config.Degradation
	if r.reads, refused, err = newReads(overHTTP, opts.ReadCache, opts.Timeout, func(id billing.MerchantID) *Client { return r.handles[id] }); err != nil {
		return nil, err
	}
	warnRefused(refused)
	for id, credential := range opts.Credentials {
		if id.IsZero() {
			return nil, fmt.Errorf("openrails: RemoteOptions.Credentials holds a credential under the zero merchant ID")
		}
		handle, err := newRemoteHandle(baseURL, r.client, opts.Timeout, id, credential, entitlementChecks)
		if err != nil {
			return nil, fmt.Errorf("openrails: credential of merchant %s: %w", id, err)
		}
		handle.reads = r.reads
		r.handles[id] = handle
	}
	return r, nil
}

// warnRefused logs each RemoteOptions setting the client ignored.
func warnRefused(refused []config.Degradation) {
	for _, d := range refused {
		log.WithField("subject", d.Field).WithError(d.Err).Warn("openrails: RemoteOptions setting ignored")
	}
}

func newRemoteHandle(baseURL string, client *http.Client, timeout time.Duration, id billing.MerchantID, credential RemoteCredential, entitlementChecks *entitlementCheckCache) (*Client, error) {
	key := strings.TrimSpace(credential.APIKey)
	token := credential.Token
	switch {
	case key != "" && token != nil:
		return nil, fmt.Errorf("set APIKey or Token, not both")
	case key != "":
		token = func(context.Context) (string, error) { return key, nil }
	case token == nil:
		return nil, fmt.Errorf("an APIKey or a Token is required")
	}
	if err := credential.Assertion.validate(); err != nil {
		return nil, err
	}
	handle := &Client{baseURL: baseURL, client: client, timeout: timeout, merchantID: id, token: token, assertion: credential.Assertion}
	if entitlementChecks != nil && key != "" {
		handle.entitlementChecks = entitlementChecks
		handle.entitlementCacheScope = entitlementDigest([]byte(key))
	}
	return handle, nil
}

// Merchant is the handle of merchant id, acting with the credential filed
// under it. A merchant with no credential is billing.ErrMerchantUnresolved.
func (r *Remote) Merchant(id billing.MerchantID) (*Client, error) {
	handle, ok := r.handles[id]
	if !ok {
		return nil, fmt.Errorf("%w: no credential for merchant %s (RemoteOptions.Credentials)", billing.ErrMerchantUnresolved, id)
	}
	return handle, nil
}

// Ready reports whether the server can serve: its /health/ready.
func (r *Remote) Ready(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+"/health/ready", nil)
	if err != nil {
		return err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", billing.ErrUnreachable, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s/health/ready answered %d", billing.ErrUnreachable, r.baseURL, resp.StatusCode)
	}
	return nil
}

// Close releases the transport's idle connections and the read caches'
// background work; handles stay usable.
func (r *Remote) Close(context.Context) error {
	if r != nil {
		r.client.CloseIdleConnections()
		r.reads.close()
	}
	return nil
}

// BoundMerchantError reports a credential bound to another merchant than
// its handle's. The server refused the operation before it ran; the response
// is discarded.
type BoundMerchantError struct {
	// Merchant is the handle's: the ID the credential was filed under in
	// RemoteOptions.Credentials.
	Merchant billing.MerchantID
	// Bound is the merchant the credential is bound to.
	Bound billing.MerchantID
}

// Error names both merchants.
func (e *BoundMerchantError) Error() string {
	return fmt.Sprintf("openrails: the credential of merchant %s is bound to merchant %s; the response was discarded", e.Merchant, e.Bound)
}

// checkBound compares the merchant a response says it acted on with the
// handle's. A remote success must name it; a refusal may precede binding.
func (c *Client) checkBound(resp *http.Response) error {
	values := resp.Header.Values(merchant.BoundHeader)
	if len(values) == 0 {
		if c.inprocess || resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil
		}
		return fmt.Errorf("%w: the response names no merchant in %s", billing.ErrUnreachable, merchant.BoundHeader)
	}
	bound, err := billing.ParseMerchantID(strings.TrimSpace(values[0]))
	if len(values) != 1 || err != nil || bound.IsZero() {
		return fmt.Errorf("%w: malformed %s response header", billing.ErrUnreachable, merchant.BoundHeader)
	}
	if bound != c.merchantID {
		return &BoundMerchantError{Merchant: c.merchantID, Bound: bound}
	}
	return nil
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

// invalidErr builds the canonical client-side "bad request" error so errors.Is
// matches ErrInvalid identically to the embedded transport.
func invalidErr(msg string) error {
	return &billing.StatusError{Status: http.StatusBadRequest, ErrorDetails: billing.ErrorDetails{Type: billing.ErrorTypeInvalidRequest, Code: billing.CodeInvalidParam, Message: msg}}
}

// batchSize refuses an empty batch or one longer than limit before any I/O.
func batchSize(n, limit int) error {
	if n == 0 || n > limit {
		return invalidErr(fmt.Sprintf("a batch holds 1 to %d items", limit))
	}
	return nil
}

// batchIDs refuses an empty, oversized or zero-holding id list before any I/O.
func batchIDs[T interface{ IsZero() bool }](field string, ids []T, limit int) error {
	if err := batchSize(len(ids), limit); err != nil {
		return err
	}
	for _, id := range ids {
		if id.IsZero() {
			return invalidErr(field + " must be nonzero ids")
		}
	}
	return nil
}

// setIDs adds a List's ids filter to q: 1 to billing.MaxBatchItems nonzero
// ids, or nothing when ids is nil. The server refuses ids beside any other
// parameter.
func setIDs[T wireID](q url.Values, ids []T) error {
	if ids == nil {
		return nil
	}
	if err := batchIDs("ids", ids, billing.MaxBatchItems); err != nil {
		return err
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = id.String()
	}
	q.Set("ids", strings.Join(parts, ","))
	return nil
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
	return url.PathEscape(id.String()), nil
}

// bearer mints the credential for the next call. There is no fallback; a mint
// failure or empty token errors the call so the issue surfaces.
func (c *Client) bearer(ctx context.Context) (string, error) {
	if c.token == nil {
		return "", fmt.Errorf("openrails: the handle has no credential")
	}
	tok, err := c.token(ctx)
	if err != nil {
		return "", fmt.Errorf("openrails: mint token: %w", err)
	}
	if tok = strings.TrimSpace(tok); tok == "" {
		return "", fmt.Errorf("openrails: the credential's Token returned an empty token")
	}
	return tok, nil
}

// ListEntitlements returns one page of the keys customers hold at params.At
// (zero: now), by customer then key in byte order: of params.CustomerIDs (1
// to billing.MaxBatchItems), only params.Entitlements and keys under
// params.Prefix. A key absent from the answer is not held. Without
// customers, params.Entitlements names one key and the page is its holders.
func (c *Client) ListEntitlements(ctx context.Context, params billing.EntitlementListParams) (*billing.ListPage[billing.CustomerEntitlement], error) {
	switch {
	case len(params.CustomerIDs) > billing.MaxBatchItems:
		return nil, invalidErr(fmt.Sprintf("at most %d customers per read", billing.MaxBatchItems))
	case len(params.Entitlements) > billing.MaxBatchItems:
		return nil, invalidErr(fmt.Sprintf("at most %d entitlements per read", billing.MaxBatchItems))
	case len(params.CustomerIDs) == 0 && (len(params.Entitlements) != 1 || params.Prefix != ""):
		return nil, invalidErr("without customer ids, name exactly one entitlement")
	}
	query := pageValues(nil, params.PageRequest)
	if len(params.CustomerIDs) > 0 {
		parts := make([]string, len(params.CustomerIDs))
		for i, id := range params.CustomerIDs {
			if id.IsZero() {
				return nil, invalidErr("customer ids must be nonzero")
			}
			parts[i] = id.String()
		}
		query.Set("customer_id", strings.Join(parts, ","))
	}
	for _, key := range params.Entitlements {
		if strings.TrimSpace(key) == "" {
			return nil, invalidErr("entitlements must not contain a blank key")
		}
		query.Add("entitlement", key)
	}
	if params.Prefix != "" {
		query.Set("prefix", params.Prefix)
	}
	if !params.At.IsZero() {
		query.Set("at", params.At.UTC().Format(time.RFC3339Nano))
	}
	var out billing.ListPage[billing.CustomerEntitlement]
	if err := c.do(ctx, http.MethodGet, "/v1/admin/entitlements?"+query.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CheckEntitlements is the programmatic content gate: which of
// params.Entitlements params.CustomerID holds, and the keys they hold under
// each of params.Prefixes, at params.At (zero: now); every requested key and
// prefix is in the answer. One call checks at most
// billing.MaxEntitlementChecks keys and billing.MaxEntitlementPrefixes
// prefixes; more is refused with invalid_param, not split. It needs an
// application holding billing.MerchantEntitlementsRead or RootEntitlementsRead
// (RouteGroups.Programmatic). A remote client answers a current exact-key
// check from its entitlement cache when every key is allowed there
// (EntitlementCheckCacheOptions), unless the handle is Uncached; a denial
// always asks OpenRails.
func (c *Client) CheckEntitlements(ctx context.Context, params billing.CheckEntitlementsParams) (*billing.EntitlementCheck, error) {
	return c.checkEntitlements(ctx, params, c == nil || !c.uncached)
}

// CheckEntitlementsLive checks access against OpenRails, never answering from
// the SDK cache. Its answer replaces allowed keys' entries and drops denied
// ones; a fill begun before it cannot restore them. The drop reaches this
// client's own memory at once, or the shared Redis within a bounded wait; if
// Redis fails, other replicas may serve the old answer for up to MaxAge. Use
// it when a revocation or credential change must be visible immediately.
func (c *Client) CheckEntitlementsLive(ctx context.Context, params billing.CheckEntitlementsParams) (*billing.EntitlementCheck, error) {
	return c.checkEntitlements(ctx, params, false)
}

func (c *Client) checkEntitlements(ctx context.Context, params billing.CheckEntitlementsParams, useCache bool) (*billing.EntitlementCheck, error) {
	switch {
	case params.CustomerID.IsZero():
		return nil, invalidErr("customer_id is required")
	case len(params.Entitlements) == 0 && len(params.Prefixes) == 0:
		return nil, invalidErr("entitlements or prefixes is required")
	case len(params.Entitlements) > billing.MaxEntitlementChecks:
		return nil, invalidErr(fmt.Sprintf("at most %d entitlements per check", billing.MaxEntitlementChecks))
	case len(params.Prefixes) > billing.MaxEntitlementPrefixes:
		return nil, invalidErr(fmt.Sprintf("at most %d prefixes per check", billing.MaxEntitlementPrefixes))
	}
	for _, key := range params.Entitlements {
		if strings.TrimSpace(key) == "" {
			return nil, invalidErr("entitlements must not contain a blank key")
		}
	}
	if params.Entitlements == nil {
		params.Entitlements = []string{}
	}
	if c != nil && c.entitlementChecks != nil {
		if keys := cacheableEntitlementKeys(params); keys != nil {
			return c.checkEntitlementsCached(ctx, params, keys, !useCache)
		}
	}
	var out billing.EntitlementCheck
	if err := c.do(ctx, http.MethodPost, entitlementCheckPath, params, &out); err != nil {
		return nil, err
	}
	return normalizeEntitlementCheck(&out), nil
}

func normalizeEntitlementCheck(out *billing.EntitlementCheck) *billing.EntitlementCheck {
	if out.Entitlements == nil {
		out.Entitlements = map[string]bool{}
	}
	if out.Quantities == nil {
		out.Quantities = map[string]*int{}
	}
	if out.Held == nil {
		out.Held = map[string]billing.HeldEntitlements{}
	}
	return out
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
func (c *Client) doRaw(ctx context.Context, method, path string, body any, headers http.Header) (*clientResponse, error) {
	var rdr io.Reader
	var raw []byte
	headers = headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return nil, fmt.Errorf("openrails: marshal request: %w", err)
		}
		rdr = bytes.NewReader(raw)
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
	})
	return response, err
}

// withHTTPResponse gives JSON and streamed archive operations the same
// merchant binding, credential, cancellation and timeout. consume owns
// response decoding, but cannot outlive the request or leak its body.
func (c *Client) withHTTPResponse(ctx context.Context, method, path string, rdr io.Reader, headers http.Header, consume func(*http.Response) error) error {
	if c == nil || c.client == nil {
		return fmt.Errorf("openrails: a Client comes from Engine.Merchant or Remote.Merchant")
	}
	if changesReads(method, path) {
		// Even a failed write may have landed, or found the cache stale.
		defer c.reads.forget(c.merchantID)
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
	var bearer string
	if c.inprocess {
		// Holding the engine is the authority: the handle's merchant and
		// invoker reach the in-process transport as context values, never as
		// headers.
		ctx = inprocess.WithMerchant(ctx, c.merchantID)
		if c.invoker != nil {
			ctx = inprocess.WithInvoker(ctx, *c.invoker)
		}
	} else if path != "/v1/config" {
		var err error
		if bearer, err = c.bearer(ctx); err != nil {
			return err
		}
	}
	req, rerr := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if rerr != nil {
		return fmt.Errorf("openrails: build request: %w", rerr)
	}
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	// Extra headers never carry a credential: the handle's is the only one.
	req.Header.Del("Authorization")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	// Expectations come from the handle, never caller-supplied headers.
	req.Header.Del(merchant.ExpectedHeader)
	if !c.inprocess && path != "/v1/config" {
		req.Header.Set(merchant.ExpectedHeader, c.merchantID.String())
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", billing.ErrUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if path != "/v1/config" {
		if err := c.checkBound(resp); err != nil {
			return err
		}
	}
	if err := consume(resp); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", billing.ErrUnreachable, err)
	}
	return nil
}

// idempotent is the header carrying key; none for an empty key, which a
// route that requires one refuses.
func idempotent(key string) http.Header {
	if key = strings.TrimSpace(key); key == "" {
		return nil
	}
	return http.Header{"Idempotency-Key": {key}}
}

// do issues a single authed request, mapping any non-2xx onto the canonical
// StatusError. out may be nil when no body is expected.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	return c.doWithHeaders(ctx, method, path, body, out, nil)
}

func (c *Client) doWithHeaders(ctx context.Context, method, path string, body, out any, headers http.Header) error {
	response, err := c.doResponse(ctx, method, path, body, headers)
	if err != nil || out == nil {
		return err
	}
	return decodeJSON(response.body, out)
}

// doResponse preserves the complete typed error contract for operations whose
// successful status distinguishes durable acceptance from final completion.
func (c *Client) doResponse(ctx context.Context, method, path string, body any, headers http.Header) (*clientResponse, error) {
	response, err := c.doRaw(ctx, method, path, body, headers)
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
