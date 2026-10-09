//go:build e2e && integration

package ci_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/checkoutsession"
)

// A purchase is made as production makes it: the merchant hands its customer
// a checkout session through its Client, and the payment page reads and pays
// it by its id alone.

type checkoutSession struct {
	t    *testing.T
	page http.Handler
	id   string
}

// sell mints a checkout session through client and opens its payment page.
func sell(t *testing.T, client *openrails.Client, mint billing.CreateCheckoutSessionParams) (*checkoutSession, error) {
	t.Helper()
	link, err := client.CreateCheckoutSession(t.Context(), mint)
	if err != nil {
		return nil, err
	}
	return (&checkoutSession{t: t, id: link.ID}).on(client), nil
}

// on is the session on client's payment page: any process of its merchant
// serves it.
func (s *checkoutSession) on(client *openrails.Client) *checkoutSession {
	s.t.Helper()
	mux := http.NewServeMux()
	require.NoError(s.t, openrailshttp.Mount(mux, client, openrails.Routes{Storefront: true}))
	return &checkoutSession{t: s.t, page: mux, id: s.id}
}

func (s *checkoutSession) call(method, path string, body any) (int, []byte) {
	s.t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		require.NoError(s.t, err)
	}
	req := httptest.NewRequest(method, "/v1/checkout-sessions/"+s.id+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.page.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// read is the session as the payment page shows it.
func (s *checkoutSession) read() checkoutsession.CheckoutSession {
	s.t.Helper()
	status, raw := s.call(http.MethodGet, "", nil)
	require.Equal(s.t, http.StatusOK, status, "%s", raw)
	var out checkoutsession.CheckoutSession
	require.NoError(s.t, json.Unmarshal(raw, &out))
	return out
}

// option is the session's option on rail.
func (s *checkoutSession) option(rail string) string {
	s.t.Helper()
	for _, option := range s.read().Options {
		if option.Rail == rail {
			return option.ID
		}
	}
	s.t.Fatalf("no %s option", rail)
	return ""
}

// pay sends the page's pay body: the option on rail and fields beside it. A
// refusal is the error the remote Client reads.
func (s *checkoutSession) pay(rail string, fields map[string]any) (*checkoutsession.CheckoutSessionPayResult, error) {
	s.t.Helper()
	body := map[string]any{"option_id": s.option(rail)}
	for k, v := range fields {
		body[k] = v
	}
	status, raw := s.call(http.MethodPost, "/pay", body)
	if status != http.StatusOK {
		var envelope struct {
			Error *billing.ErrorDetails `json:"error"`
		}
		out := &billing.StatusError{Status: status}
		if json.Unmarshal(raw, &envelope) == nil && envelope.Error != nil {
			out.ErrorDetails = *envelope.Error
		}
		return nil, out
	}
	var out checkoutsession.CheckoutSessionPayResult
	require.NoError(s.t, json.Unmarshal(raw, &out))
	return &out, nil
}

// attempt is the checkout attempt the session's payment created.
func (s *checkoutSession) attempt(f *fixture) billing.CheckoutAttemptID {
	s.t.Helper()
	var id uuid.UUID
	require.NoError(s.t, f.pool.QueryRow(s.t.Context(), "SELECT attempt_id FROM "+pgx.Identifier{f.schema, "checkout_sessions"}.Sanitize()+" WHERE id_hash = sha256($1)", []byte(s.id)).Scan(&id))
	return billing.CheckoutAttemptID(id)
}
