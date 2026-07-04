//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/embedded"
)

func TestEmbeddedMountHandlerEndToEnd(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	dbtest.EnsureTestMerchant(ctx, t, h.sharedPool())

	subject := uuid.New()
	authn := billingauth.DelegatedAuthenticatorFunc(func(ctx context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			return nil, billingauth.ErrUnauthenticated
		}
		return &billingauth.DelegatedPrincipal{
			MerchantID:   dbtest.TestMerchantID.String(),
			MerchantSlug: dbtest.TestMerchantSlug,
			SubjectID:    subject.String(),
			Issuer:       "embedded-host",
			Permissions:  []string{permissions.MerchantAll},
		}, nil
	})

	rt, err := embed.New(ctx, embed.Options{
		Options: embedded.Options{
			Config: &config.Config{
				Env:      "dev",
				TestMode: config.CredentialPostureSandbox,
				DB:       &config.DBConfig{URL: h.DSN},
			},
			Redis: h.Redis,
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	handler, err := embedded.MountHandler(rt.Embedded(), embedded.MountOptions{
		MountPrefix:            "/api/openrails",
		RouteSets:              []embedded.RouteSet{embedded.RouteSetMerchantAPI, embedded.RouteSetCustomer},
		Gate:                   httproutes.NewGate(httproutes.GateOptions{DelegatedAuthenticator: authn}),
		DelegatedAuthenticator: authn,
	})
	require.NoError(t, err)

	sourceID := uuid.New()
	body, err := json.Marshal(map[string]any{
		"customer_id": subject.String(),
		"invoker":     subject.String(),
		"currency":    "USD",
		"amount":      12345,
		"source":      "embedded_mount_test",
		"source_id":   sourceID.String(),
	})
	require.NoError(t, err)
	w := doMounted(handler, http.MethodPost, "/api/openrails/v1/merchant/credits/deposit", body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = doMounted(handler, http.MethodGet, "/api/openrails/v1/me/balance?currency=USD", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var balance struct {
		Currency      string `json:"currency"`
		BalanceAmount int64  `json:"balance_amount"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &balance), w.Body.String())
	require.Equal(t, "USD", balance.Currency)
	require.EqualValues(t, 12345, balance.BalanceAmount)
}

func doMounted(h http.Handler, method, path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer host-session")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
