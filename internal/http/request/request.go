package request

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/go-playground/validator/v10"
	"github.com/sirupsen/logrus"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/authprovider"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/message"
)

// Transport is the framework backend behind a Request. The handler-facing
// Request API is identical regardless of backend; only construction differs:
// ginreq.New(ginCtx) for the standalone gin server, NewHTTP(w, r) for the
// embedded net/http surface. This is what lets the same handlers serve both
// without importing gin in the net/http path (#282/#285). It is exported so the
// gin backend can live in the separate internal/http/request/ginreq package,
// keeping this package gin-free.
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
	UserContext() (authprovider.UserContext, bool)
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
	uc    authprovider.UserContext
	ucSet bool
}

// NewWithTransport builds a Request over an arbitrary Transport. The gin backend
// uses it from internal/http/request/ginreq; the net/http backend uses NewHTTP.
// Keeping construction transport-agnostic is what lets this package stay
// gin-free (#285).
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

func (r *Request) APIError(err *api.APIError) {
	logrus.WithFields(logrus.Fields{
		"type":   err.Type,
		"code":   err.Code,
		"param":  err.Param,
		"status": err.HTTPStatus,
	}).Error(err.Message)
	r.t.WriteJSON(err.HTTPStatus, err.ToResponse())
}

func (r *Request) SuccessJSON(data any) {
	r.t.WriteJSON(http.StatusOK, data)
}

func (r *Request) SuccessJSONMessage(msg string) {
	r.t.WriteJSON(http.StatusOK, message.Json{
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
	r.t.WriteJSON(http.StatusOK, message.Json{
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
// of the former authprovider.UserContextFromGin(r.GinCtx). Works on both the gin
// and net/http backends.
func (r *Request) UserContext() (authprovider.UserContext, bool) {
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
func (r *Request) SetUserContext(uc authprovider.UserContext) {
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

func (r *Request) GetClientIP() string {
	if forwarded := r.t.Header("X-Forwarded-For"); forwarded != "" {
		ips := strings.Split(forwarded, ",")
		if len(ips) > 0 {
			return strings.TrimSpace(ips[0])
		}
	}

	if realIP := r.t.Header("X-Real-IP"); realIP != "" {
		return strings.TrimSpace(realIP)
	}

	return r.GetRemoteIP()
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

func normaliseBindError(err error) string {
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

// --- net/http backend (embedded, gin-free) ---
//
// The gin backend (standalone) lives in internal/http/request/ginreq (#285), so
// this package imports no gin.

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
	if err := json.NewDecoder(h.r.Body).Decode(data); err != nil {
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
func (h *httpTransport) UserContext() (authprovider.UserContext, bool) {
	if h.r == nil {
		return authprovider.UserContext{}, false
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
// given struct tag (gin uses "form" for query and "uri" for path params).
func decodeTaggedValues(dst any, tag string, get func(string) string) error {
	v := reflect.ValueOf(dst)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		return errors.New("bind target must be a non-nil pointer")
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return errors.New("bind target must be a struct pointer")
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		name := field.Tag.Get(tag)
		if name == "" || name == "-" {
			continue
		}
		if idx := strings.IndexByte(name, ','); idx >= 0 {
			name = name[:idx]
		}
		raw := get(name)
		if raw == "" {
			continue
		}
		if err := setField(v.Field(i), raw); err != nil {
			return fmt.Errorf("%s: %w", field.Name, err)
		}
	}
	return nil
}

func setField(fv reflect.Value, raw string) error {
	if !fv.CanSet() {
		return nil
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
		return setField(fv.Elem(), raw)
	default:
		return fmt.Errorf("unsupported field kind %s", fv.Kind())
	}
	return nil
}
