package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/pkg/api"
)

// Every decoder failure maps to a coded envelope that names the field and
// never carries Go's decoder text.
func TestCatalogDecodeErrorIsCodedAndNamesTheField(t *testing.T) {
	decode := func(body string) error {
		var out struct {
			Key   string `json:"key"`
			Price int64  `json:"price"`
		}
		d := json.NewDecoder(strings.NewReader(body))
		d.DisallowUnknownFields()
		return d.Decode(&out)
	}
	param := func(e *api.APIError) string {
		if e.Param == nil {
			return ""
		}
		return *e.Param
	}
	cases := []struct {
		name          string
		err           error
		status        int
		code, message string
		param         string
	}{
		{"unknown field", decode(`{"credits_spec":{}}`), http.StatusBadRequest, api.CodeInvalidParam, "unknown field credits_spec", "credits_spec"},
		{"wrong type", decode(`{"price":"7"}`), http.StatusBadRequest, api.CodeInvalidParam, "price is invalid", "price"},
		{"empty body", decode(``), http.StatusBadRequest, api.CodeInvalidParam, "empty_request_body", ""},
		{"malformed", decode(`{"key":`), http.StatusBadRequest, api.CodeInvalidParam, "invalid_request", ""},
		{"too large", &http.MaxBytesError{Limit: 1}, http.StatusRequestEntityTooLarge, openrails.CodeRequestBodyTooLarge, "request body too large", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, tc.err)
			got := catalogDecodeError(tc.err)
			require.Equal(t, tc.status, got.HTTPStatus)
			require.Equal(t, api.ErrorTypeInvalidRequest, got.Type)
			require.Equal(t, tc.code, got.Code)
			require.Equal(t, tc.message, got.Message)
			require.Equal(t, tc.param, param(got))
			require.NotContains(t, got.Message, "json:")
		})
	}
	require.True(t, errors.Is(decode(``), io.EOF))
}
