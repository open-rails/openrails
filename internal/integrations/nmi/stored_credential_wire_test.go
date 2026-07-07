package nmi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Wire-pinning tests for the #297 stored-credential (CIT/MIT) fields: a
// charge.Context-derived StoredCredential in ⇒ EXACTLY the portal-documented
// classic Direct Post fields out (initiated_by / stored_credential_indicator /
// initial_transaction_id / billing_method).

// classicFormServer records every classic Direct Post form and answers an
// approved sale.
func classicFormServer(t *testing.T) (*httptest.Server, *[]url.Values) {
	t.Helper()
	forms := &[]url.Values{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		*forms = append(*forms, r.Form)
		fmt.Fprint(w, "response=1&responsetext=SUCCESS&authcode=OK&transactionid=txn-wire-1&response_code=100")
	}))
	t.Cleanup(server.Close)
	return server, forms
}

func lastForm(t *testing.T, forms *[]url.Values) url.Values {
	t.Helper()
	require.NotEmpty(t, *forms)
	return (*forms)[len(*forms)-1]
}

func TestRunSale_StoredCredentialRoutesClassicWithCITFields(t *testing.T) {
	server, forms := classicFormServer(t)
	client := newTestClient(t, server.URL)

	// Unscheduled initial CIT: indicator=stored, no reference, NO billing_method.
	_, err := client.RunSale(SaleParams{
		CustomerVaultID: "v1",
		Amount:          moneyutil.Cents(1999),
		Currency:        "USD",
		OrderID:         "o1",
		StoredCredential: &StoredCredential{
			InitiatedBy: InitiatedByCustomer,
			Indicator:   IndicatorStored,
		},
	})
	require.NoError(t, err)
	form := lastForm(t, forms)
	assert.Equal(t, "sale", form.Get("type"), "stored-credential sales ride classic Direct Post")
	assert.Equal(t, "v1", form.Get("customer_vault_id"))
	assert.Equal(t, "customer", form.Get("initiated_by"))
	assert.Equal(t, "stored", form.Get("stored_credential_indicator"))
	_, hasInitial := form["initial_transaction_id"]
	assert.False(t, hasInitial, "initial CIT carries no initial_transaction_id")
	_, hasBillingMethod := form["billing_method"]
	assert.False(t, hasBillingMethod, "unscheduled CoF sends no billing_method")
	assert.Equal(t, "19.99", form.Get("amount"))
}

func TestRunSale_StoredCredentialMITCarriesReference(t *testing.T) {
	server, forms := classicFormServer(t)
	client := newTestClient(t, server.URL)

	// Unscheduled MIT: merchant + used + the sequence anchor.
	_, err := client.RunSale(SaleParams{
		CustomerVaultID: "v1",
		BillingID:       "b1",
		Amount:          moneyutil.Cents(500),
		Currency:        "USD",
		OrderID:         "o2",
		StoredCredential: &StoredCredential{
			InitiatedBy:          InitiatedByMerchant,
			Indicator:            IndicatorUsed,
			InitialTransactionID: "1234567890",
		},
	})
	require.NoError(t, err)
	form := lastForm(t, forms)
	assert.Equal(t, "merchant", form.Get("initiated_by"))
	assert.Equal(t, "used", form.Get("stored_credential_indicator"))
	assert.Equal(t, "1234567890", form.Get("initial_transaction_id"))
	assert.Equal(t, "b1", form.Get("billing_id"), "targeted MIT still scopes the exact card")
	_, hasBillingMethod := form["billing_method"]
	assert.False(t, hasBillingMethod, "unscheduled MIT sends no billing_method")
}

func TestRunSale_StoredCredentialValidation(t *testing.T) {
	server, _ := classicFormServer(t)
	client := newTestClient(t, server.URL)

	_, err := client.RunSale(SaleParams{
		CustomerVaultID:  "v1",
		Amount:           moneyutil.Cents(100),
		Currency:         "USD",
		StoredCredential: &StoredCredential{InitiatedBy: "robot", Indicator: IndicatorUsed},
	})
	require.ErrorContains(t, err, "initiated_by")

	_, err = client.RunSale(SaleParams{
		CustomerVaultID:  "v1",
		Amount:           moneyutil.Cents(100),
		Currency:         "USD",
		StoredCredential: &StoredCredential{InitiatedBy: InitiatedByCustomer, Indicator: "maybe"},
	})
	require.ErrorContains(t, err, "indicator")
}

func TestAddRecurringSubscription_StoredCredentialRecurringCIT(t *testing.T) {
	var form url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		form = r.Form
		fmt.Fprint(w, "response=1&responsetext=SUCCESS&subscription_id=sub1&transactionid=t3")
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	_, err := client.AddRecurringSubscription(RecurringPaymentData{
		PlanID:          "plan1",
		CustomerVaultID: "v1",
		Currency:        "USD",
		Amount:          moneyutil.MajorUnits(19.99),
		StoredCredential: &StoredCredential{
			InitiatedBy: InitiatedByCustomer,
			Indicator:   IndicatorStored,
			Recurring:   true,
		},
	})
	require.NoError(t, err)
	// The portal's recurring initial CIT MUST-INCLUDE trio:
	assert.Equal(t, "recurring", form.Get("billing_method"))
	assert.Equal(t, "customer", form.Get("initiated_by"))
	assert.Equal(t, "stored", form.Get("stored_credential_indicator"))
	_, hasInitial := form["initial_transaction_id"]
	assert.False(t, hasInitial)
}

func TestAttemptManualRebill_StoredCredentialRecurringMIT(t *testing.T) {
	var form url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		form = r.Form
		fmt.Fprint(w, "response=1&transactionid=t4")
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	resp, err := client.AttemptManualRebill(ManualRebillParams{
		VaultID:        "v1",
		BillingID:      "b1",
		SubscriptionID: "sub1",
		OrderID:        "ord1",
		StoredCredential: &StoredCredential{
			InitiatedBy:          InitiatedByMerchant,
			Indicator:            IndicatorUsed,
			InitialTransactionID: "9876543210",
			Recurring:            true,
		},
	})
	require.NoError(t, err)
	require.True(t, resp.Success)
	// The portal's recurring MIT combination:
	assert.Equal(t, "rebill_subscription", form.Get("recurring"))
	assert.Equal(t, "recurring", form.Get("billing_method"))
	assert.Equal(t, "merchant", form.Get("initiated_by"))
	assert.Equal(t, "used", form.Get("stored_credential_indicator"))
	assert.Equal(t, "9876543210", form.Get("initial_transaction_id"))
}

func TestAttemptManualRebill_LegacyReferenceLessMITOmitsInitialTransactionID(t *testing.T) {
	var form url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		form = r.Form
		fmt.Fprint(w, "response=1&transactionid=t5")
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	_, err := client.AttemptManualRebill(ManualRebillParams{
		VaultID:        "v1",
		BillingID:      "b1",
		SubscriptionID: "sub1",
		StoredCredential: &StoredCredential{
			InitiatedBy: InitiatedByMerchant,
			Indicator:   IndicatorUsed,
			Recurring:   true,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "merchant", form.Get("initiated_by"))
	assert.Equal(t, "used", form.Get("stored_credential_indicator"))
	_, hasInitial := form["initial_transaction_id"]
	assert.False(t, hasInitial, "legacy instrument charges reference-less; the key is omitted, never sent empty")
}
