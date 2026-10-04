package request

import (
	"bytes"
	"context"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/iputil"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// Transport is the backend behind a Request. Since #670 the only production
// backend is the net/http one (NewHTTP); the interface remains the seam that
// keeps handlers framework-agnostic.
type Transport interface {
	WriteJSON(code int, body any)
	AbortJSON(code int, body any)
	Bind(data any) error
	BindJSON(data any) error
	BindQuery(data any) error
	BindURI(data any) error
	Param(key string) string
	Query(key string) string
	Get(key string) (any, bool)
	Set(key string, value any)
	Next()
	Header(key string) string
	SetHeader(key, value string)
	Redirect(code int, location string)
	PostForm(key string) string
	FormFile(key string) (multipart.File, *multipart.FileHeader, error)
	UserContext() (billingauth.UserContext, bool)
}

type Request struct {
	State   *app.Runtime
	Request *http.Request
	Clock   clockwork.Clock

	t Transport

	// uc is the authenticated principal pinned by the auth middleware
	// (SetUserContext). It is the source of truth for UserContext()/GetUser so
	// the identity survives middleware reassigning r.Request — the net/http
	// Transport caches its own *http.Request and would otherwise not see a
	// UserContext stored only on a re-wrapped request context.
	uc    billingauth.UserContext
	ucSet bool

	requestID string
}

// NewWithTransport builds a Request over an arbitrary Transport (test seam);
// production uses NewHTTP.
func NewWithTransport(runtime *app.Runtime, r *http.Request, t Transport) *Request {
	var clock clockwork.Clock
	if runtime != nil {
		clock = runtime.Clock
	}
	return &Request{
		State:   runtime,
		Request: r,
		Clock:   timeutil.FirstClock(clock),
		t:       t,
	}
}

// NewHTTP builds a net/http-backed Request (embedded surface) — no gin.
func NewHTTP(w http.ResponseWriter, r *http.Request, runtime *app.Runtime) *Request {
	return NewWithTransport(runtime, r, newHTTPTransport(w, r))
}

// writeDeadliner is implemented by transports that own a connection whose
// write deadline a route budget must lift (the net/http one).
type writeDeadliner interface {
	SetWriteDeadline(deadline time.Time) error
}

// Budget bounds the handler's own work by d — the route's declared budget,
// derived from the provider round-trips it makes — and lifts the connection's
// write deadline for the duration (xs-007 row 37).
//
// A server-level WriteTimeout is set when the request is read, before the
// handler knows what it is about to do. The standalone server used to carry
// 30 s while the payment-method replacement route declared 50 s: the provider
// write committed at the provider, the durable rows landed, and the client
// received EOF instead of the response. A host mounting this handler in its
// own server can carry any number. The route is the only place that knows its
// budget, so the route is where the connection learns it: the write deadline
// is cleared here for the request's life and the route's ctx is the one bound.
// Response bodies here are small JSON; their write completes into the socket
// buffer regardless of the peer, so no second clock is needed after the
// budget ends.
func (r *Request) Budget(d time.Duration) (context.Context, context.CancelFunc) {
	if wd, ok := r.t.(writeDeadliner); ok {
		if err := wd.SetWriteDeadline(time.Time{}); err != nil {
			logrus.WithError(err).WithField("request_id", r.RequestID()).Debug("route budget: connection write deadline could not be lifted")
		}
	}
	return context.WithTimeout(r.Request.Context(), d)
}

func (r *Request) AbortJSON(code int, msg string) {
	r.logRefusal(code, nil, msg)
	response := api.SimpleErrorResponse(code, msg)
	response.Error.RequestID = r.RequestID()
	r.t.AbortJSON(code, response)
}

func (r *Request) ErrorJSON(code int, msg string) {
	r.logRefusal(code, nil, msg)
	response := api.SimpleErrorResponse(code, msg)
	response.Error.RequestID = r.RequestID()
	r.t.WriteJSON(code, response)
}

// logRefusal records a response the handler itself chose. A 4xx is the
// contract answering as designed — not found, conflict, precondition failed,
// refused input — so it is logged at info; only a 5xx is an error an operator
// must act on. Before this, every expected refusal (a get-or-create probe's
// 404, an idempotent ensure's 412) reached the log as an error.
func (r *Request) logRefusal(code int, fields logrus.Fields, msg string) {
	entry := logrus.WithFields(fields).WithFields(logrus.Fields{"status": code, "request_id": r.RequestID()})
	if code >= http.StatusInternalServerError {
		entry.Error(msg)
		return
	}
	entry.Info(msg)
}

// InternalError answers 500 with a STABLE, non-leaky msg and logs the cause
// verbatim against the request id.
//
// ErrorJSON(500, "...") drops whatever error the handler was holding, so an
// internal failure reaches the operator as a bare constant. upstream#1627: three
// boot-time `set billing policy failed` lines and a 500 on every billed
// admission carried no cause at all, and attributing them cost a bisect across
// two standing stacks. Every 500 that has an error in hand should use this.
func (r *Request) InternalError(msg string, cause error) {
	// A saturated database pool is a retryable 503, never a 500 (#1105).
	var refusal *apperr.Error
	if errors.As(cause, &refusal) && refusal.Status == http.StatusServiceUnavailable {
		r.APIError(api.NewAPIError(refusal.Status, api.ErrorTypeForStatus(refusal.Status), refusal.Code, refusal.Message))
		return
	}
	requestID := r.RequestID()
	logrus.WithError(cause).WithField("request_id", requestID).Error(msg)
	response := api.SimpleErrorResponse(http.StatusInternalServerError, msg)
	response.Error.RequestID = requestID
	r.t.WriteJSON(http.StatusInternalServerError, response)
}

// AbortGate answers a Gate or authenticator refusal with its code.
func (r *Request) AbortGate(err error) {
	var refusal billingauth.GateError
	if !errors.As(err, &refusal) {
		r.AbortAPIError(api.Coded(billing.CodeInternalError, "authorization unavailable"))
		return
	}
	if refusal.Code == billing.CodeSenderProofRequired {
		r.SetHeader("WWW-Authenticate", `DPoP error="invalid_dpop_proof", algs="ES256"`)
	}
	r.AbortAPIError(billingauth.RefusalError(refusal))
}

// AbortCode stops the chain with a registered error code; an empty message
// answers the code's meaning.
func (r *Request) AbortCode(code, message string) {
	r.AbortAPIError(api.Coded(code, message))
}

// ErrorCode answers a registered error code; an empty message answers the
// code's meaning.
func (r *Request) ErrorCode(code, message string) {
	r.APIError(api.Coded(code, message))
}

// AbortAPIError is APIError that also stops the middleware chain.
func (r *Request) AbortAPIError(err *api.APIError) {
	err.WithRequestID(r.RequestID())
	r.logRefusal(err.HTTPStatus, logrus.Fields{"type": err.Type, "code": err.Code, "param": err.Param}, err.Message)
	r.t.AbortJSON(err.HTTPStatus, err.ToResponse())
}

func (r *Request) APIError(err *api.APIError) {
	err.WithRequestID(r.RequestID())
	r.logRefusal(err.HTTPStatus, logrus.Fields{"type": err.Type, "code": err.Code, "param": err.Param}, err.Message)
	r.t.WriteJSON(err.HTTPStatus, err.ToResponse())
}

const maxRequestIDLength = 128

// EnsureRequestID returns a bounded request correlation identifier and stores
// it on the HTTP request so middleware and handlers share one value.
func EnsureRequestID(r *http.Request) string {
	requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
	if requestID == "" || len(requestID) > maxRequestIDLength {
		requestID = uuid.NewString()
	}
	r.Header.Set("X-Request-ID", requestID)
	return requestID
}

// RequestID returns the request's correlation identifier, generating one when
// the caller did not supply a usable value. The same value is propagated to
// the request and response headers so handler logs and API errors correlate.
func (r *Request) RequestID() string {
	if r.requestID != "" {
		return r.requestID
	}
	requestID := EnsureRequestID(r.Request)
	r.requestID = requestID
	r.SetHeader("X-Request-ID", requestID)
	return requestID
}

func (r *Request) SuccessJSON(data any) {
	r.t.WriteJSON(http.StatusOK, data)
}

func (r *Request) SuccessJSONMessage(msg string) {
	r.t.WriteJSON(http.StatusOK, map[string]any{
		"message": msg,
	})
}

func (r *Request) SuccessJSONPaginated(data any, total int64, limit, offset int) {
	dataLen := 0
	if slice, ok := data.([]any); ok {
		dataLen = len(slice)
	} else {
		v := reflect.ValueOf(data)
		if v.Kind() == reflect.Slice {
			dataLen = v.Len()
		}
	}
	hasMore := int64(offset+dataLen) < total

	urlPath := ""
	if r.Request != nil {
		urlPath = r.Request.URL.Path
	}
	r.t.WriteJSON(http.StatusOK, map[string]any{
		"object":   "list",
		"data":     data,
		"total":    total,
		"limit":    limit,
		"offset":   offset,
		"has_more": hasMore,
		"url":      urlPath,
	})
}

func (r *Request) Bind(data any) error {
	return r.t.Bind(data)
}

// BindJSON is the one request-body decoder: exactly one JSON value, no field
// the target does not declare, then the target's binding rules. A refusal is
// answered here (unknown_field with param, unsupported_media_type,
// request_body_too_large, or invalid_param) and BindJSON returns false.
func (r *Request) BindJSON(data any) bool {
	if err := r.t.BindJSON(data); err != nil {
		r.APIError(BindError(err))
		return false
	}
	return true
}

// BindOptionalJSON is BindJSON for a route whose body may be absent: an empty
// body leaves data untouched.
func (r *Request) BindOptionalJSON(data any) bool {
	err := r.t.BindJSON(data)
	if err == nil || errors.Is(err, io.EOF) {
		return true
	}
	r.APIError(BindError(err))
	return false
}

// DecodeJSON is BindJSON for a handler that answers a refusal itself: the
// error is the refusal BindJSON would have written.
func (r *Request) DecodeJSON(data any) error {
	if err := r.t.BindJSON(data); err != nil {
		return BindError(err)
	}
	return nil
}

// Page reads a list route's ?limit= and ?cursor=: the page size
// (billing.DefaultPageLimit when absent) and the opaque cursor a previous
// page returned. A limit that is not an integer in 1..billing.MaxPageLimit is
// answered 400 invalid_query and Page returns false.
func (r *Request) Page() (billing.PageRequest, bool) {
	page := billing.PageRequest{Cursor: r.Query("cursor"), Limit: billing.DefaultPageLimit}
	if raw := r.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > billing.MaxPageLimit {
			r.APIError(api.Coded(billing.CodeInvalidQuery, "limit is invalid").WithParam("limit"))
			return billing.PageRequest{}, false
		}
		page.Limit = n
	}
	return page, true
}

// BindQuery reads the query string into data's `form` fields. A value that
// does not parse as its field's type is 400 invalid_query on that parameter.
func (r *Request) BindQuery(data any) bool {
	if err := r.t.BindQuery(data); err != nil {
		r.APIError(QueryError(err))
		return false
	}
	return true
}

func (r *Request) BindURI(data any) bool {
	if err := r.t.BindURI(data); err != nil {
		r.ErrorJSON(http.StatusBadRequest, normaliseBindError(err))
		return false
	}
	return true
}

// JSON writes a JSON response with the given status code.
func (r *Request) JSON(code int, body any) {
	r.t.WriteJSON(code, body)
}

// Status writes an HTTP status with no response body.
func (r *Request) Status(code int) {
	r.t.WriteJSON(code, nil)
}

// ShouldBindURI binds path parameters into data and returns the error WITHOUT
// writing a response.
func (r *Request) ShouldBindURI(data any) error {
	return r.t.BindURI(data)
}

// ShouldBindQuery binds query parameters into data and returns the error
// WITHOUT writing a response.
func (r *Request) ShouldBindQuery(data any) error {
	return r.t.BindQuery(data)
}

func (r *Request) Param(key string) string {
	return r.t.Param(key)
}

func (r *Request) Query(key string) string {
	return r.t.Query(key)
}

// Header returns a request header value, framework-neutral counterpart of the
// former r.GinCtx.GetHeader(...).
func (r *Request) Header(key string) string {
	return r.t.Header(key)
}

// SetHeader sets a response header (framework-neutral). Used e.g. for
// x-ratelimit-* and Retry-After on the admission endpoint (#298).
func (r *Request) SetHeader(key, value string) {
	r.t.SetHeader(key, value)
}

// UserContext returns the authenticated principal, framework-neutral counterpart
// of the former billingauth.UserContextFromGin(r.GinCtx). Works on both the gin
// and net/http backends.
func (r *Request) UserContext() (billingauth.UserContext, bool) {
	if r.ucSet {
		return r.uc, true
	}
	return r.t.UserContext()
}

// SetUserContext pins the authenticated principal on this request. The auth
// middleware calls it after Authenticate succeeds; handlers read it via
// UserContext()/GetUser(). It also propagates the principal into the request
// context (billingauth.SetUserContext) for any downstream that reads it from
// r.Request.Context() directly.
func (r *Request) SetUserContext(uc billingauth.UserContext) {
	r.uc = uc
	r.ucSet = true
	if r.Request != nil {
		r.Request = r.Request.WithContext(billingauth.SetUserContext(r.Request.Context(), uc))
	}
}

func (r *Request) Next() {
	r.t.Next()
}

func (r *Request) Get(key string) (any, bool) {
	return r.t.Get(key)
}

func (r *Request) MustGet(key string) any {
	v, ok := r.t.Get(key)
	if !ok {
		panic("request: key " + key + " does not exist")
	}
	return v
}

func (r *Request) Set(key string, value any) {
	r.t.Set(key, value)
}

func (r *Request) GetUser() *checkout.UserIdentity {
	if uc, ok := r.UserContext(); ok && uc.UserID != "" {
		user := &checkout.UserIdentity{
			ID:       uc.UserID,
			Username: uc.Username,
			Roles:    uc.Roles,
			ClientIP: r.ClientIP(),
		}
		if uc.EmailVerified && uc.Email != "" {
			email := uc.Email
			user.Email = &email
		}
		return user
	}

	user, ok := r.Get("user")
	if !ok {
		return nil
	}

	if ui, ok := user.(*checkout.UserIdentity); ok {
		if ui.ClientIP == "" {
			ui.ClientIP = r.ClientIP()
		}
		return ui
	}

	return nil
}

// ClientIP returns the resolved client IP for this request (#746): the raw
// socket peer, or — when that peer is inside a configured trusted_proxies
// CIDR — the first untrusted address found walking X-Forwarded-For
// right-to-left. Every consumer that needs "the client's IP" (rate limiting,
// abuse tracking, webhook IPAddress recording, the CCBill IP allowlist) MUST
// resolve through this (or the same Runtime.TrustedProxies resolver) so proxy
// trust is enforced identically everywhere. With no trusted_proxies
// configured this is exactly GetRemoteIP.
func (r *Request) ClientIP() string {
	if r.Request == nil {
		return ""
	}
	var resolver *iputil.TrustedProxies
	if r.State != nil {
		resolver = r.State.TrustedProxies
	}
	return resolver.ClientIP(r.Request)
}

// SecureTransport reports whether the client reached this request over HTTPS:
// TLS on this connection, or a trusted proxy's X-Forwarded-Proto.
func (r *Request) SecureTransport() bool {
	if r.Request == nil {
		return false
	}
	if r.Request.TLS != nil {
		return true
	}
	var resolver *iputil.TrustedProxies
	if r.State != nil {
		resolver = r.State.TrustedProxies
	}
	return resolver.ForwardedHTTPS(r.Request)
}

func (r *Request) GetRemoteIP() string {
	if r.Request == nil {
		return ""
	}
	ip, _, err := net.SplitHostPort(r.Request.RemoteAddr)
	if err != nil {
		return r.Request.RemoteAddr
	}
	return ip
}

func (r *Request) Redirect(code int, location string) {
	r.t.Redirect(code, location)
}

func (r *Request) FormValue(key string) string {
	return r.t.PostForm(key)
}

func (r *Request) FormFile(key string) (multipart.File, *multipart.FileHeader, error) {
	return r.t.FormFile(key)
}

func (r *Request) GetState() *app.Runtime {
	return r.State
}

// errTrailingJSON is a body holding more than one JSON value.
var errTrailingJSON = errors.New("request body must be one JSON value")

// errMediaType is a body sent as something other than JSON.
var errMediaType = errors.New("request body must be application/json")

// DecodeStrict decodes raw as exactly one JSON value into data and refuses a
// field data does not declare.
func DecodeStrict(raw []byte, data any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(data); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errTrailingJSON
	}
	return nil
}

// BindError is the refusal for a body BindJSON could not accept.
func BindError(err error) *api.APIError {
	var tooLarge *http.MaxBytesError
	var typeErr *json.UnmarshalTypeError
	var safe ClientSafeBindError
	switch {
	case errors.As(err, &tooLarge), isRequestBodyTooLarge(err):
		return api.Coded(billing.CodeRequestBodyTooLarge, "request body too large")
	case errors.Is(err, errMediaType):
		return api.Coded(billing.CodeUnsupportedMediaType, "")
	case errors.As(err, &safe):
		return api.Coded(billing.CodeInvalidParam, safe.ClientSafeBindMessage())
	case errors.Is(err, errTrailingJSON):
		return api.Coded(billing.CodeInvalidParam, errTrailingJSON.Error())
	case errors.As(err, &typeErr) && typeErr.Field != "":
		return api.Coded(billing.CodeInvalidParam, typeErr.Field+" is invalid").WithParam(typeErr.Field)
	}
	if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		field = strings.Trim(field, `"`)
		return api.Coded(billing.CodeUnknownField, "unknown field "+field).WithParam(field)
	}
	return api.Coded(billing.CodeInvalidParam, normaliseBindError(err))
}

// QueryError is the refusal for a query string BindQuery could not accept.
func QueryError(err error) *api.APIError {
	var field *fieldError
	if errors.As(err, &field) {
		return api.Coded(billing.CodeInvalidQuery, field.name+" is invalid").WithParam(field.name)
	}
	var verr validator.ValidationErrors
	if errors.As(err, &verr) && len(verr) > 0 {
		name := strings.ToLower(verr[0].Field())
		return api.Coded(billing.CodeInvalidQuery, name+" is invalid").WithParam(name)
	}
	return api.Coded(billing.CodeInvalidQuery, "")
}

// fieldError is a query or path value that does not parse as its field's type.
type fieldError struct {
	name string
	err  error
}

func (e *fieldError) Error() string { return e.name + ": " + e.err.Error() }
func (e *fieldError) Unwrap() error { return e.err }

// ClientSafeBindError is a decode error whose message is written FOR the
// caller — e.g. a retired wire key naming its replacement. Everything else
// collapses to "invalid_request": a decoder's own text can echo internal
// structure, so it is not a client-facing message by default.
type ClientSafeBindError interface {
	error
	ClientSafeBindMessage() string
}

func normaliseBindError(err error) string {
	var safe ClientSafeBindError
	if errors.As(err, &safe) {
		return safe.ClientSafeBindMessage()
	}
	var verr validator.ValidationErrors
	if errors.As(err, &verr) {
		if len(verr) > 0 {
			e := verr[0]
			return strings.ToLower(e.Field()) + " is invalid"
		}
	}
	if errors.Is(err, io.EOF) {
		return "empty_request_body"
	}
	return "invalid_request"
}

func isRequestBodyTooLarge(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "request body too large")
}

// --- net/http backend (the only backend since #670) ---

type httpTransport struct {
	w     http.ResponseWriter
	r     *http.Request
	kv    map[string]any
	wrote bool
}

func newHTTPTransport(w http.ResponseWriter, r *http.Request) *httpTransport {
	return &httpTransport{w: w, r: r, kv: map[string]any{}}
}

// SetWriteDeadline reaches the connection through net/http's
// ResponseController, which unwraps the middleware writers (statusWriter,
// captureWriter) via their Unwrap.
func (h *httpTransport) SetWriteDeadline(deadline time.Time) error {
	return http.NewResponseController(h.w).SetWriteDeadline(deadline)
}

func (h *httpTransport) WriteJSON(code int, body any) {
	if h.wrote {
		return
	}
	h.wrote = true
	if body == nil {
		h.w.WriteHeader(code)
		return
	}
	h.w.Header().Set("Content-Type", "application/json")
	h.w.WriteHeader(code)
	_ = json.NewEncoder(h.w).Encode(body)
}

func (h *httpTransport) AbortJSON(code int, body any) { h.WriteJSON(code, body) }

func (h *httpTransport) Bind(data any) error { return h.BindJSON(data) }

func (h *httpTransport) BindJSON(data any) error {
	raw, err := io.ReadAll(h.r.Body)
	if err != nil {
		return err
	}
	// The body may carry a card (#1129); decoded values are copies, so the
	// bytes read from the wire are wiped once decoding is done.
	defer clear(raw)
	if len(bytes.TrimSpace(raw)) == 0 {
		return io.EOF
	}
	if !jsonBody(h.r) {
		return errMediaType
	}
	if err := DecodeStrict(raw, data); err != nil {
		return err
	}
	return validateBinding(data)
}

// jsonBody reports whether the request declares a JSON body; a request that
// declares no media type is read as JSON.
func jsonBody(r *http.Request) bool {
	declared := r.Header.Get("Content-Type")
	if declared == "" {
		return true
	}
	mediaType, _, err := mime.ParseMediaType(declared)
	return err == nil && (mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"))
}

func (h *httpTransport) BindQuery(data any) error {
	if err := decodeTaggedValues(data, "form", func(k string) string { return h.r.URL.Query().Get(k) }); err != nil {
		return err
	}
	return validateBinding(data)
}

func (h *httpTransport) BindURI(data any) error {
	if err := decodeTaggedValues(data, "uri", func(k string) string { return h.r.PathValue(k) }); err != nil {
		return err
	}
	return validateBinding(data)
}

func (h *httpTransport) Param(key string) string     { return h.r.PathValue(key) }
func (h *httpTransport) Query(key string) string     { return h.r.URL.Query().Get(key) }
func (h *httpTransport) Get(key string) (any, bool)  { v, ok := h.kv[key]; return v, ok }
func (h *httpTransport) Set(key string, value any)   { h.kv[key] = value }
func (h *httpTransport) Next()                       {}
func (h *httpTransport) Header(key string) string    { return h.r.Header.Get(key) }
func (h *httpTransport) SetHeader(key, value string) { h.w.Header().Set(key, value) }
func (h *httpTransport) Redirect(code int, location string) {
	http.Redirect(h.w, h.r, location, code)
}
func (h *httpTransport) PostForm(key string) string { return h.r.PostFormValue(key) }
func (h *httpTransport) FormFile(key string) (multipart.File, *multipart.FileHeader, error) {
	return h.r.FormFile(key)
}
func (h *httpTransport) UserContext() (billingauth.UserContext, bool) {
	if h.r == nil {
		return billingauth.UserContext{}, false
	}
	return billingauth.FromContext(h.r.Context())
}

// bindingValidator matches gin's binding: it reads the `binding:"..."` struct
// tag with the go-playground/validator rule set gin uses, so net/http binding
// validates identically to the gin server.
var bindingValidator = func() *validator.Validate {
	v := validator.New()
	v.SetTagName("binding")
	return v
}()

func validateBinding(data any) error {
	if data == nil {
		return nil
	}
	rv := reflect.ValueOf(data)
	for rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil
	}
	return bindingValidator.Struct(data)
}

// decodeTaggedValues populates dst's fields from get(), matching them by the
// given struct tag (gin used "form" for query and "uri" for path params; the
// neutral binder keeps those tag conventions). Like gin's form binding it
// RECURSES into nested/embedded struct fields (e.g. query.QueryOptions[T]'s
// Filters), parses time.Time via the `time_format` tag (default RFC3339), and
// supports encoding.TextUnmarshaler fields (uuid.UUID etc.).
func decodeTaggedValues(dst any, tag string, get func(string) string) error {
	v := reflect.ValueOf(dst)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		return errors.New("bind target must be a non-nil pointer")
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return errors.New("bind target must be a struct pointer")
	}
	return decodeStructValues(v, tag, get)
}

var (
	timeType            = reflect.TypeOf(time.Time{})
	textUnmarshalerType = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
)

func decodeStructValues(v reflect.Value, tag string, get func(string) string) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		fv := v.Field(i)
		if !fv.CanSet() {
			continue
		}
		name := field.Tag.Get(tag)
		if idx := strings.IndexByte(name, ','); idx >= 0 {
			name = name[:idx]
		}

		// Recurse into struct-typed fields that are not directly bindable
		// (nested filter structs, embedded structs) — gin binding parity.
		ft := field.Type
		elem := ft
		if ft.Kind() == reflect.Ptr {
			elem = ft.Elem()
		}
		if elem.Kind() == reflect.Struct && elem != timeType && !reflect.PointerTo(elem).Implements(textUnmarshalerType) {
			target := fv
			if ft.Kind() == reflect.Ptr {
				if fv.IsNil() {
					fv.Set(reflect.New(elem))
				}
				target = fv.Elem()
			}
			if err := decodeStructValues(target, tag, get); err != nil {
				return err
			}
			continue
		}

		if name == "" || name == "-" {
			continue
		}
		raw := get(name)
		if raw == "" {
			continue
		}
		if err := setField(fv, raw, field.Tag); err != nil {
			return &fieldError{name: name, err: err}
		}
	}
	return nil
}

func setField(fv reflect.Value, raw string, tag reflect.StructTag) error {
	if !fv.CanSet() {
		return nil
	}
	// time.Time honors the `time_format` tag (gin convention); default RFC3339.
	if fv.Type() == timeType {
		layout := tag.Get("time_format")
		if layout == "" {
			layout = time.RFC3339
		}
		parsed, err := time.Parse(layout, raw)
		if err != nil {
			return err
		}
		fv.Set(reflect.ValueOf(parsed))
		return nil
	}
	// encoding.TextUnmarshaler (uuid.UUID etc.), matching gin's trySetCustom.
	if fv.CanAddr() {
		if tu, ok := fv.Addr().Interface().(encoding.TextUnmarshaler); ok {
			return tu.UnmarshalText([]byte(raw))
		}
	}
	switch fv.Kind() {
	case reflect.String:
		fv.SetString(raw)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return err
		}
		fv.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return err
		}
		fv.SetUint(n)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return err
		}
		fv.SetBool(b)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return err
		}
		fv.SetFloat(f)
	case reflect.Ptr:
		if fv.IsNil() {
			fv.Set(reflect.New(fv.Type().Elem()))
		}
		return setField(fv.Elem(), raw, tag)
	default:
		return fmt.Errorf("unsupported field kind %s", fv.Kind())
	}
	return nil
}
