package request

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/customerscope"
	"github.com/open-rails/openrails/internal/modules/checkout"
)

type retiredKeyError struct{}

func (retiredKeyError) Error() string                 { return "decoder internals" }
func (retiredKeyError) ClientSafeBindMessage() string { return "role_id was removed: use scope_key" }

type retiredKeyBody struct{}

func (*retiredKeyBody) UnmarshalJSON([]byte) error { return retiredKeyError{} }

func newReq(method, target, body string) (*Request, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	return NewHTTP(rec, r, nil), rec
}

func errorBody(t *testing.T, rec *httptest.ResponseRecorder) api.ErrorResponse {
	t.Helper()
	var body api.ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return body
}

// The net/http binder must match gin's: binding tags validate, nested/inline
// structs recurse, time_format and TextUnmarshaler fields decode.
func TestBindingMatchesGinSemantics(t *testing.T) {
	type body struct {
		Name     string         `json:"name" binding:"required"`
		Metadata map[string]any `json:"metadata"`
	}
	req, _ := newReq(http.MethodPost, "/x", `{"name":"ada","metadata":{"provider_user_ref":"src"}}`)
	var b body
	require.NoError(t, req.t.BindJSON(&b))
	require.Equal(t, "src", b.Metadata["provider_user_ref"])

	type filters struct {
		UserID  string     `form:"user_id"`
		PriceID uuid.UUID  `form:"price_id"`
		After   *time.Time `form:"created_after" time_format:"2006-01-02"`
		Active  *bool      `form:"active"`
	}
	type opts struct {
		Limit   int     `form:"limit" binding:"max=100"`
		Filters filters `form:",inline"`
	}
	priceID := uuid.New()
	req, _ = newReq(http.MethodGet, "/x?limit=7&user_id=u-1&active=true&price_id="+priceID.String()+"&created_after=2026-01-02", "")
	var got opts
	require.NoError(t, req.ShouldBindQuery(&got))
	require.Equal(t, 7, got.Limit)
	require.Equal(t, "u-1", got.Filters.UserID)
	require.Equal(t, priceID, got.Filters.PriceID)
	require.Equal(t, "2026-01-02", got.Filters.After.Format("2006-01-02"))
	require.True(t, *got.Filters.Active)

	for _, query := range []string{"limit=abc", "limit=101", "price_id=not-a-uuid", "created_after=01/02/2026"} {
		req, _ = newReq(http.MethodGet, "/x?"+query, "")
		require.Error(t, req.ShouldBindQuery(&opts{}), query)
	}

	type uri struct {
		ID string `uri:"id" binding:"required"`
	}
	r := httptest.NewRequest(http.MethodGet, "/things/abc-123", nil)
	r.SetPathValue("id", "abc-123")
	req = NewHTTP(httptest.NewRecorder(), r, nil)
	var u uri
	require.NoError(t, req.ShouldBindURI(&u))
	require.Equal(t, "abc-123", u.ID)
	require.Equal(t, "abc-123", req.Param("id"))
	require.Error(t, NewHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/things/", nil), nil).ShouldBindURI(&uri{}))
}

// The one decoder: exactly one JSON value, no undeclared field, a coded and
// client-safe refusal (never decoder text).
func TestBindJSONIsStrict(t *testing.T) {
	type body struct {
		Name   string `json:"name" binding:"required"`
		Nested struct {
			Count int `json:"count"`
		} `json:"nested"`
	}
	for _, tc := range []struct {
		name, body, contentType string
		target                  any
		limit                   int64
		status                  int
		code, message, param    string
	}{
		{"empty body", "", "", &body{}, 0, 400, "invalid_param", "empty_request_body", ""},
		{"missing required", `{}`, "", &body{}, 0, 400, "invalid_param", "name is invalid", ""},
		{"malformed", `{"name":`, "", &body{}, 0, 400, "invalid_param", "invalid_request", ""},
		{"wrong type", `{"name":1}`, "", &body{}, 0, 400, "invalid_param", "name is invalid", "name"},
		{"unknown field", `{"name":"ada","nmae":"typo"}`, "", &body{}, 0, 400, "unknown_field", "unknown field nmae", "nmae"},
		{"unknown nested field", `{"name":"ada","nested":{"cuont":1}}`, "", &body{}, 0, 400, "unknown_field", "unknown field cuont", "cuont"},
		{"two values", `{"name":"ada"}{"name":"bob"}`, "", &body{}, 0, 400, "invalid_param", "request body must be one JSON value", ""},
		{"not json", `name=ada`, "application/x-www-form-urlencoded", &body{}, 0, 415, "unsupported_media_type", "", ""},
		{"client-safe decoder error", `{}`, "", &retiredKeyBody{}, 0, 400, "invalid_param", "role_id was removed: use scope_key", ""},
		{"too large", `{"name":"ada"}`, "", &body{}, 4, 413, "request_body_too_large", "request body too large", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(tc.body))
			if tc.contentType != "" {
				r.Header.Set("Content-Type", tc.contentType)
			}
			if tc.limit > 0 {
				r.Body = http.MaxBytesReader(rec, r.Body, tc.limit)
			}
			require.False(t, NewHTTP(rec, r, nil).BindJSON(tc.target))
			require.Equal(t, tc.status, rec.Code)
			got := errorBody(t, rec).Error
			require.Equal(t, tc.code, got.Code)
			if tc.message != "" {
				require.Equal(t, tc.message, got.Message)
			}
			if tc.param != "" {
				require.NotNil(t, got.Param)
				require.Equal(t, tc.param, *got.Param)
			}
		})
	}

	for _, contentType := range []string{"", "application/json", "application/json; charset=utf-8", "application/merge-patch+json"} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(` {"name":"ada","nested":{"count":2}} `))
		if contentType != "" {
			r.Header.Set("Content-Type", contentType)
		}
		var got body
		require.True(t, NewHTTP(rec, r, nil).BindJSON(&got), contentType)
		require.Equal(t, "ada", got.Name)
		require.Equal(t, 2, got.Nested.Count)
	}

	// An optional body may be absent, and is strict when present.
	var optional body
	rec := httptest.NewRecorder()
	require.True(t, NewHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil), nil).BindOptionalJSON(&optional))
	require.Empty(t, optional.Name)
	rec = httptest.NewRecorder()
	require.False(t, NewHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"name":"ada","extra":1}`)), nil).BindOptionalJSON(&optional))
	require.Equal(t, "unknown_field", errorBody(t, rec).Error.Code)

	// DecodeJSON hands the same refusal to a handler that answers it itself.
	err := NewHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"extra":1}`)), nil).DecodeJSON(&optional)
	var refusal *api.APIError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, "unknown_field", refusal.Code)
}

// A query value that does not parse as its field's type is refused, never
// read as the default.
func TestBindQueryRefusesMalformedValues(t *testing.T) {
	type query struct {
		Limit  int       `form:"limit"`
		Active *bool     `form:"active"`
		Since  time.Time `form:"since"`
		Name   string    `form:"name"`
	}
	for _, tc := range []struct{ raw, param string }{
		{"limit=ten", "limit"},
		{"active=maybe", "active"},
		{"since=yesterday", "since"},
	} {
		rec := httptest.NewRecorder()
		require.False(t, NewHTTP(rec, httptest.NewRequest(http.MethodGet, "/x?"+tc.raw, nil), nil).BindQuery(&query{}), tc.raw)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		got := errorBody(t, rec).Error
		require.Equal(t, "invalid_query", got.Code)
		require.NotNil(t, got.Param)
		require.Equal(t, tc.param, *got.Param)
	}
	var got query
	require.True(t, NewHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x?limit=5&active=true&name=ada&unknown=1", nil), nil).BindQuery(&got))
	require.Equal(t, 5, got.Limit)
	require.True(t, *got.Active)
	require.Equal(t, "ada", got.Name)
}

func TestResponsesAreWrittenOnceAndCorrelated(t *testing.T) {
	req, rec := newReq(http.MethodGet, "/x", "")
	req.SuccessJSON(map[string]any{"ok": true})
	req.ErrorCode(billing.CodeInternalError, "late")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	require.JSONEq(t, `{"ok":true}`, rec.Body.String())

	for _, status := range []int{http.StatusAccepted, http.StatusNoContent} {
		req, rec = newReq(http.MethodDelete, "/x", "")
		req.Status(status)
		require.Equal(t, status, rec.Code)
		require.Empty(t, rec.Body.String())
		require.Empty(t, rec.Header().Get("Content-Type"))
	}

	for _, tc := range []struct{ name, supplied, want string }{
		{"who id preserved", "req-149", "req-149"},
		{"missing id generated", "", ""},
		{"oversized id replaced", strings.Repeat("x", 129), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/checkout", nil)
			if tc.supplied != "" {
				r.Header.Set("X-Request-ID", tc.supplied)
			}
			NewHTTP(rec, r, nil).APIError(api.NewAPIError(http.StatusPaymentRequired, api.ErrorTypeCard, "card_declined", "declined"))
			id := errorBody(t, rec).Error.RequestID
			require.NotEmpty(t, id)
			require.LessOrEqual(t, len(id), 128)
			require.Equal(t, id, rec.Header().Get("X-Request-ID"))
			if tc.want != "" {
				require.Equal(t, tc.want, id)
			}
		})
	}
}

// A refusal the handler chose (4xx) is the contract answering and logs at
// info; only a 5xx logs as an error.
func TestRefusalLogLevel(t *testing.T) {
	hook := logtest.NewGlobal()
	t.Cleanup(hook.Reset)
	for _, tc := range []struct {
		write func(*Request)
		want  logrus.Level
		code  int
	}{
		{func(r *Request) { r.ErrorCode("product_not_found", "") }, logrus.InfoLevel, 404},
		{func(r *Request) { r.AbortCode(billing.CodeResourceConflict, "conflict") }, logrus.InfoLevel, 409},
		{func(r *Request) {
			r.APIError(api.NewAPIError(http.StatusPaymentRequired, api.ErrorTypeCard, "card_declined", "declined"))
		}, logrus.InfoLevel, 402},
		{func(r *Request) { r.ErrorCode(billing.CodeInternalError, "boom") }, logrus.ErrorLevel, 500},
		{func(r *Request) {
			r.APIError(api.NewAPIError(http.StatusServiceUnavailable, api.ErrorTypeAPI, "service_unavailable", "down"))
		}, logrus.ErrorLevel, 503},
	} {
		hook.Reset()
		req, _ := newReq(http.MethodGet, "/x", "")
		tc.write(req)
		entry := hook.LastEntry()
		require.NotNil(t, entry)
		require.Equal(t, tc.want, entry.Level, tc.code)
		require.Equal(t, tc.code, entry.Data["status"])
		require.NotEmpty(t, entry.Data["request_id"])
	}
}

// The customer a handler sees is the route gate's scope, with the who's
// verified email only; nothing else (a pinned user, a "user" value) is a
// customer.
func TestGetUserIsTheCustomerScope(t *testing.T) {
	req, _ := newReq(http.MethodGet, "/x", "")
	require.Nil(t, req.GetUser())
	req.SetUserContext(billingauth.UserContext{UserID: "11111111-1111-4111-8111-111111111111"})
	req.Set("user", &checkout.UserIdentity{ID: "u-3"})
	require.Nil(t, req.GetUser(), "a control-plane user or a stray value is no customer")

	customer := billing.CustomerID(uuid.MustParse("22222222-2222-4222-8222-222222222222"))
	ctx := customerscope.Bind(context.Background(), billing.MerchantID(uuid.New()), customer, customer.String(), true)
	for verified, want := range map[bool]*string{true: new("ada@example.test"), false: nil} {
		who := billingauth.Identity{Subject: customer.String(), SubjectKind: billingauth.SubjectUser, Username: "ada", Email: "ada@example.test", EmailVerified: verified}
		req.Request = req.Request.WithContext(billingauth.BindIdentity(ctx, who))
		u := req.GetUser()
		require.NotNil(t, u)
		require.Equal(t, customer.String(), u.ID)
		require.Equal(t, "ada", u.Username)
		require.Equal(t, want, u.Email)
	}
}

// Without configured trusted proxies a spoofed X-Forwarded-For never changes
// the client identity that rate limits and abuse tracking key on.
func TestClientIPIgnoresForwardedForWithoutTrustedProxies(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.RemoteAddr = "203.0.113.9:4444"
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	req := NewHTTP(httptest.NewRecorder(), r, nil)
	require.Equal(t, "203.0.113.9", req.ClientIP())
	require.Equal(t, "203.0.113.9", req.GetRemoteIP())
}
