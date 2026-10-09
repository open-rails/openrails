//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// A customer's settings are one document: a write changes only the fields it
// names, a read lists them back, and clearing a field returns it to its
// default.
func TestCustomerSettingsDocument(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	declareBillingPolicy(t, w, "enterprise")
	a, b := w.newCustomer(), w.newCustomer()
	net30 := billing.InvoiceProfile{NetTermsDays: 30, CollectionMethod: billing.CollectSendInvoice, PONumber: "PO-7",
		BillingContacts: []billing.InvoiceContact{{Name: "AP", Email: "ap@example.test"}}, Tax: map[string]any{}}
	for _, tp := range []topology{embedded, remote} {
		client := w.client[tp]
		fresh, err := client.ListCustomerSettings(t.Context(), billing.CustomerSettingsListParams{IDs: []billing.CustomerID{a.cid()}})
		require.NoError(t, err)
		require.Equal(t, []billing.CustomerSettings{{CustomerID: a.cid(), CreditLimits: []billing.CreditLimit{}, TrustLevels: []billing.TrustLevel{}}}, fresh.Items, "a customer starts at every default")

		updated, err := client.UpdateCustomerSettings(t.Context(), []billing.UpdateCustomerSettingsParams{
			{CustomerID: b.cid(), TrustLevels: []billing.TrustLevel{{Currency: "usd", TrustLevel: " gold "}}},
			{CustomerID: a.cid(), CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 50_000_000}}, BillingPolicy: catalog.Value("enterprise"), InvoiceProfile: catalog.Value(net30)},
		})
		require.NoError(t, err)
		require.Equal(t, []billing.CustomerID{b.cid(), a.cid()}, []billing.CustomerID{updated[0].CustomerID, updated[1].CustomerID}, "request order")
		require.Equal(t, []billing.TrustLevel{{Currency: "USD", TrustLevel: "gold"}}, updated[0].TrustLevels)
		require.Equal(t, []billing.CreditLimit{{Currency: "USD", Amount: 50_000_000}}, updated[1].CreditLimits)
		require.Equal(t, "enterprise", *updated[1].BillingPolicy)
		require.Equal(t, net30, *updated[1].InvoiceProfile)

		// A field the write does not name is unchanged.
		updated, err = client.UpdateCustomerSettings(t.Context(), []billing.UpdateCustomerSettingsParams{{CustomerID: a.cid(), TrustLevels: []billing.TrustLevel{{Currency: "USD", TrustLevel: "silver"}}}})
		require.NoError(t, err)
		require.Equal(t, []billing.CreditLimit{{Currency: "USD", Amount: 50_000_000}}, updated[0].CreditLimits)
		require.Equal(t, "enterprise", *updated[0].BillingPolicy)
		require.Equal(t, net30, *updated[0].InvoiceProfile)

		read, err := client.ListCustomerSettings(t.Context(), billing.CustomerSettingsListParams{IDs: []billing.CustomerID{a.cid(), b.cid(), billing.CustomerID(uuid.New())}})
		require.NoError(t, err)
		require.Len(t, read.Items, 2, "an unknown customer is absent")
		require.ElementsMatch(t, []billing.CustomerSettings{updated[0], {CustomerID: b.cid(), CreditLimits: []billing.CreditLimit{}, TrustLevels: []billing.TrustLevel{{Currency: "USD", TrustLevel: "gold"}}}}, read.Items)

		// Clearing every field returns the document to its defaults.
		cleared, err := client.UpdateCustomerSettings(t.Context(), []billing.UpdateCustomerSettingsParams{
			{CustomerID: a.cid(), CreditLimits: []billing.CreditLimit{{Currency: "USD"}}, TrustLevels: []billing.TrustLevel{{Currency: "USD"}}, BillingPolicy: catalog.Null[string](), InvoiceProfile: catalog.Null[billing.InvoiceProfile]()},
			{CustomerID: b.cid(), TrustLevels: []billing.TrustLevel{{Currency: "USD"}}},
		})
		require.NoError(t, err)
		for i, c := range []*customer{a, b} {
			require.Equal(t, billing.CustomerSettings{CustomerID: c.cid(), CreditLimits: []billing.CreditLimit{}, TrustLevels: []billing.TrustLevel{}}, cleared[i])
		}
	}

	// ids names records alone, 1 to 100 of them, as one comma list.
	for _, query := range []string{"ids=" + a.id + "&limit=1", "ids=" + a.id + "&ids=" + b.id, "ids=" + strings.Repeat(a.id+",", 100) + a.id, "ids="} {
		status, body := w.staffJSON(http.MethodGet, "/v1/merchant/customers/settings?"+query, nil)
		require.Equal(t, http.StatusBadRequest, status, "%s: %v", query, body)
		require.Equal(t, "invalid_query", body["error"].(map[string]any)["code"], "%s: %v", query, body)
	}

	// Without ids, every customer is listed, newest first, a page at a time.
	first, err := w.client[remote].ListCustomerSettings(t.Context(), billing.CustomerSettingsListParams{PageRequest: billing.PageRequest{Limit: 1}})
	require.NoError(t, err)
	require.Equal(t, b.cid(), first.Items[0].CustomerID)
	second, err := w.client[remote].ListCustomerSettings(t.Context(), billing.CustomerSettingsListParams{PageRequest: billing.PageRequest{Limit: 1, Cursor: first.Next}})
	require.NoError(t, err)
	require.Equal(t, a.cid(), second.Items[0].CustomerID)
}

// A batch is validated whole and written all or none: one invalid item
// changes nothing, and its refusal names the item's field.
func TestCustomerSettingsBatchIsAllOrNone(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	declareBillingPolicy(t, w, "enterprise")
	a, b := w.newCustomer(), w.newCustomer()
	valid := map[string]any{"customer_id": a.id, "credit_limits": []any{map[string]any{"currency": "USD", "amount": "10000000"}}}
	for _, refused := range []struct {
		item        map[string]any
		status      int
		code, param string
	}{
		{map[string]any{"customer_id": b.id, "credit_limits": []any{map[string]any{"currency": "USD", "amount": "-1"}}}, http.StatusBadRequest, "invalid_param", "items[1].credit_limits[0].amount"},
		{map[string]any{"customer_id": b.id, "trust_levels": []any{map[string]any{"currency": "XXX", "trust_level": "gold"}}}, http.StatusBadRequest, "currency_unsupported", "items[1].trust_levels[0].currency"},
		{map[string]any{"customer_id": b.id, "invoice_profile": map[string]any{"net_terms_days": 30, "collection_method": "wire"}}, http.StatusBadRequest, "invalid_param", "items[1].invoice_profile.collection_method"},
		{map[string]any{"customer_id": a.id}, http.StatusBadRequest, "invalid_param", "items[1].customer_id"},
		{map[string]any{"customer_id": b.id, "billing_policy": "undeclared"}, http.StatusNotFound, "billing_policy_not_found", "items[1].billing_policy"},
		{map[string]any{"customer_id": uuid.NewString()}, http.StatusNotFound, "customer_not_found", "items[1].customer_id"},
	} {
		// The host's key: a person's writes are limited per item.
		status, body := w.hostJSON(http.MethodPatch, "/v1/merchant/customers/settings", map[string]any{"items": []any{valid, refused.item}})
		require.Equal(t, refused.status, status, "%v", body)
		e := body["error"].(map[string]any)
		require.Equal(t, refused.code, e["code"], "%v", body)
		require.Equal(t, refused.param, e["param"], "%v", body)
	}
	status, body := w.hostJSON(http.MethodPatch, "/v1/merchant/customers/settings", map[string]any{"items": []any{}})
	require.Equal(t, http.StatusBadRequest, status, "%v", body)

	read, err := w.client[remote].ListCustomerSettings(t.Context(), billing.CustomerSettingsListParams{IDs: []billing.CustomerID{a.cid()}})
	require.NoError(t, err)
	require.Empty(t, read.Items[0].CreditLimits, "a refused batch writes nothing")
}

// Every settings field can change a customer's spending authority, so every
// settings write needs a person's recent sign-in. A read does not, and
// machines carry no sign-in and are never asked.
func TestCustomerSettingsStepUp(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	declareBillingPolicy(t, w, "enterprise")
	c := w.newCustomer()
	stale, fresh := w.auth.staleToken(t, "staff"), w.auth.token(t, "staff")
	for _, item := range []map[string]any{
		{"credit_limits": []any{map[string]any{"currency": "USD", "amount": "1000000"}}},
		{"trust_levels": []any{map[string]any{"currency": "USD", "trust_level": "gold"}}},
		{"billing_policy": "enterprise"},
		{"billing_policy": nil},
		{"invoice_profile": map[string]any{"net_terms_days": 15, "collection_method": "send_invoice", "po_number": "", "tax": map[string]any{}, "billing_contacts": []any{}, "memo": ""}},
		{},
	} {
		item["customer_id"] = c.id
		body := map[string]any{"items": []any{item}}
		status, refused := w.merchantJSON(stale, http.MethodPatch, "/v1/merchant/customers/settings", body)
		require.Equal(t, http.StatusForbidden, status, "%v: %v", item, refused)
		require.Equal(t, "step_up_required", refused["error"].(map[string]any)["code"], "%v: %v", item, refused)
		status, answered := w.merchantJSON(fresh, http.MethodPatch, "/v1/merchant/customers/settings", body)
		require.Equal(t, http.StatusOK, status, "%v: %v", item, answered)
	}
	status, body := w.merchantJSON(stale, http.MethodGet, "/v1/merchant/customers/settings?ids="+c.id, nil)
	require.Equal(t, http.StatusOK, status, "a read needs no step-up: %v", body)
	status, body = w.hostJSON(http.MethodPatch, "/v1/merchant/customers/settings", map[string]any{"items": []any{map[string]any{"customer_id": c.id, "credit_limits": []any{map[string]any{"currency": "USD", "amount": "2000000"}}}}})
	require.Equal(t, http.StatusOK, status, "the host's API key needs no sign-in: %v", body)
}

// The per-field settings routes are gone.
func TestCustomerSettingsFieldRoutesRemoved(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	token := w.auth.token(t, "staff")
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "credit-limit?currency=USD"}, {http.MethodPut, "credit-limit"},
		{http.MethodGet, "trust-level?currency=USD"}, {http.MethodPut, "trust-level"},
		{http.MethodGet, "billing-policy"}, {http.MethodPut, "billing-policy"},
		{http.MethodGet, "invoice-profile"}, {http.MethodPut, "invoice-profile"},
	} {
		req, err := http.NewRequestWithContext(t.Context(), route.method, w.server.URL+mountPrefix+"/v1/merchant/customers/"+c.id+"/"+route.path, strings.NewReader(`{}`))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("OpenRails-Merchant", w.slug)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.NoError(t, res.Body.Close())
		require.Equal(t, http.StatusNotFound, res.StatusCode, "%s %s", route.method, route.path)
	}
}

func declareBillingPolicy(t *testing.T, w *world, name string) {
	t.Helper()
	client := w.client[embedded]
	got, err := client.GetMerchantConfiguration(t.Context())
	require.NoError(t, err)
	_, err = client.ApplyMerchantConfiguration(t.Context(), billing.ApplyMerchantConfigurationParams{
		ApplicationID: uuid.NewString(), ExpectedRevision: &got.Revision,
		Settings: &billing.MerchantSettings{BillingPolicies: []billing.BillingPolicy{{Name: name, Kind: "outstanding_cap", OutstandingCapAmount: 100_000_000}}},
	})
	require.NoError(t, err)
}
