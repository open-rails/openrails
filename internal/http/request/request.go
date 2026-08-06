package request

import (
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/shared/iputil"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/billingauth"
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
		Clock:   clock,
		t:       t,
	}
}

// NewHTTP builds a net/http-backed Request (embedded surface) — no gin.
func NewHTTP(w http.ResponseWriter, r *http.Request, runtime *app.Runtime) *Request {
	return NewWithTransport(runtime, r, newHTTPTransport(w, r))
}

func (r *Request) AbortJSON(code int, msg string) {
	logrus.Error(msg)
	r.t.AbortJSON(code, api.SimpleErrorResponse(code, msg))
}

func (r *Request) ErrorJSON(code int, msg string) {
	logrus.Error(msg)
	r.t.WriteJSON(code, api.SimpleErrorResponse(code, msg))
}

// InternalError answers 500 with a STABLE, non-leaky msg and logs the cause
// verbatim against the request id.
//
// ErrorJSON(500, "...") drops whatever error the handler was holding, so an
// internal failure reaches the operator as a bare constant. th#1627: three
// boot-time `set billing policy failed` lines and a 500 on every billed
// admission carried no cause at all, and attributing them cost a bisect across
// two standing stacks. Every 500 that has an error in hand should use this.
func (r *Request) InternalError(msg string, cause error) {
	requestID := r.RequestID()
	logrus.WithError(cause).WithField("request_id", requestID).Error(msg)
	r.t.WriteJSON(http.StatusInternalServerError, api.SimpleErrorResponse(http.StatusInternalServerError, msg))
}

func (r *Request) APIError(err *api.APIError) {
	requestID := r.RequestID()
	err.WithRequestID(requestID)
	logrus.WithFields(logrus.Fields{
		"type":       err.Type,
		"code":       err.Code,
		"param":      err.Param,
		"request_id": requestID,
		"status":     err.HTTPStatus,
	}).Error(err.Message)
	r.t.WriteJSON(err.HTTPStatus, err.ToResponse())
}

const maxErrorRequestIDLength = 128

// RequestID returns the request's correlation identifier, generating one when
// the caller did not supply a usable value. The same value is propagated to
// the request and response headers so handler logs and API errors correlate.
func (r *Request) RequestID() string {
	if r.requestID != "" {
		return r.requestID
	}
	requestID := strings.TrimSpace(r.Request.Header.Get("X-Request-ID"))
	if requestID == "" || len(requestID) > maxErrorRequestIDLength {
		requestID = uuid.NewString()
	}
	r.requestID = requestID
	r.Request.Header.Set("X-Request-ID", requestID)
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

func (r *Request) BindJSON(data any) bool {
	if err := r.t.BindJSON(data); err != nil {
		if isRequestBodyTooLarge(err) {
			r.ErrorJSON(http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		r.ErrorJSON(http.StatusBadRequest, normaliseBindError(err))
		return false
	}
	return true
}

func (r *Request) BindQuery(data any) bool {
	if err := r.t.BindQuery(data); err != nil {
		r.ErrorJSON(http.StatusBadRequest, normaliseBindError(err))
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
		}
		if uc.Email != "" {
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
	return resolver.ResolveClientIP(r.Request.RemoteAddr, r.t.Header("X-Forwarded-For"))
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
	if strings.Contains(err.Error(), "EOF") {
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

func (h *httpTransport) WriteJSON(code int, body any) {
	if h.wrote {
		return
	}
	h.wrote = true
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
	if err := json.Unmarshal(raw, data); err != nil {
		return err
	}
	return validateBinding(data)
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
			return fmt.Errorf("%s: %w", field.Name, err)
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
