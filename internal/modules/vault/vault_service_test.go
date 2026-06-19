package vault

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	secretName, err := merchants.ProviderAccountSecretName("nmi", "live", "mobius-account", "production_key")
	require.NoError(t, err)
	_, err = store.Put(ctx, dbtest.TestMerchantID, secretName, "merchant-mobius-key")
	require.NoError(t, err)

	seen := make(chan url.Values, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		seen <- r.PostForm
		_, _ = io.WriteString(w, "response=1&customer_vault_id=vault_123")
	}))
	defer server.Close()

	pms := &fakePaymentMethodStore{}
	svc := &VaultService{
		PaymentMethodService: pms,
		MerchantSecrets:      store,
		ProviderSecrets:      vaultStaticProviderSecretResolver{providerType: "nmi", environment: "live", accountID: "mobius-account"},
		Config:               vaultTestConfig(true),
		Processors:           vaultTestProcessors(""),
		newNMIClient: func(provider string, cfg *config.NMIProviderSettings, testMode bool) (*nmi.NMIClient, error) {
			client, err := nmi.NewClient(provider, cfg, testMode)
			if err != nil {
				return nil, err
			}
			client.DirectPostURL = server.URL
			return client, nil
		},
	}

	pm, err := svc.CreateVault(ctx, "11111111-1111-1111-1111-111111111111", &CreateVaultRequest{
		Provider:     "mobius",
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
	require.Equal(t, "vault_123", pm.VaultID)
	require.Len(t, pms.created, 1)

	form := <-seen
	require.Equal(t, "merchant-mobius-key", form.Get("security_key"))
	require.Equal(t, "provider-token", form.Get("payment_token"))
	require.Equal(t, "Ada", form.Get("first_name"))
	require.Equal(t, "Lovelace", form.Get("last_name"))
	require.Equal(t, "4242", *pm.LastFour)
	require.Equal(t, "Visa", *pm.CardType)
	require.Equal(t, "12/30", *pm.ExpiryDate)
	require.Equal(t, "Ada Lovelace", pm.Metadata["name_on_card"])
}

func TestVaultFallsBackToStaticMobiusClientWhenMerchantSecretMissing(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := &VaultService{
		Config:     vaultTestConfig(true),
		Processors: vaultTestProcessors("static-mobius-key"),
	}

	client, err := svc.resolveNMIClient(ctx, "mobius")
	require.NoError(t, err)
	require.Equal(t, "static-mobius-key", client.SecurityKey)
}

func TestVaultProviderAccountResolverMissingMobiusSecretDoesNotUseStaticClient(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := &VaultService{
		MerchantSecrets: merchants.NewMemorySecretStore(),
		ProviderSecrets: vaultMissingProviderSecretResolver{},
		Config:          vaultTestConfig(true),
		Processors:      vaultTestProcessors("static-mobius-key"),
	}

	_, err := svc.resolveNMIClient(ctx, "mobius")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing scoped merchant NMI secret")
}

func TestVaultMissingMerchantSecretAndNoStaticClientReturnsMissingClient(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := &VaultService{
		MerchantSecrets: merchants.NewMemorySecretStore(),
	}

	_, err := svc.resolveNMIClient(ctx, "mobius")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing client")
}

func TestVaultFailsClosedWhenMerchantSecretBackendUnavailable(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := &VaultService{
		MerchantSecrets: vaultUnavailableSecretStore{},
		ProviderSecrets: vaultStaticProviderSecretResolver{providerType: "nmi", environment: "live", accountID: "mobius-account"},
		Config:          vaultTestConfig(true),
		Processors:      vaultTestProcessors("static-mobius-key"),
	}

	_, err := svc.resolveNMIClient(ctx, "mobius")
	require.Error(t, err)
	require.True(t, errors.Is(err, merchants.ErrSecretBackendUnavailable), "err = %v", err)
}

func TestVaultMerchantSecretResolutionIsMerchantScoped(t *testing.T) {
	merchantA := merchant.ID(uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"))
	merchantB := merchant.ID(uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"))
	store := merchants.NewMemorySecretStore()
	nameA, err := merchants.ProviderAccountSecretName("nmi", "live", "mobius-a", "production_key")
	require.NoError(t, err)
	_, err = store.Put(context.Background(), merchantA, nameA, "merchant-a-key")
	require.NoError(t, err)
	nameB, err := merchants.ProviderAccountSecretName("nmi", "live", "mobius-b", "production_key")
	require.NoError(t, err)
	_, err = store.Put(context.Background(), merchantB, nameB, "merchant-b-key")
	require.NoError(t, err)

	svc := &VaultService{
		MerchantSecrets: store,
		ProviderSecrets: vaultPerMerchantProviderSecretResolver{
			merchantA: "mobius-a",
			merchantB: "mobius-b",
		},
		Config:     vaultTestConfig(true),
		Processors: vaultTestProcessors(""),
	}

	clientA, err := svc.resolveNMIClient(merchant.WithID(context.Background(), merchantA), "mobius")
	require.NoError(t, err)
	clientB, err := svc.resolveNMIClient(merchant.WithID(context.Background(), merchantB), "mobius")
	require.NoError(t, err)
	require.Equal(t, "merchant-a-key", clientA.SecurityKey)
	require.Equal(t, "merchant-b-key", clientB.SecurityKey)
}

type vaultStaticProviderSecretResolver struct {
	providerType string
	environment  string
	accountID    string
}

func (r vaultStaticProviderSecretResolver) PrimaryProviderAccountSecretName(_ context.Context, _ merchant.ID, _, _, key string) (string, bool, error) {
	name, err := merchants.ProviderAccountSecretName(r.providerType, r.environment, r.accountID, key)
	return name, err == nil, err
}

type vaultMissingProviderSecretResolver struct{}

func (vaultMissingProviderSecretResolver) PrimaryProviderAccountSecretName(context.Context, merchant.ID, string, string, string) (string, bool, error) {
	return "", false, nil
}

type vaultPerMerchantProviderSecretResolver map[merchant.ID]string

func (r vaultPerMerchantProviderSecretResolver) PrimaryProviderAccountSecretName(_ context.Context, id merchant.ID, _, _, key string) (string, bool, error) {
	accountID := r[id]
	if accountID == "" {
		return "", false, nil
	}
	name, err := merchants.ProviderAccountSecretName("nmi", "live", accountID, key)
	return name, err == nil, err
}

func vaultTestConfig(testEnv bool) *config.Config {
	return &config.Config{
		Mode:    config.ModeFull,
		TestEnv: testEnv,
	}
}

func vaultTestProcessors(mobiusKey string) config.ProcessorSet {
	return config.ProcessorSet{
		"mobius": {
			Type:        config.ProcessorTypeNMI,
			SecurityKey: mobiusKey,
		},
	}
}
