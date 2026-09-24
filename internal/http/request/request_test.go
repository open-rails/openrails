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

	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/billingauth"
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

// Bind failures answer with a stable client-safe message, never decoder text.
func TestBindFailuresAnswerWithSafeMessages(t *testing.T) {
	type body struct {
		Name string `json:"name" binding:"required"`
	}
	for _, tc := range []struct {
		name, body string
		target     any
		limit      int64
		status     int
		message    string
	}{
		{"empty body", "", &body{}, 0, 400, ""},
		{"missing required", `{}`, &body{}, 0, 400, "name is invalid"},
		{"malformed", `{"name":`, &body{}, 0, 400, "invalid_request"},
		{"wrong type", `{"name":1}`, &body{}, 0, 400, "invalid_request"},
		{"client-safe decoder error", `{}`, &retiredKeyBody{}, 0, 400, "role_id was removed: use scope_key"},
		{"too large", `{"name":"ada"}`, &body{}, 4, 413, "request body too large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(tc.body))
			if tc.limit > 0 {
				r.Body = http.MaxBytesReader(rec, r.Body, tc.limit)
			}
			require.False(t, NewHTTP(rec, r, nil).BindJSON(tc.target))
			require.Equal(t, tc.status, rec.Code)
			if tc.message != "" {
				require.Equal(t, tc.message, errorBody(t, rec).Error.Message)
			}
		})
	}
}

func TestResponsesAreWrittenOnceAndCorrelated(t *testing.T) {
	req, rec := newReq(http.MethodGet, "/x", "")
	req.SuccessJSON(map[string]any{"ok": true})
	req.ErrorJSON(http.StatusInternalServerError, "late")
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

	req, rec = newReq(http.MethodGet, "/items", "")
	req.SuccessJSONPaginated([]string{"a", "b"}, 5, 2, 2)
	var page map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Equal(t, true, page["has_more"])
	require.Equal(t, "/items", page["url"])
	req, rec = newReq(http.MethodGet, "/items", "")
	req.SuccessJSONPaginated([]string{"a"}, 5, 2, 4)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Equal(t, false, page["has_more"])

	for _, tc := range []struct{ name, supplied, want string }{
		{"caller id preserved", "req-149", "req-149"},
		{"missing id generated", "", ""},
		{"oversized id replaced", strings.Repeat("x", 129), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/checkout", nil)
			if tc.supplied != "" {
				r.Header.Set("X-Request-ID", tc.supplied)
			}
			NewHTTP(rec, r, nil).APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeCard, "card_declined", "declined"))
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
		{func(r *Request) { r.ErrorJSON(http.StatusNotFound, "product_not_found") }, logrus.InfoLevel, 404},
		{func(r *Request) { r.AbortJSON(http.StatusConflict, "conflict") }, logrus.InfoLevel, 409},
		{func(r *Request) {
			r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeCard, "card_declined", "declined"))
		}, logrus.InfoLevel, 400},
		{func(r *Request) { r.ErrorJSON(http.StatusInternalServerError, "boom") }, logrus.ErrorLevel, 500},
		{func(r *Request) {
			r.APIError(api.NewAPIError(http.StatusServiceUnavailable, api.ErrorTypeAPI, "unavailable", "down"))
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

// The principal pinned by auth middleware survives middleware replacing
// r.Request, and an unverified email never reaches handlers.
func TestPinnedUserContext(t *testing.T) {
	req, _ := newReq(http.MethodGet, "/x", "")
	req.SetUserContext(billingauth.UserContext{UserID: "u-1", Username: "ada", Email: "ada@example.test", EmailVerified: true, Roles: []string{"admin"}})
	req.Request = req.Request.WithContext(context.Background())
	u := req.GetUser()
	require.NotNil(t, u)
	require.Equal(t, "u-1", u.ID)
	require.Equal(t, "ada@example.test", *u.Email)
	uc, ok := billingauth.FromContext(req.Request.Context())
	require.False(t, ok, "context replaced by middleware: %+v", uc)
	_, ok = req.UserContext()
	require.True(t, ok)

	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r = r.WithContext(billingauth.SetUserContext(r.Context(), billingauth.UserContext{UserID: "u-2", Email: "x@example.test"}))
	u = NewHTTP(httptest.NewRecorder(), r, nil).GetUser()
	require.Equal(t, "u-2", u.ID)
	require.Nil(t, u.Email)

	req, _ = newReq(http.MethodGet, "/x", "")
	require.Nil(t, req.GetUser())
	fallback := &checkout.UserIdentity{ID: "u-3"}
	req.Set("user", fallback)
	require.Same(t, fallback, req.GetUser())
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
