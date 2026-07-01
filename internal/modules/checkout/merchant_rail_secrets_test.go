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
	rail        string
	environment string
	accountID   string
	settings    map[string]any
}

func (r checkoutStaticProviderSecretResolver) ActiveProviderAccountSecretName(_ context.Context, _ merchant.ID, rail, environment, key string) (string, bool, error) {
	name, err := merchants.ProviderAccountSecretName(r.rail, r.environment, r.accountID, key)
	return name, err == nil, err
}

func (r checkoutStaticProviderSecretResolver) ActiveProviderAccountScope(context.Context, merchant.ID, string, string) (merchants.ProviderAccountScope, bool, error) {
	return merchants.ProviderAccountScope{
		Rail:        r.rail,
		Environment: r.environment,
		AccountID:   r.accountID,
		Settings:    r.settings,
	}, true, nil
}

type checkoutMissingProviderSecretResolver struct{}

func (checkoutMissingProviderSecretResolver) ActiveProviderAccountSecretName(context.Context, merchant.ID, string, string, string) (string, bool, error) {
	return "", false, nil
}

func (checkoutMissingProviderSecretResolver) ActiveProviderAccountScope(context.Context, merchant.ID, string, string) (merchants.ProviderAccountScope, bool, error) {
	return merchants.ProviderAccountScope{}, false, nil
}

func TestCheckoutResolvesMobiusClientFromVaultMerchantSecret(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	fakeKV := newCheckoutFakeVaultKV()
	store := merchants.NewVaultSecretStore("secret", fakeKV, checkoutSlugResolver{dbtest.TestMerchantID.String(): "cozy-art"})
	secretName, err := merchants.ProviderAccountSecretName("nmi", "live", "mobius-account", "security_key")
	require.NoError(t, err)
	_, err = store.Put(ctx, dbtest.TestMerchantID, secretName, "merchant-mobius-key")
	require.NoError(t, err)

	svc := &CheckoutService{Config: checkoutRailConfig(true), Rails: checkoutRailSet("static-mobius-key")}
	svc.SetMerchantSecretStore(store)
	svc.SetProviderAccountSecretResolver(checkoutStaticProviderSecretResolver{rail: "nmi", environment: "live", accountID: "mobius-account"})

	client, err := svc.resolveNMIClient(ctx, "nmi")
	require.NoError(t, err)
	require.Equal(t, "merchant-mobius-key", client.SecurityKey)
	require.Contains(t, fakeKV.data, "secret/openrails/merchants/cozy-art/provider_accounts/nmi/live/mobius-account/security_key")
}

func TestCheckoutFallsBackToStaticMobiusClientWhenMerchantSecretMissing(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := &CheckoutService{Config: checkoutRailConfig(true), Rails: checkoutRailSet("static-mobius-key")}

	client, err := svc.resolveNMIClient(ctx, "nmi")
	require.NoError(t, err)
	require.Equal(t, "static-mobius-key", client.SecurityKey)
}

func TestCheckoutProviderAccountResolverMissingMobiusSecretDoesNotUseStaticClient(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := &CheckoutService{Config: checkoutRailConfig(true), Rails: checkoutRailSet("static-mobius-key")}
	svc.SetMerchantSecretStore(merchants.NewMemorySecretStore())
	svc.SetProviderAccountSecretResolver(checkoutMissingProviderSecretResolver{})

	_, err := svc.resolveNMIClient(ctx, "nmi")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing scoped merchant NMI secret")
}

func TestCheckoutFailsClosedWhenMerchantSecretBackendUnavailable(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := &CheckoutService{Config: checkoutRailConfig(true), Rails: checkoutRailSet("static-mobius-key")}
	svc.SetMerchantSecretStore(unavailableSecretStore{})
	svc.SetProviderAccountSecretResolver(checkoutStaticProviderSecretResolver{rail: "nmi", environment: "live", accountID: "mobius-account"})

	_, err := svc.resolveNMIClient(ctx, "nmi")
	require.Error(t, err)
	require.True(t, errors.Is(err, merchants.ErrSecretBackendUnavailable), "err = %v", err)
}

func TestCheckoutResolvesCCBillConfigFromMerchantSecret(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	store := merchants.NewMemorySecretStore()
	secretName, err := merchants.ProviderAccountSecretName("ccbill", "live", "merchant-acc/merchant-sub", "salt")
	require.NoError(t, err)
	_, err = store.Put(ctx, dbtest.TestMerchantID, secretName, "merchant-salt")
	require.NoError(t, err)

	svc := &CheckoutService{Config: checkoutRailConfig(true), Rails: checkoutRailSet("static-mobius-key")}
	svc.SetMerchantSecretStore(store)
	svc.SetProviderAccountSecretResolver(checkoutStaticProviderSecretResolver{
		rail:        "ccbill",
		environment: "live",
		accountID:   "merchant-acc/merchant-sub",
		settings:    map[string]any{"allowed_cidrs": []any{"64.38.240.0/24"}},
	})

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
	require.Contains(t, err.Error(), "missing scoped merchant CCBill provider account")
}

func TestCheckoutCCBillSubscriptionUsesMerchantSecret(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	store := merchants.NewMemorySecretStore()
	secretName, err := merchants.ProviderAccountSecretName("ccbill", "live", "merchant-acc/merchant-sub", "salt")
	require.NoError(t, err)
	_, err = store.Put(ctx, dbtest.TestMerchantID, secretName, "merchant-salt")
	require.NoError(t, err)

	svc := &CheckoutService{Config: checkoutRailConfig(true), Rails: checkoutRailSet("static-mobius-key")}
	svc.SetMerchantSecretStore(store)
	svc.SetProviderAccountSecretResolver(checkoutStaticProviderSecretResolver{rail: "ccbill", environment: "live", accountID: "merchant-acc/merchant-sub"})

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

func checkoutRailSet(mobiusKey string) config.ProviderAccountSet {
	return config.ProviderAccountSet{
		"mobius": {
			Rail: models.RailNMI,
			NMI:  &config.NMIRailConfig{SecurityKey: mobiusKey},
		},
		"ccbill": {
			Rail: models.RailCCBill,
			CCBill: &config.CCBillRailConfig{
				ClientAccNum: "static-acc",
				ClientSubAcc: "static-sub",
				Salt:         "static-salt",
			},
		},
	}
}
