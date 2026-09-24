package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db"
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

func TestResolvedWebhookAccountFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		found  bool
		id     uuid.UUID
		err    error
		status int
	}{
		{name: "resolver failure", found: true, id: uuid.New(), err: errors.New("database unavailable"), status: 500},
		{name: "missing account", status: 401},
		{name: "empty resolved identity", found: true, status: 401},
		{name: "resolved account", found: true, id: uuid.New(), status: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := request.NewHTTP(w, httptest.NewRequest(http.MethodPost, "/", nil), nil)
			bound := bindResolvedWebhookPSP(r, tc.id, tc.found, tc.err)
			require.Equal(t, tc.status, w.Code)
			if tc.status == 200 {
				require.True(t, bound)
				require.Equal(t, tc.id, db.PSPIDFromContext(r.Request.Context()))
			} else {
				require.False(t, bound)
				require.Equal(t, uuid.Nil, db.PSPIDFromContext(r.Request.Context()))
			}
		})
	}
}

func TestSignedStripeSnapshotCannotNameForeignAccount(t *testing.T) {
	for _, field := range []string{"account", "context"} {
		body := []byte(`{"id":"evt_scope","type":"charge.refunded","` + field + `":"acct_other","data":{"object":{"id":"ch_scope"}}}`)
		prepared, err := prepareStripeMultiSecret(body, []string{"whsec_selected"}, signStripe("whsec_selected", body), time.Minute)
		require.NoError(t, err, "the valid signature must not replace account scope validation")
		_, err = hydrateThinStripeEvent(context.Background(), "sk_test", "acct_selected", prepared.Body, nil)
		require.ErrorContains(t, err, "does not match routed account")
	}
}
