package request

import (
	"bytes"
	"cmp"
	"context"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"

	"github.com/jonboulle/clockwork"
	"golang.org/x/text/language"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/customerscope"
	"github.com/open-rails/openrails/internal/http/wireform"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/modules/payment"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/iputil"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// Request is the HTTP request a route handler answers and its response
// writer.
type Request struct {
	State   *app.Runtime
	Request *http.Request
	Clock   clockwork.Clock

	w         http.ResponseWriter
	wrote     bool
	requestID string
	Log       *slog.Logger
}

// New builds the Request a route handler answers.
func New(logger *slog.Logger, w http.ResponseWriter, r *http.Request, runtime *app.Runtime) *Request {
	var clock clockwork.Clock
	if runtime != nil {
		clock = runtime.Clock
	}
	return &Request{
		State:   runtime,
		Request: r,
		Clock:   timeutil.FirstClock(clock),
		w:       w,
		Log:     logger,
	}
}

// Budget bounds the handler's own work by d, the route's declared budget, and
// lifts the connection's write deadline for the duration. A server-level
// WriteTimeout is set before the handler knows its work, so a slow provider
// write could commit while the client gets EOF; only the route knows its
// budget. Response bodies are small JSON, so no second clock is needed after
// the budget ends.
func (r *Request) Budget(d time.Duration) (context.Context, context.CancelFunc) {
	// ResponseController unwraps the middleware writers via their Unwrap.
	if err := http.NewResponseController(r.w).SetWriteDeadline(time.Time{}); err != nil {
		r.Log.DebugContext(r.Request.Context(), "route budget: connection write deadline could not be lifted", logx.Err(err), "request_id", r.RequestID())
	}
	return context.WithTimeout(r.Request.Context(), d)
}

// logRefusal records a response the handler itself chose. A 4xx is the
// contract answering as designed and logs at info; only a 5xx is an error an
// operator must act on.
func (r *Request) logRefusal(code int, fields []any, msg string) {
	entry := r.Log.With(fields...).With("status", code, "request_id", r.RequestID())
	if code >= http.StatusInternalServerError {
		entry.ErrorContext(r.Request.Context(), msg)
		return
	}
	entry.InfoContext(r.Request.Context(), msg)
}

// InternalError answers 500 with a STABLE, non-leaky msg and logs the cause
// verbatim against the request id. Every 500 that has an error in hand uses
// this, so an internal failure never reaches the operator as a bare constant.
func (r *Request) InternalError(msg string, cause error) {
	// A saturated database pool is a retryable 503, never a 500.
	var refusal *apperr.Error
	if errors.As(cause, &refusal) && refusal.Status == http.StatusServiceUnavailable {
		r.APIError(api.NewAPIError(refusal.Status, api.ErrorTypeForStatus(refusal.Status), refusal.Code, refusal.Message))
		return
	}
	requestID := r.RequestID()
	r.Log.ErrorContext(r.Request.Context(), msg, logx.Err(cause), "request_id", requestID)
	response := api.SimpleErrorResponse(http.StatusInternalServerError, msg)
	response.Error.RequestID = requestID
	r.writeJSON(http.StatusInternalServerError, response)
}

// Through runs net/http middleware (the host's auth) inside the route chain:
// next sees the request and writer the middleware passed on, and does not
// run when the middleware answers the request itself.
func (r *Request) Through(mw func(http.Handler) http.Handler, next func(*Request)) {
	if mw == nil {
		r.InternalError("auth middleware unavailable", errors.New("request: Through needs a middleware"))
		return
	}
	outer := r.w
	mw(http.HandlerFunc(func(w http.ResponseWriter, passed *http.Request) {
		if passed != nil {
			r.Request = passed
		}
		if w != nil {
			r.w = w
			defer func() { r.w = outer }()
		}
		next(r)
	})).ServeHTTP(outer, r.Request)
}

// FromHTTP adapts a plain net/http handler to a route handler, for a route
// whose handler an assembly builds outside the handlers package.
func FromHTTP(h http.Handler) func(*Request) {
	return func(r *Request) {
		r.wrote = true
		h.ServeHTTP(r.w, r.Request)
	}
}

// AbortCode stops the chain with a registered error code; an empty message
// answers the code's meaning.
func (r *Request) AbortCode(code billing.ErrorCode, message string) {
	r.AbortAPIError(api.Coded(code, message))
}

// ErrorCode answers a registered error code; an empty message answers the
// code's meaning.
func (r *Request) ErrorCode(code billing.ErrorCode, message string) {
	r.APIError(api.Coded(code, message))
}

// AbortAPIError is APIError that also stops the middleware chain.
func (r *Request) AbortAPIError(err *api.APIError) {
	err.WithRequestID(r.RequestID())
	r.logRefusal(err.HTTPStatus, []any{"type", err.Type, "code", err.Code, "param", paramValue(err.Param)}, err.Message)
	r.writeAPIError(err)
}

// paramValue logs a refusal's param, not its pointer.
func paramValue(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func (r *Request) APIError(err *api.APIError) {
	err.WithRequestID(r.RequestID())
	fields := []any{"type", err.Type, "code", err.Code, "param", paramValue(err.Param)}
	if cause := err.Cause(); cause != nil {
		fields = append(fields, logx.Err(cause))
	}
	r.logRefusal(err.HTTPStatus, fields, err.Message)
	r.writeAPIError(err)
}

// writeAPIError answers err, logging how it departs from the error registry.
func (r *Request) writeAPIError(err *api.APIError) {
	if violation := err.Violation(); violation != "" {
		r.Log.ErrorContext(r.Request.Context(), violation, "request_id", r.RequestID())
	}
	r.writeJSON(err.HTTPStatus, err.ToResponse())
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
	r.writeJSON(http.StatusOK, data)
}

// BindJSON is the one request-body decoder: exactly one JSON value, no field
// the target does not declare, then the target's binding rules. A refusal is
// answered here (unknown_field with param, unsupported_media_type,
// request_body_too_large, or invalid_param) and BindJSON returns false.
func (r *Request) BindJSON(data any) bool {
	if err := r.decodeBody(data); err != nil {
		r.APIError(BindError(err))
		return false
	}
	return true
}

// BindOptionalJSON is BindJSON for a route whose body may be absent: an empty
// body leaves data untouched.
func (r *Request) BindOptionalJSON(data any) bool {
	err := r.decodeBody(data)
	if err == nil || errors.Is(err, io.EOF) {
		return true
	}
	r.APIError(BindError(err))
	return false
}

// PeekJSON decodes the JSON body into data and leaves the body for the
// handler's BindJSON. Middleware uses it to size a request before it runs.
func (r *Request) PeekJSON(data any) error {
	if r.Request.Body == nil {
		return io.EOF
	}
	raw, err := io.ReadAll(r.Request.Body)
	if err != nil {
		return err
	}
	r.Request.Body = io.NopCloser(bytes.NewReader(raw))
	return json.Unmarshal(raw, data)
}

// Page reads a list route's ?limit= and ?cursor=: the page size
// (billing.DefaultPageLimit when absent) and the opaque cursor a previous
// page returned. A limit that is not an integer in 1..billing.MaxPageLimit is
// answered 400 invalid_query and Page returns false.
func (r *Request) Page() (billing.PageRequest, bool) { return r.PageOf(pagination.Standard) }

// PageOf is Page for a list of its own size.
func (r *Request) PageOf(size pagination.Size) (billing.PageRequest, bool) {
	page := billing.PageRequest{Cursor: r.Query("cursor"), Limit: size.Default}
	if raw := r.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > size.Max {
			r.APIError(api.Coded(billing.CodeInvalidQuery, "limit is invalid").WithParam("limit"))
			return billing.PageRequest{}, false
		}
		page.Limit = n
	}
	return page, true
}

// BindQuery reads the query string into data's `form` fields, as the route
// declares them (wireform). A value that does not parse as its field's type
// is 400 invalid_query on that parameter.
func (r *Request) BindQuery(data any) bool {
	if err := wireform.Decode(r.Request.URL.Query(), data); err != nil {
		r.APIError(QueryError(err))
		return false
	}
	return true
}

// JSON writes a JSON response with the given status code.
func (r *Request) JSON(code int, body any) {
	r.writeJSON(code, body)
}

// Status writes an HTTP status with no response body.
func (r *Request) Status(code int) {
	r.writeJSON(code, nil)
}

// NoContent answers 204: a DELETE that removed its resource.
func (r *Request) NoContent() { r.Status(http.StatusNoContent) }

// ShouldBindURI binds path parameters into data and returns the error WITHOUT
// writing a response.
func (r *Request) ShouldBindURI(data any) error {
	if err := decodeTaggedValues(data, "uri", r.Request.PathValue); err != nil {
		return err
	}
	return validateBinding(data)
}

func (r *Request) Param(key string) string { return r.Request.PathValue(key) }

func (r *Request) Query(key string) string { return r.Request.URL.Query().Get(key) }

// Header returns a request header value.
func (r *Request) Header(key string) string { return r.Request.Header.Get(key) }

// SetHeader sets a response header.
func (r *Request) SetHeader(key, value string) { r.w.Header().Set(key, value) }

// GetUser is a customer route's admitted customer, as the purchase services
// take it: from the route gate only, never the request's path, query or body.
// Nil anywhere else.
func (r *Request) GetUser() *payment.UserIdentity {
	scope, ok := r.CustomerScope()
	if !ok {
		return nil
	}
	c, _ := billingauth.IdentityFromContext(r.Request.Context())
	user := &payment.UserIdentity{ID: scope.Customer().String(), Username: c.Username, ClientIP: r.ClientIP()}
	if c.EmailVerified && c.Email != "" {
		email := c.Email
		user.Email = &email
	}
	return user
}

// CustomerScope is a customer route's acting customer, as the route gate
// bound it.
func (r *Request) CustomerScope() (customerscope.Scope, bool) {
	if r == nil || r.Request == nil {
		return customerscope.Scope{}, false
	}
	return customerscope.From(r.Request.Context())
}

// Staff is the staff identity a merchant route admitted, as the route gate
// bound it.
func (r *Request) Staff() (billingauth.Staff, bool) {
	if r == nil || r.Request == nil {
		return billingauth.Staff{}, false
	}
	return billingauth.StaffFromContext(r.Request.Context())
}

// ClientCountry is the client's country as a trusted proxy reports it, or "".
func (r *Request) ClientCountry() string {
	if r.Request == nil || r.State == nil {
		return ""
	}
	return r.State.TrustedProxies.Country(r.Request)
}

// ClientLanguage is the language the request's Accept-Language prefers, or "".
func (r *Request) ClientLanguage() string {
	if r.Request == nil {
		return ""
	}
	return PreferredLanguage(r.Request.Header.Get("Accept-Language"))
}

// PreferredLanguage is an Accept-Language header's first language by weight,
// as language[-region] (en, pt-BR, es-419), or "" when it names none. An
// entry that does not parse is skipped, as are "*" and the tags that name no
// one language (mul, und, zxx).
func PreferredLanguage(header string) string {
	if header == "" || len(header) > 1024 {
		return ""
	}
	type entry struct {
		tag    language.Tag
		weight float64
	}
	var entries []entry
	for _, part := range strings.Split(header, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		weight := 1.0
		if q, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
			w, err := strconv.ParseFloat(strings.TrimSpace(q), 64)
			if err != nil || w < 0 || w > 1 {
				continue
			}
			weight = w
		}
		tag, err := language.Parse(strings.TrimSpace(name))
		if err != nil || weight == 0 {
			continue
		}
		base, confidence := tag.Base()
		if confidence == language.No {
			continue
		}
		switch base.String() {
		case "mul", "und", "zxx":
			continue
		}
		entries = append(entries, entry{tag, weight})
	}
	slices.SortStableFunc(entries, func(a, b entry) int { return cmp.Compare(b.weight, a.weight) })
	if len(entries) == 0 {
		return ""
	}
	base, _ := entries[0].tag.Base()
	out := base.String()
	if region, confidence := entries[0].tag.Region(); confidence == language.Exact {
		out += "-" + region.String()
	}
	return out
}

// ClientIP is the request's client IP: the socket peer, or, when that peer is
// inside a configured trusted_proxies CIDR, the first untrusted address
// walking X-Forwarded-For right to left. Rate limits, abuse tracking, webhook
// IP recording and the CCBill allowlist all resolve through it, so proxy trust
// is enforced identically everywhere.
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
		if cardguard.ContainsPAN(field) {
			// The name is a card number: never echo it.
			return api.Coded(billing.CodeUnknownField, "unknown field "+cardguard.Redacted)
		}
		return api.Coded(billing.CodeUnknownField, "unknown field "+field).WithParam(field)
	}
	return api.Coded(billing.CodeInvalidParam, normaliseBindError(err))
}

// QueryError is the refusal for a query string BindQuery could not accept.
func QueryError(err error) *api.APIError {
	var field *wireform.FieldError
	if errors.As(err, &field) {
		return api.Coded(billing.CodeInvalidQuery, field.Name+" is invalid").WithParam(field.Name)
	}
	return api.Coded(billing.CodeInvalidQuery, "")
}

// fieldError is a path value that does not parse as its field's type.
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

// writeJSON writes the response once; later writes are dropped.
func (r *Request) writeJSON(code int, body any) {
	if r.wrote {
		return
	}
	r.wrote = true
	if body == nil {
		r.w.WriteHeader(code)
		return
	}
	r.w.Header().Set("Content-Type", "application/json")
	r.w.WriteHeader(code)
	_ = json.NewEncoder(r.w).Encode(wireform.Of(body))
}

// decodeBody is BindJSON's decoder, returning the refusal's cause.
func (r *Request) decodeBody(data any) error {
	raw, err := io.ReadAll(r.Request.Body)
	if err != nil {
		return err
	}
	// The body may carry a card; decoded values are copies, so the bytes read
	// from the wire are wiped once decoding is done.
	defer clear(raw)
	if len(bytes.TrimSpace(raw)) == 0 {
		return io.EOF
	}
	if !jsonBody(r.Request) {
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

// bindingValidator reads the `binding:"..."` struct tag with the
// go-playground/validator rule set.
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

// decodeTaggedValues populates dst's fields from get(), matched by the given
// struct tag ("uri" for path). It recurses into nested and
// embedded struct fields, parses
// time.Time by its `time_format` tag (default RFC 3339), and decodes
// encoding.TextUnmarshaler fields (uuid.UUID etc.).
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
		// (nested filter structs, embedded structs).
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
	// time.Time honors the `time_format` tag; default RFC 3339.
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
	// encoding.TextUnmarshaler (uuid.UUID etc.).
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

// Record keeps a copy of the response r writes from now on, for a stored
// replay; the returned func stops recording and answers its status and body.
func (r *Request) Record() func() (int, []byte) {
	rec := &recorder{ResponseWriter: r.w}
	r.w = rec
	return func() (int, []byte) {
		r.w = rec.ResponseWriter
		return rec.status, rec.body.Bytes()
	}
}

type recorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *recorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *recorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.body.Write(b)
	return w.ResponseWriter.Write(b)
}
