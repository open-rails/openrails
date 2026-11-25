//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

// A personal access token is its user's but proves no sign-in: it reads as the
// customer and holds its staff permissions, yet never pays or takes a Person
// action. The user's own session does both.
func TestPersonalAccessTokens(t *testing.T) {
	t.Run("OR-J01-S17/personal-access-token-never-acts-in-person", func(t *testing.T) {
		a, provider := newPaymentWorldWithAuth(t, func(c *authkit.Config) { c.PersonalAccessTokens.Enabled = true })
		input := saleInput(t, a, provider)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		invoice, err := a.alpha.CreateInvoice(t.Context(), "pat-invoice", billing.CreateInvoiceParams{CustomerID: customer, Lines: input.Lines})
		require.NoError(t, err)
		session, _ := authenticateCustomerProof(t, a.auth.Authenticator(), a.aliceToken, nil)
		pat, err := a.auth.CreatePersonalAccessToken(t.Context(), session.Identity(), iam.NewPersonalAccessToken{
			Name: "ci", GroupID: a.alpha.MerchantID().String(),
			Permissions: []iam.Perm{mustPermission(t, string(billing.MerchantConfigRead)), mustPermission(t, string(billing.MerchantConfigManage))},
		})
		require.NoError(t, err)

		path := "/v1/me/invoices/" + invoice.ID.String()
		status, raw := a.request(t, http.MethodGet, path, pat.Secret, nil)
		require.Equal(t, http.StatusOK, status, "a personal access token reads as its customer: %s", raw)
		pay := func() billing.PayInvoiceParams {
			token := provider.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})
			return billing.PayInvoiceParams{Payment: billing.InvoicePaymentParams{Token: token}, ExpectedTotal: 30_000_000, Reusable: ptr(true)}
		}
		status, raw = a.request(t, http.MethodPost, path+"/pay", pat.Secret, pay(), map[string]string{"Idempotency-Key": "pat-pay"})
		require.Equal(t, http.StatusForbidden, status, string(raw))
		require.Contains(t, string(raw), `"code":"customer_action_required"`)
		require.Empty(t, provider.effects(t), "a refused token charges nothing")
		unpaid, err := a.alpha.GetInvoice(t.Context(), invoice.ID)
		require.NoError(t, err)
		require.Equal(t, billing.InvoiceOpen, unpaid.Status)
		status, raw = a.request(t, http.MethodPost, path+"/pay", a.aliceToken, pay(), map[string]string{"Idempotency-Key": "session-pay"})
		require.Equal(t, http.StatusOK, status, string(raw))
		var paid billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &paid))
		require.Equal(t, billing.InvoiceComplete, paid.Status)
		require.Len(t, provider.effects(t), 1)

		status, raw = a.request(t, http.MethodGet, "/v1/merchant/proposals", pat.Secret, nil)
		require.Equal(t, http.StatusOK, status, "the token holds the merchant's config permissions: %s", raw)
		body := map[string]any{"ids": []string{uuid.NewString()}, "notes": "automation"}
		for _, route := range []string{"/v1/merchant/proposals/approve", "/v1/merchant/proposals/dismiss", "/v1/merchant/psp-imports/" + uuid.NewString() + "/apply"} {
			key := map[string]string{"Idempotency-Key": "person" + route}
			status, raw = a.request(t, http.MethodPost, route, pat.Secret, body, key)
			require.Equal(t, http.StatusForbidden, status, "%s: %s", route, raw)
			require.Contains(t, string(raw), `"code":"person_required"`)
			_, raw = a.request(t, http.MethodPost, route, a.aliceToken, body, key)
			require.NotContains(t, string(raw), "person_required", "%s: a session is a person", route)
		}
		status, raw = a.request(t, http.MethodPost, "/v1/merchant/proposals/dismiss", a.aliceToken, map[string]any{"ids": []string{uuid.NewString()}})
		require.Equal(t, http.StatusBadRequest, status, "a session reaches the handler's own checks: %s", raw)
		require.Contains(t, string(raw), `"code":"invalid_param"`)
	})
}
