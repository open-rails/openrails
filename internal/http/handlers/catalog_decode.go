package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/open-rails/openrails"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/pkg/api"
)

// Catalog declarations reject unknown fields so retired benefits cannot be silently ignored.
func bindCatalogJSON(r *httprequest.Request, out any) bool {
	decoder := json.NewDecoder(r.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		r.APIError(catalogDecodeError(err))
		return false
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		r.APIError(invalidCatalogParam("one catalog document required"))
		return false
	}
	return true
}

// catalogDecodeError answers a decoder failure in the coded envelope: the
// refused field is named in param, Go's decoder text never reaches the wire,
// and an oversized body carries the same code as the body-limit middleware.
func catalogDecodeError(err error) *api.APIError {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return api.NewAPIError(http.StatusRequestEntityTooLarge, api.ErrorTypeInvalidRequest, openrails.CodeRequestBodyTooLarge, "request body too large")
	}
	if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		field = strings.Trim(field, `"`)
		return invalidCatalogParam("unknown field " + field).WithParam(field)
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) && typeErr.Field != "" {
		return invalidCatalogParam(typeErr.Field + " is invalid").WithParam(typeErr.Field)
	}
	if errors.Is(err, io.EOF) {
		return invalidCatalogParam("empty_request_body")
	}
	return invalidCatalogParam("invalid_request")
}

func invalidCatalogParam(message string) *api.APIError {
	return api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, api.CodeInvalidParam, message)
}
