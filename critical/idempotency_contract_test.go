//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/stretchr/testify/require"
)

func TestRequestIdempotency(t *testing.T) {
	t.Run("OR-J27-S15/a-grant-needs-its-key-and-replays-under-it", func(t *testing.T) {
		a := newAuthorityWorld(t)
		product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "comp-pass", DisplayName: "Comp pass", Entitlements: []string{"library:read"}})
		require.NoError(t, err)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		grant := billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{{CustomerID: customer, ProductID: product.ID, Hours: ptr(24), Reason: billing.GrantReasonComp}}}
		status, raw := a.request(t, http.MethodPost, "/v1/admin/product-access", a.alphaKey.Secret, grant)
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		require.Contains(t, string(raw), `"code":"idempotency_key_required"`)
		var granted []billing.ProductAccessGrant
		for range 2 {
			status, raw = a.request(t, http.MethodPost, "/v1/admin/product-access", a.alphaKey.Secret, grant, map[string]string{"Idempotency-Key": "comp-once"})
			require.Equal(t, http.StatusCreated, status, string(raw))
			var out billing.CreateProductAccessBatchResult
			require.NoError(t, json.Unmarshal(raw, &out))
			require.Len(t, out.Items, 1)
			granted = append(granted, out.Items[0])
		}
		require.Equal(t, granted[0].ID, granted[1].ID, "the retry answers the first grant")
		windows, err := a.alpha.ListProductAccess(t.Context(), billing.ProductAccessListParams{CustomerIDs: []billing.CustomerID{customer}, ProductIDs: []billing.ProductID{product.ID}})
		require.NoError(t, err)
		require.Len(t, windows.Items, 1, "a refused request and a retry grant nothing more")
	})
	t.Run("OR-J10-S06/a-card-save-key-answers-one-saved-card", func(t *testing.T) {
		a, journal := newNMIWorld(t)
		psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		require.Len(t, psps.Items, 1)
		key := map[string]string{"Idempotency-Key": "save-once"}
		save := billing.CreatePaymentMethodParams{PSPID: psps.Items[0].ID, Token: journal.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242", Exp: "1235"}), BillingDetails: billedTo("US", "10001")}
		var saved []billing.PaymentMethod
		for range 2 {
			status, raw := a.request(t, http.MethodPost, "/v1/me/payment-methods", a.aliceToken, save, key)
			require.Equal(t, http.StatusCreated, status, string(raw))
			var method billing.PaymentMethod
			require.NoError(t, json.Unmarshal(raw, &method))
			saved = append(saved, method)
		}
		require.Equal(t, saved[0].ID, saved[1].ID, "the retry answers the first saved card")
		other := billing.CreatePaymentMethodParams{PSPID: psps.Items[0].ID, Token: journal.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "1111", Exp: "1236"}), BillingDetails: billedTo("US", "10001")}
		status, raw := a.request(t, http.MethodPost, "/v1/me/payment-methods", a.aliceToken, other, key)
		require.Equal(t, http.StatusUnprocessableEntity, status, string(raw))
		require.Contains(t, string(raw), `"code":"idempotency_key_reused"`)
		methods, err := a.alpha.ListPaymentMethods(t.Context(), saved[0].CustomerID, billing.PaymentMethodListParams{})
		require.NoError(t, err)
		require.Len(t, methods.Items, 1, "one key vaults one card at the PSP")
	})
}
