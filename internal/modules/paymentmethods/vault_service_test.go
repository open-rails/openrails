package paymentmethods

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

type fakePaymentMethodStore struct {
	created []*models.PaymentMethod
}

func (f *fakePaymentMethodStore) Create(_ context.Context, method *models.PaymentMethod) error {
	f.created = append(f.created, method)
	return nil
}

func (f *fakePaymentMethodStore) Update(_ context.Context, _ *models.PaymentMethod) error {
	return nil
}

func (f *fakePaymentMethodStore) Delete(_ context.Context, _ uuid.UUID) error {
	return nil
}

func (f *fakePaymentMethodStore) GetByUserID(_ context.Context, _ string) ([]*models.PaymentMethod, error) {
	return nil, nil
}

type vaultUnavailableSecretStore struct{}

func (vaultUnavailableSecretStore) Get(context.Context, merchant.ID, string) (merchants.Secret, error) {
	return merchants.Secret{}, merchants.ErrSecretBackendUnavailable
}

func TestApplyUpdatedCardMetadataReplacesStoredCardDetails(t *testing.T) {
	lastFour := "4242"
	cardType := "Visa"
	expiryDate := "12/30"
	pm := &models.PaymentMethod{}

	applyUpdatedCardMetadata(pm, &UpdateVaultRequest{
		LastFour:   &lastFour,
		CardType:   &cardType,
		ExpiryDate: &expiryDate,
	})

	require.NotNil(t, pm.LastFour)
	require.Equal(t, "4242", *pm.LastFour)
	require.NotNil(t, pm.CardType)
	require.Equal(t, "Visa", *pm.CardType)
	require.NotNil(t, pm.ExpiryDate)
	require.Equal(t, "12/30", *pm.ExpiryDate)
}

func TestApplyUpdatedCardMetadataClearsOmittedCardDetails(t *testing.T) {
	oldLastFour := "1111"
	oldCardType := "Visa"
	oldExpiryDate := "01/29"
	pm := &models.PaymentMethod{
		LastFour:   &oldLastFour,
		CardType:   &oldCardType,
		ExpiryDate: &oldExpiryDate,
	}

	applyUpdatedCardMetadata(pm, &UpdateVaultRequest{})

	require.Nil(t, pm.LastFour)
	require.Nil(t, pm.CardType)
	require.Nil(t, pm.ExpiryDate)
}

func TestCreateVaultUsesMerchantSecretMobiusKeyWithoutStaticClient(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	store := merchants.NewMemorySecretStore()
	secretName, err := merchants.RailMerchantAccountSecretName("nmi", "live", "mobius-account", "security_key")
	require.NoError(t, err)
	_, err = store.Put(ctx, dbtest.TestMerchantID, secretName, "merchant-mobius-key")
	require.NoError(t, err)

	type v5CreateSeen struct {
		auth string
		body map[string]any
	}
	seen := make(chan v5CreateSeen, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/customers", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		seen <- v5CreateSeen{auth: r.Header.Get("Authorization"), body: body}
		_, _ = io.WriteString(w, `{"object":"customer","id":"vault_123","billing":[{"object":"billing","id":"billing_456","priority":1}]}`)
	}))
	defer server.Close()

	pms := &fakePaymentMethodStore{}
	svc := &VaultService{
		PaymentMethodService: pms,
		MerchantSecrets:      store,
		ProviderSecrets:      vaultStaticProviderSecretResolver{rail: "nmi", environment: "live", accountID: "mobius-account"},
		Config:               vaultTestConfig(true),
		Rails:                vaultTestRails(""),
		newNMIClient: func(provider string, cfg *config.NMIProviderSettings, testMode bool) (*nmi.NMIClient, error) {
			client, err := nmi.NewClient(provider, cfg, testMode)
			if err != nil {
				return nil, err
			}
			client.DirectPostURL = server.URL
			client.V5BaseURL = server.URL
			return client, nil
		},
	}

	pm, err := svc.CreateVault(ctx, "11111111-1111-1111-1111-111111111111", &CreateVaultRequest{
		Provider:     "nmi",
		PaymentToken: "provider-token",
		NameOnCard:   "Ada Lovelace",
		LastFour:     "xx4242",
		CardType:     "Visa",
		ExpiryDate:   "12/30",
		Metadata: map[string]any{
			"name_on_card": "Ada Lovelace",
		},
	})
	require.NoError(t, err)
	require.Equal(t, "vault_123", pm.RailCustomerRef)
	require.Equal(t, "billing_456", pm.RailMethodRef, "billing id recorded verbatim (#682)")
	require.Equal(t, models.RebillDriverProvider, pm.RebillDriver, "native vaults stay provider-billed regardless of billing id")
	require.Len(t, pms.created, 1)

	got := <-seen
	// The merchant-secret key is the ENTIRE Authorization header (v5).
	require.Equal(t, "merchant-mobius-key", got.auth)
	billing, ok := got.body["billing"].(map[string]any)
	require.True(t, ok, "v5 create-customer body must carry a billing object: %v", got.body)
	paymentDetails, ok := billing["payment_details"].(map[string]any)
	require.True(t, ok, "billing must carry payment_details: %v", billing)
	require.Equal(t, "provider-token", paymentDetails["payment_token"])
	require.Equal(t, "Ada", billing["first_name"])
	require.Equal(t, "Lovelace", billing["last_name"])
	require.Equal(t, "4242", *pm.LastFour)
	require.Equal(t, "Visa", *pm.CardType)
	require.Equal(t, "12/30", *pm.ExpiryDate)
	require.Equal(t, "Ada Lovelace", pm.Metadata["name_on_card"])
}

func TestVaultFallsBackToStaticMobiusClientWhenMerchantSecretMissing(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := &VaultService{
		Config: vaultTestConfig(true),
		Rails:  vaultTestRails("static-mobius-key"),
	}

	client, _, err := svc.resolveNMIClient(ctx, "nmi")
	require.NoError(t, err)
	require.Equal(t, "static-mobius-key", client.SecurityKey)
}

func TestVaultRailMerchantAccountResolverMissingMobiusSecretDoesNotUseStaticClient(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := &VaultService{
		MerchantSecrets: merchants.NewMemorySecretStore(),
		ProviderSecrets: vaultMissingProviderSecretResolver{},
		Config:          vaultTestConfig(true),
		Rails:           vaultTestRails("static-mobius-key"),
	}

	_, _, err := svc.resolveNMIClient(ctx, "nmi")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing scoped merchant NMI provider account")
}

func TestVaultMissingMerchantSecretAndNoStaticClientReturnsMissingClient(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := &VaultService{
		MerchantSecrets: merchants.NewMemorySecretStore(),
	}

	_, _, err := svc.resolveNMIClient(ctx, "nmi")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing client")
}

func TestVaultFailsClosedWhenMerchantSecretBackendUnavailable(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := &VaultService{
		MerchantSecrets: vaultUnavailableSecretStore{},
		ProviderSecrets: vaultStaticProviderSecretResolver{rail: "nmi", environment: "live", accountID: "mobius-account"},
		Config:          vaultTestConfig(true),
		Rails:           vaultTestRails("static-mobius-key"),
	}

	_, _, err := svc.resolveNMIClient(ctx, "nmi")
	require.Error(t, err)
	require.True(t, errors.Is(err, merchants.ErrSecretBackendUnavailable), "err = %v", err)
}

func TestVaultMerchantSecretResolutionIsMerchantScoped(t *testing.T) {
	merchantA := merchant.ID(uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"))
	merchantB := merchant.ID(uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"))
	store := merchants.NewMemorySecretStore()
	nameA, err := merchants.RailMerchantAccountSecretName("nmi", "live", "mobius-a", "security_key")
	require.NoError(t, err)
	_, err = store.Put(context.Background(), merchantA, nameA, "merchant-a-key")
	require.NoError(t, err)
	nameB, err := merchants.RailMerchantAccountSecretName("nmi", "live", "mobius-b", "security_key")
	require.NoError(t, err)
	_, err = store.Put(context.Background(), merchantB, nameB, "merchant-b-key")
	require.NoError(t, err)

	svc := &VaultService{
		MerchantSecrets: store,
		ProviderSecrets: vaultPerMerchantProviderSecretResolver{
			merchantA: "mobius-a",
			merchantB: "mobius-b",
		},
		Config: vaultTestConfig(true),
		Rails:  vaultTestRails(""),
	}

	clientA, _, err := svc.resolveNMIClient(merchant.WithID(context.Background(), merchantA), "nmi")
	require.NoError(t, err)
	clientB, _, err := svc.resolveNMIClient(merchant.WithID(context.Background(), merchantB), "nmi")
	require.NoError(t, err)
	require.Equal(t, "merchant-a-key", clientA.SecurityKey)
	require.Equal(t, "merchant-b-key", clientB.SecurityKey)
}

type vaultStaticProviderSecretResolver struct {
	rail        string
	environment string
	accountID   string
}

func (r vaultStaticProviderSecretResolver) ActiveRailMerchantAccountSecretName(_ context.Context, _ merchant.ID, _, _, key string) (string, bool, error) {
	name, err := merchants.RailMerchantAccountSecretName(r.rail, r.environment, r.accountID, key)
	return name, err == nil, err
}

func (r vaultStaticProviderSecretResolver) ActiveRailMerchantAccountScope(context.Context, merchant.ID, string, string) (merchants.RailMerchantAccountScope, bool, error) {
	return merchants.RailMerchantAccountScope{
		Rail:        r.rail,
		Environment: r.environment,
		AccountID:   r.accountID,
	}, true, nil
}

type vaultMissingProviderSecretResolver struct{}

func (vaultMissingProviderSecretResolver) ActiveRailMerchantAccountSecretName(context.Context, merchant.ID, string, string, string) (string, bool, error) {
	return "", false, nil
}

func (vaultMissingProviderSecretResolver) ActiveRailMerchantAccountScope(context.Context, merchant.ID, string, string) (merchants.RailMerchantAccountScope, bool, error) {
	return merchants.RailMerchantAccountScope{}, false, nil
}

type vaultPerMerchantProviderSecretResolver map[merchant.ID]string

func (r vaultPerMerchantProviderSecretResolver) ActiveRailMerchantAccountSecretName(_ context.Context, id merchant.ID, _, _, key string) (string, bool, error) {
	accountID := r[id]
	if accountID == "" {
		return "", false, nil
	}
	name, err := merchants.RailMerchantAccountSecretName("nmi", "live", accountID, key)
	return name, err == nil, err
}

func (r vaultPerMerchantProviderSecretResolver) ActiveRailMerchantAccountScope(_ context.Context, id merchant.ID, _, _ string) (merchants.RailMerchantAccountScope, bool, error) {
	accountID := r[id]
	if accountID == "" {
		return merchants.RailMerchantAccountScope{}, false, nil
	}
	return merchants.RailMerchantAccountScope{Rail: "nmi", Environment: "live", AccountID: accountID}, true, nil
}

func vaultTestConfig(testMode bool) *config.Config {
	return &config.Config{
		Mode:     config.ModeFull,
		TestMode: testMode,
	}
}

func vaultTestRails(mobiusKey string) config.RailMerchantAccountSet {
	return config.RailMerchantAccountSet{
		"mobius": {
			Rail: models.RailNMI,
			NMI:  &config.NMIRailConfig{SecurityKey: mobiusKey},
		},
	}
}
