package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/app"
	request "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestWebhookAccountCannotFallBack(t *testing.T) {
	for _, provider := range []string{"stripe", "nmi", "ccbill", "basistheory"} {
		t.Run(provider, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := request.NewHTTP(w, httptest.NewRequest(http.MethodPost, "/", nil), nil)
			processResolvedMerchantWebhook(r, provider, merchant.ID(uuid.New()), "")
			require.Equal(t, http.StatusBadRequest, w.Code)
			require.Contains(t, w.Body.String(), "account_id is required")
		})
	}
}

func TestBasisTheoryWebhookAccountMustMatchTenant(t *testing.T) {
	for _, tenant := range []string{"", "tenant-other"} {
		w := httptest.NewRecorder()
		r := request.NewHTTP(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"id":"evt_1","type":"token.updated","tenant_id":"`+tenant+`"}`)), &app.Runtime{})
		require.False(t, processMerchantBasisTheoryWebhook(r, merchant.ID(uuid.New()), "tenant-selected"))
		require.Equal(t, http.StatusBadRequest, w.Code)
		require.Contains(t, w.Body.String(), "does not match payload")
	}
}
