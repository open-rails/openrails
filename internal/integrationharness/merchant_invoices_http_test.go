//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	authkit "github.com/open-rails/authkit"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
	billingservice "github.com/open-rails/openrails/pkg/service"
	"github.com/stretchr/testify/require"
)

type invoiceAdminCharger struct {
	mu        sync.Mutex
	calls     []money.ChargeRequest
	ambiguous bool
}

func (c *invoiceAdminCharger) ChargeSavedMethod(_ context.Context, request money.ChargeRequest) (money.ChargeResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, request)
	if c.ambiguous {
		return money.ChargeResult{}, &nmi.TransportAmbiguousError{Err: errors.New("lost response")}
	}
	return money.ChargeResult{Rail: "nmi", TransactionID: "invoice-test-" + request.IdempotencyKey}, nil
}

func invoiceRequest(t *testing.T, method, url, token, key string, body any) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req, err := http.NewRequest(method, url, &buf)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	response, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, raw
}

func TestMerchantInvoiceAdministrationHTTP(t *testing.T) {
	h := New(t, context.Background())
	surface := h.StartStandalone("usd")
	rt := surface.App().Runtime
	owner := surface.Token
	cp := embcp.Get(surface.App())
	actor := h.ensureAPIKeyActor(cp, dbtest.TestMerchantSlug)
	mintRole := func(role string) string {
		t.Helper()
		_, token, err := cp.Core().MintAPIKeyWithOptions(h.ctx, controlplane.MerchantGroup(dbtest.TestMerchantSlug), authkit.APIKeyMintOptions{Name: "invoice-" + role + "-" + uuid.NewString(), Role: authkit.Role(role), CreatedBy: actor})
		require.NoError(t, err)
		return token
	}
	reader := mintRole(controlplane.MerchantRoleViewer)
	collector := mintRole(controlplane.MerchantRoleSupport)
	b := surface.ProvisionOwnedMerchant("invoice-b-" + uuid.NewString())
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	makeCustomer := func(mid merchant.ID, currency string) identity.CustomerID {
		t.Helper()
		payer := identity.CustomerID(uuid.New())
		mctx := merchant.WithID(context.Background(), mid)
		mode := money.BillingModeArrears
		err := rt.DB.RunInMerchantConn(mctx, func(scoped context.Context) error {
			_, e := rt.MoneyService.UpsertAccountSettings(scoped, payer, currency, money.AccountSettingsInput{BillingMode: &mode})
			return e
		})
		require.NoError(t, err)
		return payer
	}
	issue := func(mid merchant.ID, payer identity.CustomerID, currency string, amount int64) *models.Invoice {
		t.Helper()
		mctx := merchant.WithID(context.Background(), mid)
		_, err := rt.MoneyService.AccrueOwed(mctx, payer, currency, "invoice-admin-test", uuid.NewString(), amount)
		require.NoError(t, err)
		inv, err := rt.MoneyService.FinalizeInvoice(mctx, payer, currency, time.Now().Add(-time.Hour), time.Now())
		require.NoError(t, err)
		return inv
	}
	customer := makeCustomer(dbtest.TestMerchantID, "USD")
	profilePath := surface.BaseURL + "/v1/merchant/customers/" + customer.String() + "/invoice-profile"
	original := billingservice.InvoiceProfileDTO{NetTermsDays: 30, CollectionMethod: money.CollectionSendInvoice, PONumber: "PO-old", Tax: map[string]any{"tax_id": "VAT-old"}, BillingContacts: []billingservice.InvoiceContactDTO{{Name: "AP", Email: "old@example.test"}}, Memo: "Original terms"}
	status, body := invoiceRequest(t, http.MethodPut, profilePath, owner, "", original)
	require.Equal(t, 200, status, string(body))
	invoice := issue(dbtest.TestMerchantID, customer, "USD", 5000000)
	updated := original
	updated.PONumber = "PO-new"
	updated.NetTermsDays = 7
	updated.Tax = map[string]any{"tax_id": "VAT-new"}
	updated.BillingContacts = []billingservice.InvoiceContactDTO{{Email: "new@example.test"}}
	status, body = invoiceRequest(t, http.MethodPut, profilePath, owner, "", updated)
	require.Equal(t, 200, status, string(body))
	invoicePath := surface.BaseURL + "/v1/merchant/invoices/" + invoice.ID.String()
	status, body = invoiceRequest(t, http.MethodGet, invoicePath, reader, "", nil)
	require.Equal(t, 200, status, string(body))
	var read billingservice.MerchantInvoiceDTO
	require.NoError(t, json.Unmarshal(body, &read))
	require.Equal(t, "PO-old", *read.PONumber)
	require.Equal(t, "VAT-old", read.Tax["tax_id"])
	require.Equal(t, "old@example.test", read.BillingContacts[0].Email)
	require.Equal(t, 6, read.UnitDecimals)
	require.Empty(t, read.AvailableActions)
	require.Equal(t, 30*24*time.Hour, read.DueAt.Sub(*read.IssuedAt))
	status, body = invoiceRequest(t, http.MethodGet, profilePath, reader, "", nil)
	require.Equal(t, 200, status, string(body))
	require.Contains(t, string(body), "PO-new")
	require.Contains(t, string(body), `"can_update":false`)
	status, body = invoiceRequest(t, http.MethodPut, profilePath, reader, "", original)
	require.Equal(t, 403, status, string(body))
	invalid := updated
	invalid.NetTermsDays = int(money.MaxInvoiceNetTermsDays) + 1
	status, body = invoiceRequest(t, http.MethodPut, profilePath, owner, "", invalid)
	require.Equal(t, 400, status, string(body))
	invalid = updated
	invalid.BillingContacts = []billingservice.InvoiceContactDTO{{Email: "invalid"}}
	status, body = invoiceRequest(t, http.MethodPut, profilePath, owner, "", invalid)
	require.Equal(t, 400, status, string(body))

	other := makeCustomer(b.MerchantID, "USD")
	foreign := issue(b.MerchantID, other, "USD", 3000000)
	for _, path := range []string{"/invoices/" + foreign.ID.String(), "/invoices/" + foreign.ID.String() + "/payments", "/customers/" + other.String() + "/invoice-profile"} {
		status, body = invoiceRequest(t, http.MethodGet, surface.BaseURL+"/v1/merchant"+path, owner, "", nil)
		require.Equal(t, 404, status, string(body))
	}
	status, body = invoiceRequest(t, http.MethodPut, surface.BaseURL+"/v1/merchant/customers/"+other.String()+"/invoice-profile", owner, "", updated)
	require.Equal(t, 404, status, string(body))
	status, body = invoiceRequest(t, http.MethodPost, surface.BaseURL+"/v1/merchant/invoices/"+foreign.ID.String()+"/void", owner, "", nil)
	require.Equal(t, 404, status, string(body))
	status, body = invoiceRequest(t, http.MethodGet, surface.BaseURL+"/v1/merchant/invoices?customer_id="+customer.String()+"&currency=USD&status=open&limit=1", reader, "", nil)
	require.Equal(t, 200, status, string(body))
	var page struct {
		Items []billingservice.MerchantInvoiceDTO `json:"items"`
		Total int                                 `json:"total"`
	}
	require.NoError(t, json.Unmarshal(body, &page))
	require.Len(t, page.Items, 1)
	require.Equal(t, 1, page.Total)
	require.Equal(t, invoice.ID, page.Items[0].ID)
	status, body = invoiceRequest(t, http.MethodGet, surface.BaseURL+"/v1/merchant/invoices?customer_id="+customer.String()+"&limit=1&offset=1", reader, "", nil)
	require.Equal(t, 200, status, string(body))
	require.NoError(t, json.Unmarshal(body, &page))
	require.Empty(t, page.Items)
	require.Equal(t, 1, page.Total)
	status, body = invoiceRequest(t, http.MethodGet, surface.BaseURL+"/v1/merchant/invoices?customer_id="+other.String(), owner, "", nil)
	require.Equal(t, 200, status, string(body))
	require.NoError(t, json.Unmarshal(body, &page))
	require.Empty(t, page.Items)
	for _, query := range []string{"status=bad", "limit=0", "customer_id=bad", "currency=BAD", "period_from=2026-09-03&period_to=2026-09-01"} {
		status, body = invoiceRequest(t, http.MethodGet, surface.BaseURL+"/v1/merchant/invoices?"+query, reader, "", nil)
		require.Equal(t, 400, status, string(body))
	}
	status, body = invoiceRequest(t, http.MethodPost, invoicePath+"/void", reader, "", nil)
	require.Equal(t, 403, status, string(body))
	status, body = invoiceRequest(t, http.MethodPost, invoicePath+"/payments", owner, "", map[string]any{"amount": 1000000, "reference": "bank-one"})
	require.Equal(t, 200, status, string(body))
	status, body = invoiceRequest(t, http.MethodPost, invoicePath+"/payments", owner, "", map[string]any{"amount": 1000000, "reference": "bank-one"})
	require.Equal(t, 409, status, string(body))
	for range 2 {
		status, body = invoiceRequest(t, http.MethodPost, invoicePath+"/void", owner, "", nil)
		require.Equal(t, 200, status, string(body))
		require.Contains(t, string(body), `"status":"voided"`)
	}
	owed, err := rt.MoneyService.GetOutstandingOwed(ctx, customer, "USD")
	require.NoError(t, err)
	require.Zero(t, owed)
	status, body = invoiceRequest(t, http.MethodPost, invoicePath+"/payments", owner, "", map[string]any{"amount": 1000000, "reference": "bank-two"})
	require.Equal(t, 409, status, string(body))

	// JPY stores 10^4 native units per yen. Manual settlement stays native;
	// automatic collection converts only at the existing rail boundary.
	yenCustomer := makeCustomer(dbtest.TestMerchantID, "JPY")
	yen := issue(dbtest.TestMerchantID, yenCustomer, "JPY", 120000)
	yenPath := surface.BaseURL + "/v1/merchant/invoices/" + yen.ID.String()
	status, body = invoiceRequest(t, http.MethodGet, yenPath, owner, "", nil)
	require.Equal(t, 200, status, string(body))
	require.NoError(t, json.Unmarshal(body, &read))
	require.Equal(t, 4, read.UnitDecimals)
	require.EqualValues(t, 120000, read.TotalAmount)
	status, body = invoiceRequest(t, http.MethodPost, yenPath+"/payments", owner, "", map[string]any{"amount": 20000, "reference": "JPY-bank"})
	require.Equal(t, 200, status, string(body))
	require.NoError(t, json.Unmarshal(body, &read))
	require.EqualValues(t, 100000, read.AmountDue)
	psp := dbtest.EnsureTestPSP(ctx, t, h.MerchantPool(dbtest.TestMerchantID.UUID()), dbtest.TestMerchantID.UUID(), "nmi")
	method := uuid.New()
	require.NoError(t, paymentmethods.NewPaymentMethodRepo(h.MerchantDB(dbtest.TestMerchantID.UUID())).Create(ctx, &models.PaymentMethod{ID: method, CustomerID: yenCustomer.UUID(), PspID: psp, Rail: models.RailNMI, RailCustomerRef: "vault-" + method.String(), RailMethodRef: "method-" + method.String(), InitialTransactionID: "initial-" + method.String()}))
	_, err = rt.MoneyService.MarkInvoicesPastDue(ctx, time.Now().Add(time.Minute))
	require.NoError(t, err)
	charger := &invoiceAdminCharger{}
	rt.MoneyCharger = charger
	status, body = invoiceRequest(t, http.MethodPost, yenPath+"/retry-collection", reader, "jpy-retry", map[string]any{"payment_method_id": method})
	require.Equal(t, 403, status, string(body))
	var attemptID uuid.UUID
	for i := range 2 {
		status, body = invoiceRequest(t, http.MethodPost, yenPath+"/retry-collection", collector, "jpy-retry", map[string]any{"payment_method_id": method})
		require.Equal(t, 200, status, string(body))
		var result billingservice.InvoiceCollectionRetryResult
		require.NoError(t, json.Unmarshal(body, &result))
		if i == 0 {
			attemptID = result.Attempt.ID
		} else {
			require.Equal(t, attemptID, result.Attempt.ID)
		}
		require.Equal(t, "paid", result.Invoice.Status)
		require.EqualValues(t, 120000, result.Invoice.AmountPaid)
	}
	require.Len(t, charger.calls, 1)
	require.Equal(t, "JPY", charger.calls[0].Currency)
	require.Equal(t, moneyutil.Cents(10), charger.calls[0].AmountCents)
	status, body = invoiceRequest(t, http.MethodGet, yenPath+"/payments?limit=1&offset=1", reader, "", nil)
	require.Equal(t, 200, status, string(body))
	require.Contains(t, string(body), `"total":2`)
	require.Contains(t, string(body), `"unit_decimals":4`)
	require.Contains(t, string(body), `"amount":20000`)

	blockedCustomer := makeCustomer(dbtest.TestMerchantID, "USD")
	blocked := issue(dbtest.TestMerchantID, blockedCustomer, "USD", 2000000)
	blockedMethod := uuid.New()
	require.NoError(t, paymentmethods.NewPaymentMethodRepo(h.MerchantDB(dbtest.TestMerchantID.UUID())).Create(ctx, &models.PaymentMethod{ID: blockedMethod, CustomerID: blockedCustomer.UUID(), PspID: psp, Rail: models.RailNMI, RailCustomerRef: "vault-" + blockedMethod.String(), RailMethodRef: "method-" + blockedMethod.String(), InitialTransactionID: "initial-" + blockedMethod.String()}))
	_, err = rt.MoneyService.MarkInvoicesPastDue(ctx, time.Now().Add(time.Minute))
	require.NoError(t, err)
	ambiguous := &invoiceAdminCharger{ambiguous: true}
	rt.MoneyCharger = ambiguous
	blockedPath := surface.BaseURL + "/v1/merchant/invoices/" + blocked.ID.String()
	status, body = invoiceRequest(t, http.MethodPost, blockedPath+"/retry-collection", owner, "ambiguous-key", map[string]any{"payment_method_id": blockedMethod})
	require.Equal(t, 409, status, string(body))
	require.Len(t, ambiguous.calls, 1)
	status, body = invoiceRequest(t, http.MethodGet, blockedPath, owner, "", nil)
	require.Equal(t, 200, status, string(body))
	require.NoError(t, json.Unmarshal(body, &read))
	require.Empty(t, read.AvailableActions)
	for _, action := range []string{"void", "uncollectible"} {
		status, body = invoiceRequest(t, http.MethodPost, blockedPath+"/"+action, owner, "", nil)
		require.Equal(t, 409, status, string(body))
	}
	status, body = invoiceRequest(t, http.MethodPost, blockedPath+"/payments", owner, "", map[string]any{"amount": 1000000, "reference": "unknown-bank"})
	require.Equal(t, 409, status, string(body))
	status, body = invoiceRequest(t, http.MethodPost, blockedPath+"/retry-collection", owner, "new-key", map[string]any{"payment_method_id": blockedMethod})
	require.Equal(t, 409, status, string(body))
	require.Len(t, ambiguous.calls, 1)
	uncollectibleCustomer := makeCustomer(dbtest.TestMerchantID, "USD")
	uncollectible := issue(dbtest.TestMerchantID, uncollectibleCustomer, "USD", 3000000)
	for range 2 {
		status, body = invoiceRequest(t, http.MethodPost, surface.BaseURL+"/v1/merchant/invoices/"+uncollectible.ID.String()+"/uncollectible", owner, "", nil)
		require.Equal(t, 200, status, string(body))
		require.Contains(t, string(body), `"status":"uncollectible"`)
	}
	stillOwed, err := money.NewMoneyService(h.MerchantDB(dbtest.TestMerchantID.UUID())).GetOutstandingOwed(ctx, uncollectibleCustomer, "USD")
	require.NoError(t, err)
	require.EqualValues(t, 3000000, stillOwed)

}
