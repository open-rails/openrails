package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// Catalog declarations reject unknown fields so retired benefits cannot be silently ignored.
func bindCatalogJSON(r *httprequest.Request, out any) bool {
	decoder := json.NewDecoder(r.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			r.ErrorJSON(http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			r.ErrorJSON(http.StatusBadRequest, err.Error())
		}
		return false
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		r.ErrorJSON(http.StatusBadRequest, "one catalog document required")
		return false
	}
	return true
}
