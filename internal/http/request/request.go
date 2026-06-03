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

	"github.com/gin-gonic/gin"
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

// transport is the framework backend behind a Request. The handler-facing
// Request API is identical regardless of backend; only construction differs:
// New(ginCtx) for the standalone gin server, NewHTTP(w, r) for the embedded
// net/http surface. This is what lets the same handlers serve both without
// importing gin in the net/http path (#282).
type transport interface {
	writeJSON(code int, body any)
	abortJSON(code int, body any)
	bind(data any) error
	bindJSON(data any) error
	bindQuery(data any) error
	bindURI(data any) error
	param(key string) string
	query(key string) string
	get(key string) (any, bool)
	set(key string, value any)
	next()
	header(key string) string
	redirect(code int, location string)
	postForm(key string) string
	formFile(key string) (multipart.File, *multipart.FileHeader, error)
	userContext() (authprovider.UserContext, bool)
}

type Request struct {
	State   *app.Runtime
	Request *http.Request
	Clock   clockwork.Clock

	// GinCtx is set only on the gin backend (standalone). Embedded net/http
	// requests leave it nil. Route/middleware code that still reaches into it
	// is being migrated to Request methods (#282); handlers never touch it.
	GinCtx *gin.Context

	t transport
}

// New builds a gin-backed Request (standalone server).
func New(ctx *gin.Context, runtime *app.Runtime) *Request {
	return &Request{
		State:   runtime,
		Request: ctx.Request,
		Clock:   runtime.Clock,
		GinCtx:  ctx,
		t:       ginTransport{c: ctx},
	}
}

// NewHTTP builds a net/http-backed Request (embedded surface) — no gin.
func NewHTTP(w http.ResponseWriter, r *http.Request, runtime *app.Runtime) *Request {
	var clock clockwork.Clock
	if runtime != nil {
		clock = runtime.Clock
	}
	return &Request{
		State:   runtime,
		Request: r,
		Clock:   clock,
		t:       newHTTPTransport(w, r),
	}
}

func (r *Request) AbortJSON(code int, msg string) {
	logrus.Error(msg)
	r.t.abortJSON(code, api.SimpleErrorResponse(code, msg))
}

func (r *Request) ErrorJSON(code int, msg string) {
	logrus.Error(msg)
	r.t.writeJSON(code, api.SimpleErrorResponse(code, msg))
}

func (r *Request) APIError(err *api.APIError) {
	logrus.WithFields(logrus.Fields{
		"type":   err.Type,
		"code":   err.Code,
		"param":  err.Param,
		"status": err.HTTPStatus,
	}).Error(err.Message)
	r.t.writeJSON(err.HTTPStatus, err.ToResponse())
}

func (r *Request) SuccessJSON(data any) {
	r.t.writeJSON(http.StatusOK, data)
}

func (r *Request) SuccessJSONMessage(msg string) {
	r.t.writeJSON(http.StatusOK, message.Json{
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
	r.t.writeJSON(http.StatusOK, message.Json{
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
	return r.t.bind(data)
}

func (r *Request) BindJSON(data any) bool {
	if err := r.t.bindJSON(data); err != nil {
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
	if err := r.t.bindQuery(data); err != nil {
		r.ErrorJSON(http.StatusBadRequest, normaliseBindError(err))
		return false
	}
	return true
}

func (r *Request) BindURI(data any) bool {
	if err := r.t.bindURI(data); err != nil {
		r.ErrorJSON(http.StatusBadRequest, normaliseBindError(err))
		return false
	}
	return true
}

// JSON writes a JSON response with the given status code.
func (r *Request) JSON(code int, body any) {
	r.t.writeJSON(code, body)
}

// ShouldBindURI binds path parameters into data and returns the error WITHOUT
// writing a response.
func (r *Request) ShouldBindURI(data any) error {
	return r.t.bindURI(data)
}

// ShouldBindQuery binds query parameters into data and returns the error
// WITHOUT writing a response.
func (r *Request) ShouldBindQuery(data any) error {
	return r.t.bindQuery(data)
}

func (r *Request) Param(key string) string {
	return r.t.param(key)
}

func (r *Request) Query(key string) string {
	return r.t.query(key)
}

func (r *Request) Next() {
	r.t.next()
}

func (r *Request) Get(key string) (any, bool) {
	return r.t.get(key)
}

func (r *Request) MustGet(key string) any {
	v, ok := r.t.get(key)
	if !ok {
		panic("request: key " + key + " does not exist")
	}
	return v
}

func (r *Request) Set(key string, value any) {
	r.t.set(key, value)
}

func (r *Request) GetUser() *checkout.UserIdentity {
	if uc, ok := r.t.userContext(); ok && uc.UserID != "" {
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
	if forwarded := r.t.header("X-Forwarded-For"); forwarded != "" {
		ips := strings.Split(forwarded, ",")
		if len(ips) > 0 {
			return strings.TrimSpace(ips[0])
		}
	}

	if realIP := r.t.header("X-Real-IP"); realIP != "" {
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
	r.t.redirect(code, location)
}

func (r *Request) FormValue(key string) string {
	return r.t.postForm(key)
}

func (r *Request) FormFile(key string) (multipart.File, *multipart.FileHeader, error) {
	return r.t.formFile(key)
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

// --- gin backend (standalone) ---

type ginTransport struct{ c *gin.Context }

func (g ginTransport) writeJSON(code int, body any) { g.c.JSON(code, body) }
func (g ginTransport) abortJSON(code int, body any) { g.c.AbortWithStatusJSON(code, body) }
func (g ginTransport) bind(data any) error          { return g.c.Bind(data) }
func (g ginTransport) bindJSON(data any) error      { return g.c.ShouldBindJSON(data) }
func (g ginTransport) bindQuery(data any) error     { return g.c.ShouldBindQuery(data) }
func (g ginTransport) bindURI(data any) error       { return g.c.ShouldBindUri(data) }
func (g ginTransport) param(key string) string      { return g.c.Param(key) }
func (g ginTransport) query(key string) string      { return g.c.Query(key) }
func (g ginTransport) get(key string) (any, bool)   { return g.c.Get(key) }
func (g ginTransport) set(key string, value any)    { g.c.Set(key, value) }
func (g ginTransport) next()                        { g.c.Next() }
func (g ginTransport) header(key string) string     { return g.c.GetHeader(key) }
func (g ginTransport) redirect(code int, location string) {
	g.c.Redirect(code, location)
}
func (g ginTransport) postForm(key string) string { return g.c.PostForm(key) }
func (g ginTransport) formFile(key string) (multipart.File, *multipart.FileHeader, error) {
	return g.c.Request.FormFile(key)
}
func (g ginTransport) userContext() (authprovider.UserContext, bool) {
	return authprovider.UserContextFromGin(g.c)
}

// --- net/http backend (embedded, gin-free) ---

type httpTransport struct {
	w     http.ResponseWriter
	r     *http.Request
	kv    map[string]any
	wrote bool
}

func newHTTPTransport(w http.ResponseWriter, r *http.Request) *httpTransport {
	return &httpTransport{w: w, r: r, kv: map[string]any{}}
}

func (h *httpTransport) writeJSON(code int, body any) {
	if h.wrote {
		return
	}
	h.wrote = true
	h.w.Header().Set("Content-Type", "application/json")
	h.w.WriteHeader(code)
	_ = json.NewEncoder(h.w).Encode(body)
}

func (h *httpTransport) abortJSON(code int, body any) { h.writeJSON(code, body) }

func (h *httpTransport) bind(data any) error { return h.bindJSON(data) }

func (h *httpTransport) bindJSON(data any) error {
	if err := json.NewDecoder(h.r.Body).Decode(data); err != nil {
		return err
	}
	return validateBinding(data)
}

func (h *httpTransport) bindQuery(data any) error {
	if err := decodeTaggedValues(data, "form", func(k string) string { return h.r.URL.Query().Get(k) }); err != nil {
		return err
	}
	return validateBinding(data)
}

func (h *httpTransport) bindURI(data any) error {
	if err := decodeTaggedValues(data, "uri", func(k string) string { return h.r.PathValue(k) }); err != nil {
		return err
	}
	return validateBinding(data)
}

func (h *httpTransport) param(key string) string    { return h.r.PathValue(key) }
func (h *httpTransport) query(key string) string     { return h.r.URL.Query().Get(key) }
func (h *httpTransport) get(key string) (any, bool)  { v, ok := h.kv[key]; return v, ok }
func (h *httpTransport) set(key string, value any)   { h.kv[key] = value }
func (h *httpTransport) next()                        {}
func (h *httpTransport) header(key string) string     { return h.r.Header.Get(key) }
func (h *httpTransport) redirect(code int, location string) {
	http.Redirect(h.w, h.r, location, code)
}
func (h *httpTransport) postForm(key string) string { return h.r.PostFormValue(key) }
func (h *httpTransport) formFile(key string) (multipart.File, *multipart.FileHeader, error) {
	return h.r.FormFile(key)
}
func (h *httpTransport) userContext() (authprovider.UserContext, bool) {
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
