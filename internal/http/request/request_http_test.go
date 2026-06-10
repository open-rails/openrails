package request

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

func TestHTTPGetUserFromContext(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r = r.WithContext(billingauth.SetUserContext(r.Context(), billingauth.UserContext{
		UserID: "u-1", Username: "ada", Roles: []string{"admin"},
	}))
	req := NewHTTP(httptest.NewRecorder(), r, nil)

	u := req.GetUser()
	if u == nil || u.ID != "u-1" || u.Username != "ada" {
		t.Fatalf("GetUser = %+v", u)
	}
}

func TestHTTPSetGet(t *testing.T) {
	req := NewHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil), nil)
	req.Set("k", 42)
	if v, ok := req.Get("k"); !ok || v != 42 {
		t.Fatalf("Get = %v %v", v, ok)
	}
}
