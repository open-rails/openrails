//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"sync"
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
		changed := grant.Items[0]
		changed.Hours = ptr(720)
		status, raw = a.request(t, http.MethodPost, "/v1/admin/product-access", a.alphaKey.Secret, billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{changed}}, map[string]string{"Idempotency-Key": "comp-once"})
		require.Equal(t, http.StatusUnprocessableEntity, status, string(raw))
		require.Contains(t, string(raw), `"code":"idempotency_key_reused"`)
		other := grant.Items[0]
		other.CustomerID = billing.CustomerID(uuid.New())
		batch := billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{grant.Items[0], other}}
		status, raw = a.request(t, http.MethodPost, "/v1/admin/product-access", a.alphaKey.Secret, batch, map[string]string{"Idempotency-Key": "comp-once"})
		require.Equal(t, http.StatusUnprocessableEntity, status, string(raw))
		require.Contains(t, string(raw), `"code":"idempotency_key_reused"`)
		windows, err = a.alpha.ListProductAccess(t.Context(), billing.ProductAccessListParams{CustomerIDs: []billing.CustomerID{other.CustomerID}, ProductIDs: []billing.ProductID{product.ID}})
		require.NoError(t, err)
		require.Empty(t, windows.Items)
		queued, err := a.alpha.CreateProductAccess(t.Context(), "comp-queued", grant.Items)
		require.NoError(t, err)
		require.Len(t, queued, 1)
		require.Equal(t, "scheduled", queued[0].Status)
		require.NoError(t, a.alpha.RevokeProductAccess(t.Context(), queued[0].ID, billing.RevokeProductAccessParams{Reason: "comp withdrawn before it starts"}))
		status, raw = a.request(t, http.MethodPost, "/v1/admin/product-access", a.alphaKey.Secret, grant, map[string]string{"Idempotency-Key": "comp-queued"})
		require.Equal(t, http.StatusConflict, status, string(raw))
		require.Contains(t, string(raw), `"code":"resource_conflict"`)
		windows, err = a.alpha.ListProductAccess(t.Context(), billing.ProductAccessListParams{CustomerIDs: []billing.CustomerID{customer}, ProductIDs: []billing.ProductID{product.ID}})
		require.NoError(t, err)
		require.Len(t, windows.Items, 1, "a retry after revocation grants nothing more")
		require.Equal(t, granted[0].ID, windows.Items[0].ID)
		_, err = a.alpha.CreateProductAccess(t.Context(), "comp-pair", batch.Items)
		require.NoError(t, err)
		status, raw = a.request(t, http.MethodPost, "/v1/admin/product-access", a.alphaKey.Secret, grant, map[string]string{"Idempotency-Key": "comp-pair"})
		require.Equal(t, http.StatusUnprocessableEntity, status, string(raw))
		require.Contains(t, string(raw), `"code":"idempotency_key_reused"`)
		var statuses [3]int
		var raws [3][]byte
		var errs [3]error
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range statuses {
			wg.Go(func() {
				<-start
				statuses[i], raws[i], errs[i] = a.do(t.Context(), http.MethodPost, "/v1/admin/product-access", a.alphaKey.Secret, batch, map[string]string{"Idempotency-Key": "comp-race"})
			})
		}
		close(start)
		wg.Wait()
		var answers [3]billing.CreateProductAccessBatchResult
		for i := range statuses {
			require.NoError(t, errs[i])
			require.Equal(t, http.StatusCreated, statuses[i], string(raws[i]))
			require.NoError(t, json.Unmarshal(raws[i], &answers[i]))
			require.Len(t, answers[i].Items, 2)
			for j := range answers[i].Items {
				require.Equal(t, answers[0].Items[j].ID, answers[i].Items[j].ID)
			}
		}
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
