//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/stretchr/testify/require"
)

func TestMerchantCardRemoval(t *testing.T) {
	t.Run("OR-J12-S07/merchant-card-removal-keeps-customer-scope", func(t *testing.T) {
		a, journal := newNMIWorld(t)
		psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		require.Len(t, psps.Items, 1)
		// NMI's v5 refusals, as its sandbox answers them: a request it finds
		// invalid is the provider's refusal, a card it will not take is the
		// customer's decline. Neither shows NMI's own code or text.
		for _, refusal := range []struct {
			envelope, code, recorded string
			status                   int
		}{
			{`{"type":"validationError","error_code":"E_INVALID_SUBMISSION","message":"The provided data is invalid."}`, billing.CodePaymentProviderRejected, "e_invalid_submission", http.StatusBadGateway},
			{`{"type":"generalError","error_code":"E_GENERIC_ERROR","message":"Bad Request. Please use a valid test card or test ACH account from the documentation."}`, billing.CodeCardDeclined, "e_generic_error", http.StatusPaymentRequired},
		} {
			journal.core.Intercept(func(r *http.Request) bool {
				return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v5/customers")
			},
				func(r *http.Request, _ func() *http.Response) (*http.Response, error) {
					return answer(r, http.StatusBadRequest, refusal.envelope), nil
				})
			status, raw := a.request(t, http.MethodPost, "/v1/me/payment-methods", a.aliceToken, billing.CreatePaymentMethodParams{PSPID: psps.Items[0].ID, Token: journal.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242", Exp: "1235"})})
			journal.core.ClearIntercepts()
			require.Equal(t, refusal.status, status, string(raw))
			require.Contains(t, string(raw), refusal.code)
			require.NotContains(t, strings.ToLower(string(raw)), refusal.recorded)
			require.NotContains(t, string(raw), "test card")
			tried, err := a.alpha.ListPaymentAttempts(t.Context(), billing.PaymentAttemptListParams{ResponseCode: []string{refusal.recorded}})
			require.NoError(t, err)
			require.Len(t, tried.Items, 1, "the attempt records NMI's error code")
		}
		status, raw := a.request(t, http.MethodPost, "/v1/me/payment-methods", a.aliceToken, billing.CreatePaymentMethodParams{PSPID: psps.Items[0].ID, Token: journal.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242", Exp: "1235"})})
		require.Equal(t, 201, status, string(raw))
		var method billing.PaymentMethod
		require.NoError(t, json.Unmarshal(raw, &method))
		reader, err := a.auth.CreateAPIKey(t.Context(), iam.SystemIdentity(), a.alphaGroup, iam.NewAPIKey{Name: "card-reader", Role: a.reader})
		require.NoError(t, err)
		path := "/v1/admin/customers/" + method.CustomerID.String() + "/payment-methods/" + method.ID.String()
		for _, check := range []struct {
			token  string
			status int
		}{{"", 401}, {a.bobToken, 403}, {reader.Secret, 403}, {a.betaKey.Secret, 404}} {
			status, raw = a.request(t, http.MethodDelete, path, check.token, nil)
			require.Equal(t, check.status, status, string(raw))
		}
		status, raw = a.request(t, http.MethodDelete, "/v1/admin/customers/"+billing.CustomerID(uuid.MustParse(a.bob.ID)).String()+"/payment-methods/"+method.ID.String(), a.alphaKey.Secret, nil)
		require.Equal(t, 404, status, string(raw))
		kept, err := a.alpha.ListPaymentMethods(t.Context(), method.CustomerID, billing.PaymentMethodListParams{})
		require.NoError(t, err)
		require.Len(t, kept.Items, 1)
		readonlyConfig := a.cfg
		readonlyConfig.ProviderWriteMode = openrails.ProviderWritesReadOnly
		readonly, err := openrails.New(t.Context(), readonlyConfig, a.deps)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, readonly.Close(t.Context())) })
		readonlyMerchant, err := readonly.Merchant(a.alpha.MerchantID())
		require.NoError(t, err)
		require.Error(t, readonlyMerchant.DeletePaymentMethod(t.Context(), method.CustomerID, method.ID), "an explicit host request never bypasses provider read-only mode")
		require.Len(t, journal.core.Vaults(), 1, "read-only removal sends no provider mutation")
		status, raw = a.request(t, http.MethodDelete, path, a.alphaKey.Secret, nil)
		require.Equal(t, http.StatusNoContent, status, string(raw))
		require.NoError(t, a.alphaRemote.DeletePaymentMethod(t.Context(), method.CustomerID, method.ID))
		require.NoError(t, a.alpha.DeletePaymentMethod(t.Context(), method.CustomerID, method.ID), "embedded host uses the same confirmed-operation replay")
		after, err := a.alpha.ListPaymentMethods(t.Context(), method.CustomerID, billing.PaymentMethodListParams{})
		require.NoError(t, err)
		require.Empty(t, after.Items, "removed cards are no longer offered for charging")
		require.Empty(t, journal.core.Vaults(), "the external holder confirmed removal")
		journal.money(t, 0, 0)
	})
}
