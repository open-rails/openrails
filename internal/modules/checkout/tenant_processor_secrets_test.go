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
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/tenancy"
	"github.com/open-rails/openrails/pkg/tenant"
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

func (r checkoutSlugResolver) TenantSlug(_ context.Context, id tenant.ID) (string, error) {
	return r[id.String()], nil
}

type unavailableSecretStore struct{}

func (unavailableSecretStore) Get(context.Context, tenant.ID, string) (tenancy.Secret, error) {
	return tenancy.Secret{}, tenancy.ErrSecretBackendUnavailable
}

func TestCheckoutResolvesMobiusClientFromVaultTenantSecret(t *testing.T) {
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	fakeKV := newCheckoutFakeVaultKV()
	store := tenancy.NewVaultSecretStore("secret", fakeKV, checkoutSlugResolver{tenant.DefaultID.String(): "cozy-art"})
	_, err := store.Put(ctx, tenant.DefaultID, tenancy.SecretNMIMobiusProductionKey, "tenant-mobius-key")
	require.NoError(t, err)

	svc := &CheckoutService{Config: checkoutProcessorConfig(true, "static-mobius-key")}
	svc.SetTenantSecretStore(store)

	client, err := svc.resolveNMIClient(ctx, "mobius")
	require.NoError(t, err)
	require.Equal(t, "tenant-mobius-key", client.SecurityKey)
	require.Contains(t, fakeKV.data, "secret/openrails/tenants/cozy-art/nmi/mobius/production_key")
}

func TestCheckoutFallsBackToStaticMobiusClientWhenTenantSecretMissing(t *testing.T) {
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	svc := &CheckoutService{Config: checkoutProcessorConfig(true, "static-mobius-key")}

	client, err := svc.resolveNMIClient(ctx, "mobius")
	require.NoError(t, err)
	require.Equal(t, "static-mobius-key", client.SecurityKey)
}

func TestCheckoutFailsClosedWhenTenantSecretBackendUnavailable(t *testing.T) {
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	svc := &CheckoutService{Config: checkoutProcessorConfig(true, "static-mobius-key")}
	svc.SetTenantSecretStore(unavailableSecretStore{})

	_, err := svc.resolveNMIClient(ctx, "mobius")
	require.Error(t, err)
	require.True(t, errors.Is(err, tenancy.ErrSecretBackendUnavailable), "err = %v", err)
}

func TestCheckoutResolvesCCBillConfigFromTenantSecret(t *testing.T) {
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	store := tenancy.NewMemorySecretStore()
	_, err := store.Put(ctx, tenant.DefaultID, tenancy.SecretCCBillAccountConfig, `{
		"client_acc_num": "tenant-acc",
		"client_sub_acc": "tenant-sub",
		"salt": "tenant-salt"
	}`)
	require.NoError(t, err)

	svc := &CheckoutService{Config: checkoutProcessorConfig(true, "static-mobius-key")}
	svc.SetTenantSecretStore(store)

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
	require.Equal(t, "tenant-acc", parsed.Query().Get("clientAccnum"))
	require.Equal(t, "tenant-sub", parsed.Query().Get("clientSubacc"))
	require.NotEmpty(t, parsed.Query().Get("signature"))
}

func TestCheckoutCCBillSubscriptionUsesTenantSecret(t *testing.T) {
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	store := tenancy.NewMemorySecretStore()
	_, err := store.Put(ctx, tenant.DefaultID, tenancy.SecretCCBillAccountConfig, `{
		"client_acc_num": "tenant-acc",
		"client_sub_acc": "tenant-sub"
	}`)
	require.NoError(t, err)

	svc := &CheckoutService{Config: checkoutProcessorConfig(true, "static-mobius-key")}
	svc.SetTenantSecretStore(store)

	email := "alice@example.com"
	resp, err := svc.processCCBillSubscription(ctx, &CheckoutRequest{}, &UserIdentity{
		ID:       "user-1",
		Email:    &email,
		Username: "alice",
	}, &models.Price{
		ID: uuid.New(),
		Processors: map[string]map[string]string{
			"ccbill": {
				models.ProcessorKeyCCBillFormName: "premium",
				models.ProcessorKeyCCBillFlexID:   "flex-123",
			},
		},
	})
	require.NoError(t, err)
	parsed, err := url.Parse(resp.RedirectURL)
	require.NoError(t, err)
	require.Equal(t, "tenant-acc", parsed.Query().Get("clientAccnum"))
	require.Equal(t, "tenant-sub", parsed.Query().Get("clientSubacc"))
}

func checkoutProcessorConfig(testMode bool, mobiusKey string) *config.Config {
	mode := config.ModeProduction
	if testMode {
		mode = config.ModeTest
	}
	return &config.Config{
		Mode: mode,
		Processors: map[string]*config.ProcessorConfig{
			"mobius": {
				Type:        config.ProcessorTypeNMI,
				SecurityKey: mobiusKey,
			},
			"ccbill": {
				Type:         config.ProcessorTypeCCBill,
				ClientAccNum: "static-acc",
				ClientSubAcc: "static-sub",
				Salt:         "static-salt",
			},
		},
	}
}
