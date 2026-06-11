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
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/tenancy"
	"github.com/open-rails/openrails/pkg/tenant"
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

func (vaultUnavailableSecretStore) Get(context.Context, tenant.ID, string) (tenancy.Secret, error) {
	return tenancy.Secret{}, tenancy.ErrSecretBackendUnavailable
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

func TestCreateVaultUsesTenantSecretMobiusKeyWithoutStaticClient(t *testing.T) {
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	store := tenancy.NewMemorySecretStore()
	_, err := store.Put(ctx, tenant.DefaultID, tenancy.SecretNMIMobiusProductionKey, "tenant-mobius-key")
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
		TenantSecrets:        store,
		Config:               vaultTestConfig(true, ""),
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
	require.Equal(t, "tenant-mobius-key", form.Get("security_key"))
	require.Equal(t, "provider-token", form.Get("payment_token"))
	require.Equal(t, "Ada", form.Get("first_name"))
	require.Equal(t, "Lovelace", form.Get("last_name"))
	require.Equal(t, "4242", *pm.LastFour)
	require.Equal(t, "Visa", *pm.CardType)
	require.Equal(t, "12/30", *pm.ExpiryDate)
	require.Equal(t, "Ada Lovelace", pm.Metadata["name_on_card"])
}

func TestVaultFallsBackToStaticMobiusClientWhenTenantSecretMissing(t *testing.T) {
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	svc := &VaultService{
		Config: vaultTestConfig(true, "static-mobius-key"),
	}

	client, err := svc.resolveNMIClient(ctx, "mobius")
	require.NoError(t, err)
	require.Equal(t, "static-mobius-key", client.SecurityKey)
}

func TestVaultMissingTenantSecretAndNoStaticClientReturnsMissingClient(t *testing.T) {
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	svc := &VaultService{
		TenantSecrets: tenancy.NewMemorySecretStore(),
	}

	_, err := svc.resolveNMIClient(ctx, "mobius")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing client")
}

func TestVaultFailsClosedWhenTenantSecretBackendUnavailable(t *testing.T) {
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	svc := &VaultService{
		TenantSecrets: vaultUnavailableSecretStore{},
		Config:        vaultTestConfig(true, "static-mobius-key"),
	}

	_, err := svc.resolveNMIClient(ctx, "mobius")
	require.Error(t, err)
	require.True(t, errors.Is(err, tenancy.ErrSecretBackendUnavailable), "err = %v", err)
}

func TestVaultTenantSecretResolutionIsTenantScoped(t *testing.T) {
	tenantA := tenant.ID(uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"))
	tenantB := tenant.ID(uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"))
	store := tenancy.NewMemorySecretStore()
	_, err := store.Put(context.Background(), tenantA, tenancy.SecretNMIMobiusProductionKey, "tenant-a-key")
	require.NoError(t, err)
	_, err = store.Put(context.Background(), tenantB, tenancy.SecretNMIMobiusProductionKey, "tenant-b-key")
	require.NoError(t, err)

	svc := &VaultService{
		TenantSecrets: store,
		Config:        vaultTestConfig(true, ""),
	}

	clientA, err := svc.resolveNMIClient(tenant.WithID(context.Background(), tenantA), "mobius")
	require.NoError(t, err)
	clientB, err := svc.resolveNMIClient(tenant.WithID(context.Background(), tenantB), "mobius")
	require.NoError(t, err)
	require.Equal(t, "tenant-a-key", clientA.SecurityKey)
	require.Equal(t, "tenant-b-key", clientB.SecurityKey)
}

func vaultTestConfig(testEnv bool, mobiusKey string) *config.Config {
	return &config.Config{
		Mode:    config.ModeFull,
		TestEnv: testEnv,
		Processors: map[string]*config.ProcessorConfig{
			"mobius": {
				Type:        config.ProcessorTypeNMI,
				SecurityKey: mobiusKey,
			},
		},
	}
}
