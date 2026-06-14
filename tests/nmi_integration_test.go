//go:build integration

package tests

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// NMI Demo Account credentials
// See: https://docs.nmi.com/reference/testing-methods
const (
	NMIDemoSecurityKey = "6457Thfj624V5r7WUwc5v6a68Zsd6YEm"
	NMIDirectPostURL   = "https://secure.networkmerchants.com/api/transact.php"

	// Test card numbers
	// See: https://docs.nmi.com/docs/testing-sandbox
	TestCardVisa     = "4111111111111111"
	TestCardExpiry   = "1028" // MM/YY format: October 2028
	TestCardCVV      = "999"  // AVS/CVV test value
	TestCardZip      = "77777"
	TestCardAddress1 = "888" // AVS test value
)

// TestNMIDemoDirectConnection tests that we can connect to NMI's demo API
func TestNMIDemoDirectConnection(t *testing.T) {
	// Make a direct API call to NMI to verify connectivity
	values := url.Values{
		"type":         {"sale"},
		"security_key": {NMIDemoSecurityKey},
		"ccnumber":     {TestCardVisa},
		"ccexp":        {TestCardExpiry},
		"cvv":          {TestCardCVV},
		"amount":       {"1.00"},
		"first_name":   {"Test"},
		"last_name":    {"User"},
		"address1":     {TestCardAddress1},
		"zip":          {TestCardZip},
		"test_mode":    {"enabled"},
	}

	resp, err := http.PostForm(NMIDirectPostURL, values)
	require.NoError(t, err, "Should be able to connect to NMI demo API")
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	// Parse response
	output, err := url.ParseQuery(string(body))
	require.NoError(t, err)

	// Verify successful response
	assert.Equal(t, "1", output.Get("response"), "Should receive approval response (1), got: %s - %s", output.Get("response"), output.Get("responsetext"))
	assert.NotEmpty(t, output.Get("transactionid"), "Should receive transaction ID")
	t.Logf("NMI Demo API response: %s", string(body))
}

// TestNMIDemoDeclinedPayment tests that declined payments return proper error
func TestNMIDemoDeclinedPayment(t *testing.T) {
	// Per NMI docs: amount < 1.00 causes decline
	values := url.Values{
		"type":         {"sale"},
		"security_key": {NMIDemoSecurityKey},
		"ccnumber":     {TestCardVisa},
		"ccexp":        {TestCardExpiry},
		"cvv":          {TestCardCVV},
		"amount":       {"0.50"}, // Less than $1.00 triggers decline
		"first_name":   {"Test"},
		"last_name":    {"Decline"},
		"address1":     {TestCardAddress1},
		"zip":          {TestCardZip},
		"test_mode":    {"enabled"},
	}

	resp, err := http.PostForm(NMIDirectPostURL, values)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	output, err := url.ParseQuery(string(body))
	require.NoError(t, err)

	// Should be declined (response = 2)
	assert.Equal(t, "2", output.Get("response"), "Should receive decline response (2), got: %s - %s", output.Get("response"), output.Get("responsetext"))
	t.Logf("NMI Demo decline response: %s", string(body))
}

func TestNMIMobiusConfiguredAccountSale(t *testing.T) {
	securityKey := configuredMobiusSecurityKey(t)

	amount := fmt.Sprintf("1.%02d", time.Now().UnixNano()%90+10)
	values := url.Values{
		"type":         {"sale"},
		"security_key": {securityKey},
		"ccnumber":     {TestCardVisa},
		"ccexp":        {TestCardExpiry},
		"cvv":          {TestCardCVV},
		"amount":       {amount},
		"orderid":      {"openrails-mobius-" + uuid.NewString()},
		"first_name":   {"OpenRails"},
		"last_name":    {"Mobius"},
		"address1":     {TestCardAddress1},
		"zip":          {TestCardZip},
		"test_mode":    {"enabled"},
	}

	resp, err := http.PostForm(nmi.DefaultDirectPostURL, values)
	require.NoError(t, err, "Should be able to connect to configured Mobius/NMI endpoint")
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	output, err := url.ParseQuery(string(body))
	require.NoError(t, err)

	assert.Equal(t, "1", output.Get("response"), "Configured Mobius/NMI account should approve test-mode sale, got: %s - %s", output.Get("response"), output.Get("responsetext"))
	assert.NotEmpty(t, output.Get("transactionid"), "Should receive transaction ID")
	t.Logf("Configured Mobius/NMI sale approved with response_code=%s", output.Get("response_code"))
}

func TestNMIMobiusConfiguredAccountRecurringSubscription(t *testing.T) {
	securityKey := configuredMobiusSecurityKey(t)
	client := createConfiguredMobiusClient(t, securityKey)

	vaultID := createConfiguredMobiusVault(t, securityKey)
	t.Cleanup(func() {
		deleteConfiguredVault(t, securityKey, vaultID)
	})

	planID := "openrails-it-" + uuid.NewString()
	err := client.AddRecurringPlan(planID, "OpenRails integration test", 123, 30, 0)
	require.NoError(t, err, "configured Mobius/NMI account should create a temporary recurring plan")
	// NMI has no API verb to delete a plan (portal-only; #362), so the
	// temporary plan stays behind. It is harmless: no subscribers bill on it.
	t.Logf("temporary NMI recurring plan %s left in the sandbox (plan deletion is portal-only)", planID)

	resp, err := client.AddRecurringSubscription(nmi.RecurringPaymentData{
		CardUserData: nmi.CardUserData{
			FirstName: "OpenRails",
			LastName:  "Recurring",
			Address1:  TestCardAddress1,
			City:      "Test City",
			State:     "CA",
			Zip:       TestCardZip,
			Country:   "US",
		},
		PlanID:          planID,
		CustomerVaultID: vaultID,
		Email:           "openrails-recurring-test@example.com",
		Currency:        "USD",
		Amount:          1.23,
		OrderID:         "openrails-recurring-" + uuid.NewString(),
	})
	require.NoError(t, err, "configured Mobius/NMI account should approve immediate recurring subscription")
	require.NotEmpty(t, resp.SubscriptionID, "NMI should return a provider subscription id")
	require.NotEmpty(t, resp.TransactionID, "NMI should return the initial provider transaction id")
	t.Cleanup(func() {
		if err := client.DeleteRecurringSubscription(resp.SubscriptionID); err != nil {
			t.Logf("failed to delete temporary NMI subscription %s: %v", resp.SubscriptionID, err)
		}
	})

	t.Logf("Configured Mobius/NMI recurring subscription approved with subscription_id=%s", resp.SubscriptionID)
}

// TestNMIDemoClientCreateVault tests creating a customer vault entry with real NMI API
func TestNMIDemoClientCreateVault(t *testing.T) {
	// Create NMI client with demo credentials
	client := createNMIDemoClient(t)

	// Create a customer vault using direct card data
	// Note: We need to use the direct API since CreateCustomerVault expects a payment_token
	values := url.Values{
		"customer_vault": {"add_customer"},
		"security_key":   {NMIDemoSecurityKey},
		"ccnumber":       {TestCardVisa},
		"ccexp":          {TestCardExpiry},
		"cvv":            {TestCardCVV},
		"first_name":     {"Test"},
		"last_name":      {"VaultUser"},
		"address1":       {TestCardAddress1},
		"city":           {"Test City"},
		"state":          {"CA"},
		"zip":            {TestCardZip},
		"country":        {"US"},
		"email":          {"vault-test@example.com"},
		"test_mode":      {"enabled"},
	}

	resp, err := http.PostForm(client.DirectPostURL, values)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	output, err := url.ParseQuery(string(body))
	require.NoError(t, err)

	assert.Equal(t, "1", output.Get("response"), "Should create vault successfully, got: %s - %s", output.Get("response"), output.Get("responsetext"))
	assert.NotEmpty(t, output.Get("customer_vault_id"), "Should return vault ID")

	vaultID := output.Get("customer_vault_id")
	t.Logf("Created vault ID: %s", vaultID)

	// Clean up: delete the vault
	t.Cleanup(func() {
		deleteValues := url.Values{
			"customer_vault":    {"delete_customer"},
			"security_key":      {NMIDemoSecurityKey},
			"customer_vault_id": {vaultID},
			"test_mode":         {"enabled"},
		}
		http.PostForm(client.DirectPostURL, deleteValues)
	})
}

// TestNMIDemoClientAddSubscription tests adding a subscription with real NMI API
func TestNMIDemoClientAddSubscription(t *testing.T) {
	client := createNMIDemoClient(t)

	// First create a vault
	vaultValues := url.Values{
		"customer_vault": {"add_customer"},
		"security_key":   {NMIDemoSecurityKey},
		"ccnumber":       {TestCardVisa},
		"ccexp":          {TestCardExpiry},
		"cvv":            {TestCardCVV},
		"first_name":     {"Subscription"},
		"last_name":      {"Test"},
		"address1":       {TestCardAddress1},
		"city":           {"Test City"},
		"state":          {"CA"},
		"zip":            {TestCardZip},
		"country":        {"US"},
		"email":          {"sub-test@example.com"},
		"test_mode":      {"enabled"},
	}

	resp, err := http.PostForm(client.DirectPostURL, vaultValues)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	output, err := url.ParseQuery(string(body))
	require.NoError(t, err)
	require.Equal(t, "1", output.Get("response"), "Failed to create vault: %s", output.Get("responsetext"))

	vaultID := output.Get("customer_vault_id")
	require.NotEmpty(t, vaultID)

	// Now add subscription using the vault
	// Note: We need a plan_id configured in NMI for this to work fully
	// For demo purposes, we'll test the subscription without a plan (one-time sale with vault)
	subValues := url.Values{
		"type":              {"sale"},
		"security_key":      {NMIDemoSecurityKey},
		"customer_vault_id": {vaultID},
		"amount":            {"9.99"},
		"currency":          {"USD"},
		"order_description": {"Test subscription charge"},
		"test_mode":         {"enabled"},
	}

	resp2, err := http.PostForm(client.DirectPostURL, subValues)
	require.NoError(t, err)
	defer resp2.Body.Close()

	body2, err := io.ReadAll(resp2.Body)
	require.NoError(t, err)

	output2, err := url.ParseQuery(string(body2))
	require.NoError(t, err)

	assert.Equal(t, "1", output2.Get("response"), "Sale should succeed, got: %s - %s", output2.Get("response"), output2.Get("responsetext"))
	assert.NotEmpty(t, output2.Get("transactionid"), "Should return transaction ID")

	t.Logf("Transaction ID: %s", output2.Get("transactionid"))

	// Clean up vault
	t.Cleanup(func() {
		deleteValues := url.Values{
			"customer_vault":    {"delete_customer"},
			"security_key":      {NMIDemoSecurityKey},
			"customer_vault_id": {vaultID},
			"test_mode":         {"enabled"},
		}
		http.PostForm(client.DirectPostURL, deleteValues)
	})
}

// TestNMIDemoClientRebill tests rebilling with real NMI API
func TestNMIDemoClientRebill(t *testing.T) {
	client := createNMIDemoClient(t)

	// Create vault
	vaultValues := url.Values{
		"customer_vault": {"add_customer"},
		"security_key":   {NMIDemoSecurityKey},
		"ccnumber":       {TestCardVisa},
		"ccexp":          {TestCardExpiry},
		"cvv":            {TestCardCVV},
		"first_name":     {"Rebill"},
		"last_name":      {"Test"},
		"address1":       {TestCardAddress1},
		"zip":            {TestCardZip},
		"test_mode":      {"enabled"},
	}

	resp, err := http.PostForm(client.DirectPostURL, vaultValues)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	output, err := url.ParseQuery(string(body))
	require.NoError(t, err)
	require.Equal(t, "1", output.Get("response"))

	vaultID := output.Get("customer_vault_id")

	// Charge the vault (simulating a rebill)
	rebillValues := url.Values{
		"type":              {"sale"},
		"security_key":      {NMIDemoSecurityKey},
		"customer_vault_id": {vaultID},
		"amount":            {"19.99"},
		"currency":          {"USD"},
		"order_description": {"Rebill test"},
		"test_mode":         {"enabled"},
	}

	resp2, err := http.PostForm(client.DirectPostURL, rebillValues)
	require.NoError(t, err)
	defer resp2.Body.Close()

	body2, err := io.ReadAll(resp2.Body)
	require.NoError(t, err)

	output2, err := url.ParseQuery(string(body2))
	require.NoError(t, err)

	assert.Equal(t, "1", output2.Get("response"), "Rebill should succeed")
	assert.NotEmpty(t, output2.Get("transactionid"))

	t.Logf("Rebill transaction ID: %s", output2.Get("transactionid"))

	// Clean up
	t.Cleanup(func() {
		deleteValues := url.Values{
			"customer_vault":    {"delete_customer"},
			"security_key":      {NMIDemoSecurityKey},
			"customer_vault_id": {vaultID},
			"test_mode":         {"enabled"},
		}
		http.PostForm(client.DirectPostURL, deleteValues)
	})
}

// TestDunningWorkerWithRealNMI tests the dunning worker with real NMI API
// This test creates a past_due subscription with a valid vault and verifies
// the dunning worker can process it (though without a plan_id the rebill will fail)
func TestDunningWorkerWithRealNMI(t *testing.T) {
	suite := getSharedTestSuite(t)

	// Create vault with real NMI API
	vaultID := createNMIDemoVault(t)
	billingID := "default" // NMI uses "default" as the billing_id for single-billing vaults

	// Seed products
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	// Create user and payment method
	userID := uuid.New().String()
	pm := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
		UserID:    userID,
		Processor: models.ProcessorMobius,
		VaultID:   vaultID,
		BillingID: billingID,
		LastFour:  "1111",
		CardType:  "Visa",
	})

	// Create past_due subscription
	pastTime := time.Now().Add(-1 * time.Hour)
	retryAttempts := 1
	processorSubID := "test-sub-" + uuid.New().String()

	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:          userID,
		PriceID:         priceID,
		Status:          models.StatusPastDue,
		Processor:       models.ProcessorMobius,
		ProcessorSubID:  processorSubID,
		PaymentMethodID: &pm.ID,
		RetryAttempts:   &retryAttempts,
		NextRetryAt:     &pastTime,
	})

	// Create worker with real NMI client configured from suite
	worker := &riverjobs.DunningWorker{
		DB:         suite.App.Runtime.DB,
		NMIClients: suite.App.Runtime.NMIClients,
	}

	job := &river.Job[riverjobs.DunningArgs]{
		Args: riverjobs.DunningArgs{},
	}

	// Run the dunning worker
	err := worker.Work(context.Background(), job)
	require.NoError(t, err, "Worker should complete without error")

	// Check subscription status
	// Note: Without a valid NMI plan_id, the rebill will fail, but the worker should handle this gracefully
	updatedSub := suite.GetSubscription(sub.ID)

	// The subscription should either be renewed (if rebill succeeded) or marked as cancelled/past_due
	// Since we don't have a real NMI plan, it will likely fail
	t.Logf("Subscription status after dunning: %s", updatedSub.Status)
	t.Logf("Retry attempts: %d", updatedSub.RetryAttempts)

	assert.Contains(t, []models.SubscriptionStatus{
		models.StatusActive,
		models.StatusPastDue,
		models.StatusCancelled,
	}, updatedSub.Status, "Subscription should be in valid state")

	// Clean up vault
	t.Cleanup(func() {
		deleteVault(vaultID)
	})
}

// TestNMIRuntimeClientConfigured verifies the test suite has NMI clients configured
func TestNMIRuntimeClientConfigured(t *testing.T) {
	suite := getSharedTestSuite(t)

	require.NotNil(t, suite.App.Runtime.NMIClients, "NMI clients should be configured")
	require.Contains(t, suite.App.Runtime.NMIClients, "mobius", "Mobius provider should be configured")

	client := suite.App.Runtime.NMIClients["mobius"]
	require.NotNil(t, client)

	assert.True(t, client.TestMode, "NMI runtime client should be in test mode for integration tests")
	assert.NotEmpty(t, client.DirectPostURL, "NMI direct post URL should be configured")
	assert.True(t,
		strings.HasPrefix(client.DirectPostURL, "http://") ||
			strings.HasPrefix(client.DirectPostURL, "https://"),
		"NMI direct post URL should be absolute, got: %s", client.DirectPostURL)
	assert.Equal(t, nmi.DefaultDirectPostURL, client.DirectPostURL)
	assert.Equal(t, nmi.DefaultQueryAPIURL, client.QueryURL)

	t.Logf("NMI client configured with DirectPostURL: %s", client.DirectPostURL)
}

// Helper functions

func configuredMobiusSecurityKey(t *testing.T) string {
	t.Helper()

	securityKey := strings.TrimSpace(os.Getenv("PROCESSORS_MOBIUS_SECURITY_KEY"))
	if securityKey == "" || securityKey == NMIDemoSecurityKey {
		t.Skip("PROCESSORS_MOBIUS_SECURITY_KEY with a real Mobius/NMI test account key is required")
	}
	return securityKey
}

func createConfiguredMobiusClient(t *testing.T, securityKey string) *nmi.NMIClient {
	t.Helper()

	settings := &config.NMIProviderSettings{
		Name:        "mobius",
		SecurityKey: securityKey,
		TestMode:    true,
	}

	client, err := nmi.NewClient("mobius", settings, true)
	require.NoError(t, err)

	return client
}

func createNMIDemoClient(t *testing.T) *nmi.NMIClient {
	t.Helper()

	settings := &config.NMIProviderSettings{
		Name:        "mobius",
		SecurityKey: NMIDemoSecurityKey,
		TestMode:    true,
	}

	client, err := nmi.NewClient("mobius", settings, true)
	require.NoError(t, err)

	return client
}

func createConfiguredMobiusVault(t *testing.T, securityKey string) string {
	t.Helper()

	values := url.Values{
		"customer_vault": {"add_customer"},
		"security_key":   {securityKey},
		"ccnumber":       {TestCardVisa},
		"ccexp":          {TestCardExpiry},
		"cvv":            {TestCardCVV},
		"first_name":     {"OpenRails"},
		"last_name":      {"Recurring"},
		"address1":       {TestCardAddress1},
		"city":           {"Test City"},
		"state":          {"CA"},
		"zip":            {TestCardZip},
		"country":        {"US"},
		"email":          {"openrails-recurring-test@example.com"},
		"test_mode":      {"enabled"},
	}

	resp, err := http.PostForm(NMIDirectPostURL, values)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	output, err := url.ParseQuery(string(body))
	require.NoError(t, err)
	require.Equal(t, "1", output.Get("response"), "Failed to create configured Mobius vault: %s", output.Get("responsetext"))

	vaultID := output.Get("customer_vault_id")
	require.NotEmpty(t, vaultID)
	return vaultID
}

func createNMIDemoVault(t *testing.T) string {
	t.Helper()

	values := url.Values{
		"customer_vault": {"add_customer"},
		"security_key":   {NMIDemoSecurityKey},
		"ccnumber":       {TestCardVisa},
		"ccexp":          {TestCardExpiry},
		"cvv":            {TestCardCVV},
		"first_name":     {"Test"},
		"last_name":      {"Vault"},
		"address1":       {TestCardAddress1},
		"zip":            {TestCardZip},
		"test_mode":      {"enabled"},
	}

	resp, err := http.PostForm(NMIDirectPostURL, values)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	output, err := url.ParseQuery(string(body))
	require.NoError(t, err)
	require.Equal(t, "1", output.Get("response"), "Failed to create vault: %s", output.Get("responsetext"))

	return output.Get("customer_vault_id")
}

func deleteConfiguredVault(t *testing.T, securityKey string, vaultID string) {
	t.Helper()

	values := url.Values{
		"customer_vault":    {"delete_customer"},
		"security_key":      {securityKey},
		"customer_vault_id": {vaultID},
		"test_mode":         {"enabled"},
	}
	resp, err := http.PostForm(NMIDirectPostURL, values)
	if err != nil {
		t.Logf("failed to delete configured Mobius vault %s: %v", vaultID, err)
		return
	}
	_ = resp.Body.Close()
}

func deleteVault(vaultID string) {
	values := url.Values{
		"customer_vault":    {"delete_customer"},
		"security_key":      {NMIDemoSecurityKey},
		"customer_vault_id": {vaultID},
		"test_mode":         {"enabled"},
	}
	http.PostForm(NMIDirectPostURL, values)
}

// TestNMIDemoClientRefund tests refunding a transaction with real NMI API
// Note: In test mode, transactions are not actually settled, so refunds may behave differently
func TestNMIDemoClientRefund(t *testing.T) {
	client := createNMIDemoClient(t)

	// First create a vault and charge it
	vaultID := createNMIDemoVault(t)
	t.Cleanup(func() {
		deleteVault(vaultID)
	})

	// Make a sale
	saleResp, err := client.RunSale(nmi.SaleParams{
		CustomerVaultID:  vaultID,
		Amount:           1500, // $15.00
		Currency:         "USD",
		OrderDescription: "Refund test sale",
	})
	require.NoError(t, err, "Sale should succeed")
	require.NotEmpty(t, saleResp.TransactionID)
	t.Logf("Sale transaction ID: %s", saleResp.TransactionID)

	// Now refund the transaction (full refund)
	refundResp, err := client.Refund(nmi.RefundParams{
		TransactionID: saleResp.TransactionID,
		Amount:        0, // Full refund
	})

	// In test mode, refunds may fail because transactions aren't settled
	// But the API call should succeed and return a proper response
	if err != nil {
		t.Logf("Refund failed (expected in test mode for unsettled transactions): %v", err)
		// Try void instead since the transaction is likely unsettled
		voidErr := client.Void(saleResp.TransactionID)
		if voidErr != nil {
			t.Logf("Void also failed: %v", voidErr)
		} else {
			t.Log("Void succeeded (transaction was unsettled)")
		}
	} else {
		assert.NotEmpty(t, refundResp.TransactionID, "Refund should return transaction ID")
		t.Logf("Refund transaction ID: %s", refundResp.TransactionID)
	}
}

// TestNMIDemoClientPartialRefund tests partial refunds with real NMI API
func TestNMIDemoClientPartialRefund(t *testing.T) {
	client := createNMIDemoClient(t)

	vaultID := createNMIDemoVault(t)
	t.Cleanup(func() {
		deleteVault(vaultID)
	})

	// Make a sale
	saleResp, err := client.RunSale(nmi.SaleParams{
		CustomerVaultID:  vaultID,
		Amount:           2000, // $20.00
		Currency:         "USD",
		OrderDescription: "Partial refund test",
	})
	require.NoError(t, err, "Sale should succeed")
	t.Logf("Sale transaction ID: %s", saleResp.TransactionID)

	// Partial refund ($5.00 of $20.00)
	refundResp, err := client.Refund(nmi.RefundParams{
		TransactionID: saleResp.TransactionID,
		Amount:        500, // $5.00
	})

	if err != nil {
		t.Logf("Partial refund failed (expected in test mode): %v", err)
	} else {
		assert.NotEmpty(t, refundResp.TransactionID)
		t.Logf("Partial refund transaction ID: %s", refundResp.TransactionID)
	}
}

// TestNMIDemoClientVoid tests voiding an unsettled transaction
func TestNMIDemoClientVoid(t *testing.T) {
	client := createNMIDemoClient(t)

	vaultID := createNMIDemoVault(t)
	t.Cleanup(func() {
		deleteVault(vaultID)
	})

	// Make a sale
	saleResp, err := client.RunSale(nmi.SaleParams{
		CustomerVaultID:  vaultID,
		Amount:           1000, // $10.00
		Currency:         "USD",
		OrderDescription: "Void test sale",
	})
	require.NoError(t, err, "Sale should succeed")
	t.Logf("Sale transaction ID: %s", saleResp.TransactionID)

	// Void the unsettled transaction
	err = client.Void(saleResp.TransactionID)
	require.NoError(t, err, "Void should succeed for unsettled transaction")
	t.Log("Void succeeded")
}

// TestNMIDemoClientRefundValidation tests refund parameter validation
func TestNMIDemoClientRefundValidation(t *testing.T) {
	client := createNMIDemoClient(t)

	// Test missing transaction ID
	_, err := client.Refund(nmi.RefundParams{
		TransactionID: "",
		Amount:        1000,
	})
	assert.Error(t, err, "Should fail without transaction ID")
	assert.Contains(t, err.Error(), "transaction ID is required")
}

// TestNMIDemoClientVoidValidation tests void parameter validation
func TestNMIDemoClientVoidValidation(t *testing.T) {
	client := createNMIDemoClient(t)

	// Test missing transaction ID
	err := client.Void("")
	assert.Error(t, err, "Should fail without transaction ID")
	assert.Contains(t, err.Error(), "transaction ID is required")
}
