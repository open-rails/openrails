package checkout

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

type checkoutFakeVaultKV struct {
	data map[string]map[string]string
}

func newCheckoutFakeVaultKV() *checkoutFakeVaultKV {
	return &checkoutFakeVaultKV{data: map[string]map[string]string{}}
}

func (f *checkoutFakeVaultKV) ReadSecret(_ context.Context, path string) (map[string]string, error) {
	if value, ok := f.data[path]; ok {
		return value, nil
	}
	return nil, nil
}

func (f *checkoutFakeVaultKV) WriteSecret(_ context.Context, path string, data map[string]string) error {
	f.data[path] = data
	return nil
}

func (f *checkoutFakeVaultKV) DeleteSecret(_ context.Context, path string) error {
	delete(f.data, path)
	return nil
}

func (f *checkoutFakeVaultKV) ListSecrets(_ context.Context, prefix string) ([]string, error) {
	var out []string
	for path := range f.data {
		if len(path) > len(prefix) && path[:len(prefix)] == prefix {
			out = append(out, path[len(prefix):])
		}
	}
	return out, nil
}

type checkoutSlugResolver map[string]string

func (r checkoutSlugResolver) MerchantSlug(_ context.Context, id merchant.ID) (string, error) {
	return r[id.String()], nil
}

type unavailableSecretStore struct{}

func (unavailableSecretStore) Get(context.Context, merchant.ID, string) (merchants.Secret, error) {
	return merchants.Secret{}, merchants.ErrSecretBackendUnavailable
}

type checkoutStaticProviderSecretResolver struct {
	providerType string
	environment  string
	accountID    string
}

func (r checkoutStaticProviderSecretResolver) PrimaryProviderAccountSecretName(_ context.Context, _ merchant.ID, providerType, environment, key string) (string, bool, error) {
	name, err := merchants.ProviderAccountSecretName(r.providerType, r.environment, r.accountID, key)
	return name, err == nil, err
}

type checkoutMissingProviderSecretResolver struct{}

func (checkoutMissingProviderSecretResolver) PrimaryProviderAccountSecretName(context.Context, merchant.ID, string, string, string) (string, bool, error) {
	return "", false, nil
}

func TestCheckoutResolvesMobiusClientFromVaultMerchantSecret(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	fakeKV := newCheckoutFakeVaultKV()
	store := merchants.NewVaultSecretStore("secret", fakeKV, checkoutSlugResolver{dbtest.TestMerchantID.String(): "cozy-art"})
	secretName, err := merchants.ProviderAccountSecretName("nmi", "live", "mobius-account", "production_key")
	require.NoError(t, err)
	_, err = store.Put(ctx, dbtest.TestMerchantID, secretName, "merchant-mobius-key")
	require.NoError(t, err)

	svc := &CheckoutService{Config: checkoutRailConfig(true), Rails: checkoutRailSet("static-mobius-key")}
	svc.SetMerchantSecretStore(store)
	svc.SetProviderAccountSecretResolver(checkoutStaticProviderSecretResolver{providerType: "nmi", environment: "live", accountID: "mobius-account"})

	client, err := svc.resolveNMIClient(ctx, "mobius")
	require.NoError(t, err)
	require.Equal(t, "merchant-mobius-key", client.SecurityKey)
	require.Contains(t, fakeKV.data, "secret/openrails/merchants/cozy-art/provider_accounts/nmi/live/mobius-account/production_key")
}

func TestCheckoutFallsBackToStaticMobiusClientWhenMerchantSecretMissing(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := &CheckoutService{Config: checkoutRailConfig(true), Rails: checkoutRailSet("static-mobius-key")}

	client, err := svc.resolveNMIClient(ctx, "mobius")
	require.NoError(t, err)
	require.Equal(t, "static-mobius-key", client.SecurityKey)
}

func TestCheckoutProviderAccountResolverMissingMobiusSecretDoesNotUseStaticClient(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := &CheckoutService{Config: checkoutRailConfig(true), Rails: checkoutRailSet("static-mobius-key")}
	svc.SetMerchantSecretStore(merchants.NewMemorySecretStore())
	svc.SetProviderAccountSecretResolver(checkoutMissingProviderSecretResolver{})

	_, err := svc.resolveNMIClient(ctx, "mobius")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing scoped merchant NMI secret")
}

func TestCheckoutFailsClosedWhenMerchantSecretBackendUnavailable(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := &CheckoutService{Config: checkoutRailConfig(true), Rails: checkoutRailSet("static-mobius-key")}
	svc.SetMerchantSecretStore(unavailableSecretStore{})
	svc.SetProviderAccountSecretResolver(checkoutStaticProviderSecretResolver{providerType: "nmi", environment: "live", accountID: "mobius-account"})

	_, err := svc.resolveNMIClient(ctx, "mobius")
	require.Error(t, err)
	require.True(t, errors.Is(err, merchants.ErrSecretBackendUnavailable), "err = %v", err)
}

func TestCheckoutResolvesCCBillConfigFromMerchantSecret(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	store := merchants.NewMemorySecretStore()
	secretName, err := merchants.ProviderAccountSecretName("ccbill", "live", "merchant-acc/merchant-sub", "account_config")
	require.NoError(t, err)
	_, err = store.Put(ctx, dbtest.TestMerchantID, secretName, `{
		"client_acc_num": "merchant-acc",
		"client_sub_acc": "merchant-sub",
		"salt": "merchant-salt"
	}`)
	require.NoError(t, err)

	svc := &CheckoutService{Config: checkoutRailConfig(true), Rails: checkoutRailSet("static-mobius-key")}
	svc.SetMerchantSecretStore(store)
	svc.SetProviderAccountSecretResolver(checkoutStaticProviderSecretResolver{providerType: "ccbill", environment: "live", accountID: "merchant-acc/merchant-sub"})

	client, err := svc.resolveCCBillClient(ctx)
	require.NoError(t, err)
	resp, err := client.GenerateFlexFormURL(&ccbill.GenerateFlexFormURLParams{
		Username: "alice",
		Email:    "alice@example.com",
		FormName: "premium",
		FlexID:   "flex-123",
	})
	require.NoError(t, err)
	parsed, err := url.Parse(resp.RedirectURL)
	require.NoError(t, err)
	require.Equal(t, "merchant-acc", parsed.Query().Get("clientAccnum"))
	require.Equal(t, "merchant-sub", parsed.Query().Get("clientSubacc"))
	require.NotEmpty(t, parsed.Query().Get("signature"))
}

func TestCheckoutProviderAccountResolverMissingCCBillSecretDoesNotUseStaticConfig(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := &CheckoutService{Config: checkoutRailConfig(true), Rails: checkoutRailSet("static-mobius-key")}
	svc.SetMerchantSecretStore(merchants.NewMemorySecretStore())
	svc.SetProviderAccountSecretResolver(checkoutMissingProviderSecretResolver{})

	_, err := svc.resolveCCBillClient(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing scoped merchant CCBill account_config secret")
}

func TestCheckoutCCBillSubscriptionUsesMerchantSecret(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	store := merchants.NewMemorySecretStore()
	secretName, err := merchants.ProviderAccountSecretName("ccbill", "live", "merchant-acc/merchant-sub", "account_config")
	require.NoError(t, err)
	_, err = store.Put(ctx, dbtest.TestMerchantID, secretName, `{
		"client_acc_num": "merchant-acc",
		"client_sub_acc": "merchant-sub"
	}`)
	require.NoError(t, err)

	svc := &CheckoutService{Config: checkoutRailConfig(true), Rails: checkoutRailSet("static-mobius-key")}
	svc.SetMerchantSecretStore(store)
	svc.SetProviderAccountSecretResolver(checkoutStaticProviderSecretResolver{providerType: "ccbill", environment: "live", accountID: "merchant-acc/merchant-sub"})

	email := "alice@example.com"
	resp, err := svc.processCCBillSubscription(ctx, &CheckoutRequest{}, &UserIdentity{
		ID:       "user-1",
		Email:    &email,
		Username: "alice",
	}, &models.Price{
		ID: uuid.New(),
		Rails: map[string]map[string]string{
			"ccbill": {
				models.RailKeyCCBillFormName: "premium",
				models.RailKeyCCBillFlexID:   "flex-123",
			},
		},
	})
	require.NoError(t, err)
	parsed, err := url.Parse(resp.RedirectURL)
	require.NoError(t, err)
	require.Equal(t, "merchant-acc", parsed.Query().Get("clientAccnum"))
	require.Equal(t, "merchant-sub", parsed.Query().Get("clientSubacc"))
}

func checkoutRailConfig(testMode bool) *config.Config {
	return &config.Config{
		Mode:     config.ModeFull,
		TestMode: testMode,
	}
}

func checkoutRailSet(mobiusKey string) config.RailSet {
	return config.RailSet{
		"mobius": {
			Type:        config.RailTypeNMI,
			SecurityKey: mobiusKey,
		},
		"ccbill": {
			Type:         config.RailTypeCCBill,
			ClientAccNum: "static-acc",
			ClientSubAcc: "static-sub",
			Salt:         "static-salt",
		},
	}
}
