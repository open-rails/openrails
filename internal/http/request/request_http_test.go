package request

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// These tests exercise the gin-free net/http backend (NewHTTP / httpTransport),
// the path that lets embedded hosts serve billing handlers without gin (#282).

func TestHTTPBindJSON(t *testing.T) {
	type body struct {
		Name string `json:"name" binding:"required"`
		Age  int    `json:"age"`
	}

	t.Run("valid body binds", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"name":"ada","age":36}`))
		req := NewHTTP(httptest.NewRecorder(), r, nil)
		var b body
		if err := req.t.BindJSON(&b); err != nil {
			t.Fatalf("bindJSON err: %v", err)
		}
		if b.Name != "ada" || b.Age != 36 {
			t.Fatalf("got %+v", b)
		}
	})

	t.Run("missing required field fails validation (binding tag)", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"age":1}`))
		req := NewHTTP(httptest.NewRecorder(), r, nil)
		var b body
		if err := req.t.BindJSON(&b); err == nil {
			t.Fatal("expected validation error for missing required name")
		}
	})

	t.Run("provider keys bind inside metadata maps", func(t *testing.T) {
		type bodyWithMetadata struct {
			Name     string         `json:"name" binding:"required"`
			Metadata map[string]any `json:"metadata"`
		}
		r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"name":"ada","metadata":{"provider_user_ref":"source-user"}}`))
		req := NewHTTP(httptest.NewRecorder(), r, nil)
		var b bodyWithMetadata
		if err := req.t.BindJSON(&b); err != nil {
			t.Fatalf("bindJSON err: %v", err)
		}
		if b.Metadata["provider_user_ref"] != "source-user" {
			t.Fatalf("metadata not bound: %#v", b.Metadata)
		}
	})
}

func TestHTTPBindURIAndQuery(t *testing.T) {
	type uriP struct {
		ID string `uri:"id" binding:"required"`
	}
	type queryP struct {
		Limit  int    `form:"limit"`
		Cursor string `form:"cursor"`
	}

	r := httptest.NewRequest(http.MethodGet, "/things/abc-123?limit=25&cursor=xyz", nil)
	r.SetPathValue("id", "abc-123")
	req := NewHTTP(httptest.NewRecorder(), r, nil)

	var u uriP
	if err := req.t.BindURI(&u); err != nil || u.ID != "abc-123" {
		t.Fatalf("bindURI got %+v err %v", u, err)
	}

	var q queryP
	if err := req.t.BindQuery(&q); err != nil || q.Limit != 25 || q.Cursor != "xyz" {
		t.Fatalf("bindQuery got %+v err %v", q, err)
	}

	if req.Param("id") != "abc-123" {
		t.Fatalf("Param(id) = %q", req.Param("id"))
	}
	if req.Query("limit") != "25" {
		t.Fatalf("Query(limit) = %q", req.Query("limit"))
	}
}

func TestHTTPSuccessJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	req := NewHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil), nil)
	req.SuccessJSON(map[string]any{"ok": true})

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type %q", ct)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out["ok"] != true {
		t.Fatalf("body %s err %v", rec.Body.String(), err)
	}
}

func TestHTTPStatusWritesNoBody(t *testing.T) {
	for _, status := range []int{http.StatusAccepted, http.StatusNoContent} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := NewHTTP(rec, httptest.NewRequest(http.MethodDelete, "/x", nil), nil)
			req.Status(status)

			require.Equal(t, status, rec.Code)
			require.Empty(t, rec.Body.String())
			require.Empty(t, rec.Header().Get("Content-Type"))
		})
	}
}

func TestHTTPAPIErrorIncludesRequestID(t *testing.T) {
	tests := []struct {
		name      string
		requestID string
	}{
		{name: "preserves caller request id", requestID: "req-149"},
		{name: "generates missing request id"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			httpReq := httptest.NewRequest(http.MethodPost, "/checkout", nil)
			if tt.requestID != "" {
				httpReq.Header.Set("X-Request-ID", tt.requestID)
			}
			req := NewHTTP(rec, httpReq, nil)
			req.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeCard, payments.FailureCardDeclined, "Your card was declined."))

			var body api.ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.NotEmpty(t, body.Error.RequestID)
			require.Equal(t, body.Error.RequestID, rec.Header().Get("X-Request-ID"))
			if tt.requestID != "" {
				require.Equal(t, tt.requestID, body.Error.RequestID)
			}
		})
	}
}

func TestHTTPGetUserFromContext(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r = r.WithContext(billingauth.SetUserContext(r.Context(), billingauth.UserContext{
		UserID: "u-1", Username: "ada", Email: "ada@example.test", EmailVerified: true, Roles: []string{"admin"},
	}))
	req := NewHTTP(httptest.NewRecorder(), r, nil)

	u := req.GetUser()
	if u == nil || u.ID != "u-1" || u.Username != "ada" || u.Email == nil || *u.Email != "ada@example.test" {
		t.Fatalf("GetUser = %+v", u)
	}

	unverified := httptest.NewRequest(http.MethodGet, "/x", nil)
	unverified = unverified.WithContext(billingauth.SetUserContext(unverified.Context(), billingauth.UserContext{
		UserID: "u-2", Email: "unverified@example.test", EmailVerified: false,
	}))
	got := NewHTTP(httptest.NewRecorder(), unverified, nil).GetUser()
	if got == nil || got.Email != nil {
		t.Fatalf("unverified GetUser = %+v", got)
	}
}

// TestHTTPBindQueryNestedFiltersTimeAndUUID pins gin-binding parity for the
// neutral query binder (#670): nested filter structs (query.QueryOptions[T]),
// time.Time with time_format tags, and TextUnmarshaler fields (uuid.UUID) all
// bind from the query string. Regression: the standalone /v1/merchant/payments
// user_id filter was silently dropped when the binder did not recurse.
func TestHTTPBindQueryNestedFiltersTimeAndUUID(t *testing.T) {
	type filters struct {
		UserID  string     `form:"user_id"`
		PriceID uuid.UUID  `form:"price_id"`
		After   *time.Time `form:"created_after" time_format:"2006-01-02"`
	}
	type opts struct {
		Limit   int     `form:"limit"`
		Filters filters `form:",inline"`
	}

	priceID := uuid.New()
	req := httptest.NewRequest(http.MethodGet,
		"/v1/things?limit=7&user_id=u-1&price_id="+priceID.String()+"&created_after=2026-01-02", nil)
	r := NewHTTP(httptest.NewRecorder(), req, nil)

	var got opts
	require.NoError(t, r.ShouldBindQuery(&got))
	require.Equal(t, 7, got.Limit)
	require.Equal(t, "u-1", got.Filters.UserID)
	require.Equal(t, priceID, got.Filters.PriceID)
	require.NotNil(t, got.Filters.After)
	require.Equal(t, "2026-01-02", got.Filters.After.Format("2006-01-02"))
}
