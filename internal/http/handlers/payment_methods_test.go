package handlers

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/stretchr/testify/require"
)

// #589: derived payment-method health is computed at query time, so the pure
// derivation logic is unit-tested without a DB.
func TestCardExpiryStatus(t *testing.T) {
	ptr := func(s string) *string { return &s }
	require.Equal(t, "", cardExpiryStatus(nil), "non-card / no expiry")
	require.Equal(t, "", cardExpiryStatus(ptr("not-a-date")), "unparseable")
	require.Equal(t, "expired", cardExpiryStatus(ptr("01/20")), "Jan 2020 is long past")
	require.Equal(t, "valid", cardExpiryStatus(ptr("12/99")), "year 2099 is far future")

	// A card whose month is the current month is still valid through month-end.
	now := time.Now().UTC()
	thisMonth := now.Format("01/06")
	require.NotEqual(t, "expired", cardExpiryStatus(&thisMonth), "valid through end of expiry month")
}

func TestChargeOutcome(t *testing.T) {
	require.Equal(t, "success", chargeOutcome("completed"))
	require.Equal(t, "failed", chargeOutcome("failed"))
	require.Equal(t, "refunded", chargeOutcome("refunded"))
	require.Equal(t, "pending", chargeOutcome("pending"))
}

func TestPaymentMethodHealthFrom(t *testing.T) {
	ptr := func(s string) *string { return &s }
	valid := ptr("12/99")
	expired := ptr("01/20")

	// No charge history, valid card -> active.
	h := paymentMethodHealthFrom(&models.PaymentMethod{ExpiryDate: valid}, nil)
	require.True(t, h.Active)
	require.Nil(t, h.LastChargedAt)
	require.Equal(t, "valid", h.ExpiryStatus)

	// Last charge failed -> not active, outcome surfaced.
	at := time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC)
	h = paymentMethodHealthFrom(&models.PaymentMethod{ExpiryDate: valid}, &models.PaymentMethodCharge{LastChargedAt: at, Status: "failed"})
	require.False(t, h.Active)
	require.Equal(t, "failed", h.LastChargeOutcome)
	require.Equal(t, at, *h.LastChargedAt)

	// Expired card -> not active even with a successful last charge.
	h = paymentMethodHealthFrom(&models.PaymentMethod{ExpiryDate: expired}, &models.PaymentMethodCharge{LastChargedAt: at, Status: "completed"})
	require.False(t, h.Active)
	require.Equal(t, "success", h.LastChargeOutcome)
}

func TestCreateVaultRequestFromPaymentMethodRequestMinimalInternationalMetadata(t *testing.T) {
	cases := []struct {
		name        string
		country     string
		postalCode  string
		legacyZip   string
		wantCountry string
		wantPostal  string
	}{
		{name: "US", country: "US", legacyZip: "80202", wantCountry: "US", wantPostal: "80202"},
		{name: "DE", country: "DE", postalCode: "10115", wantCountry: "DE", wantPostal: "10115"},
		{name: "ES", country: "ES", postalCode: "28013", wantCountry: "ES", wantPostal: "28013"},
		{name: "JP", country: "JP", postalCode: "100-0001", wantCountry: "JP", wantPostal: "100-0001"},
		{name: "KR", country: "KR", postalCode: "04524", wantCountry: "KR", wantPostal: "04524"},
		{name: "CN", country: "CN", postalCode: "100000", wantCountry: "CN", wantPostal: "100000"},
		{name: "BR", country: "BR", postalCode: "01001-000", wantCountry: "BR", wantPostal: "01001-000"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			req := &createPaymentMethodRequest{
				PaymentToken:   "provider-token",
				Provider:       "mobius",
				NameOnCard:     "Ada Lovelace",
				BillingCountry: tt.country,
				PostalCode:     tt.postalCode,
				Zip:            tt.legacyZip,
				Phone:          "+49 30 123456",
				LastFour:       "card-4242",
				CardType:       "Visa",
				ExpiryDate:     "12/30",
			}

			got := createVaultRequestFromPaymentMethodRequest(req, "ada@example.com")

			require.Equal(t, "provider-token", got.PaymentToken)
			require.Equal(t, "mobius", got.Provider)
			require.Equal(t, "Ada Lovelace", got.NameOnCard)
			require.Empty(t, got.FirstName)
			require.Empty(t, got.LastName)
			require.Equal(t, tt.wantCountry, got.Country)
			require.Equal(t, tt.wantPostal, got.Zip)
			require.Equal(t, "4242", got.LastFour)
			require.Equal(t, "ada@example.com", got.Email)
			require.Equal(t, "Ada Lovelace", got.Metadata["name_on_card"])
			require.Equal(t, tt.wantCountry, got.Metadata["billing_country"])
			require.Equal(t, tt.wantPostal, got.Metadata["postal_code"])
			require.Equal(t, "ada@example.com", got.Metadata["billing_email"])
			require.Equal(t, "+49 30 123456", got.Metadata["billing_phone"])
		})
	}
}

func TestCreateVaultRequestFromPaymentMethodRequestSupportsLegacyAddressFields(t *testing.T) {
	req := &createPaymentMethodRequest{
		PaymentToken: "provider-token",
		Provider:     "mobius",
		FirstName:    "Ada",
		LastName:     "Lovelace",
		Address1:     "1 Main",
		Address2:     "Unit 2",
		City:         "Denver",
		State:        "CO",
		Zip:          "80202",
		Country:      "US",
	}

	got := createVaultRequestFromPaymentMethodRequest(req, "ada@example.com")

	require.Equal(t, "Ada", got.FirstName)
	require.Equal(t, "Lovelace", got.LastName)
	require.Equal(t, "US", got.Country)
	require.Equal(t, "80202", got.Zip)
	require.Equal(t, "1 Main", got.Metadata["billing_address1"])
	require.Equal(t, "Unit 2", got.Metadata["billing_address2"])
	require.Equal(t, "Denver", got.Metadata["billing_city"])
	require.Equal(t, "CO", got.Metadata["billing_state"])
}

func TestRejectRawCardFields(t *testing.T) {
	var req createPaymentMethodRequest
	err := json.Unmarshal([]byte(`{
		"payment_token":"provider-token",
		"card_number":"4111111111111111",
		"cvv":"123"
	}`), &req)
	require.NoError(t, err)

	err = req.rejectRawCardFields()
	require.Error(t, err)
	require.Contains(t, err.Error(), "card_number")
}

func TestPaymentMethodToAPIIncludesStoredBillingMetadata(t *testing.T) {
	last4 := "4242"
	cardType := "Visa"
	pm := &models.PaymentMethod{
		Rail:     "mobius",
		LastFour: &last4,
		CardType: &cardType,
		Metadata: map[string]any{
			"name_on_card":     "Ada Lovelace",
			"billing_email":    "ada@example.com",
			"billing_country":  "JP",
			"postal_code":      "100-0001",
			"billing_address1": "1 Chiyoda",
		},
	}

	got := paymentMethodToAPI(pm, nil)

	require.NotNil(t, got.BillingDetails)
	require.Equal(t, "Ada Lovelace", *got.BillingDetails.Name)
	require.Equal(t, "ada@example.com", *got.BillingDetails.Email)
	require.NotNil(t, got.BillingDetails.Address)
	require.Equal(t, "JP", *got.BillingDetails.Address.Country)
	require.Equal(t, "100-0001", *got.BillingDetails.Address.PostalCode)
	require.Equal(t, "1 Chiyoda", *got.BillingDetails.Address.Line1)
	require.Equal(t, "Ada Lovelace", got.Metadata["name_on_card"])
}

func TestPaymentMethodToAPIPrivacyContract(t *testing.T) {
	t.Parallel()

	pspID := uuid.New()
	response := paymentMethodToAPI(&models.PaymentMethod{
		ID:                             uuid.New(),
		Rail:                           models.RailNMI,
		PspID:                          &pspID,
		RailCustomerRef:                "private-customer-ref",
		RailMethodRef:                  "private-method-ref",
		InitialTransactionID:           "private-transaction-ref",
		StoredCredentialRecurringRef:   "private-recurring-ref",
		StoredCredentialUnscheduledRef: "private-unscheduled-ref",
		VaultProvider:                  "private-vault-provider",
		VaultFingerprint:               "private-vault-fingerprint",
		NetworkTokenID:                 "private-network-token",
		NetworkTokenPAR:                "private-network-par",
		CreatedAt:                      time.Now().UTC(),
	}, nil)

	payload, err := json.Marshal(response)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(payload, &got))
	for _, privateField := range []string{
		"psp_id",
		"provider_account_id",
		"account_id",
		"rail_customer_ref",
		"rail_method_ref",
		"secret",
	} {
		require.NotContains(t, got, privateField)
	}
	for _, privateValue := range []string{
		pspID.String(),
		"private-customer-ref",
		"private-method-ref",
		"private-transaction-ref",
		"private-recurring-ref",
		"private-unscheduled-ref",
		"private-vault-provider",
		"private-vault-fingerprint",
		"private-network-token",
		"private-network-par",
	} {
		require.False(t, strings.Contains(string(payload), privateValue))
	}
}

func TestCreateVaultRequestDoesNotStoreRawProviderPayload(t *testing.T) {
	got := createVaultRequestFromPaymentMethodRequest(&createPaymentMethodRequest{
		PaymentToken: "opaque-provider-token",
		Provider:     "mobius",
		NameOnCard:   "Ada Lovelace",
	}, "ada@example.com")

	require.NotContains(t, got.Metadata, "payment_token")
	require.NotContains(t, got.Metadata, "raw_tokenization_payload")
	require.NotContains(t, got.Metadata, "pan")
	require.NotContains(t, got.Metadata, "cvv")
	require.IsType(t, &paymentmethods.CreateVaultRequest{}, got)
}
