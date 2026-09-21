package handlers

import (
	"encoding/json"
	"github.com/open-rails/openrails"
	"strings"
	"testing"
)

func TestCatalogPublishRefusesRetiredPreviewFlag(t *testing.T) {
	var req openrails.CatalogPublishRequest
	decoder := json.NewDecoder(strings.NewReader(`{"plan_only":true}`))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err == nil {
		t.Fatal("retired plan_only field accepted")
	}
}
