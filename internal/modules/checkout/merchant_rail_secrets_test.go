package checkout

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/merchant"
)

type checkoutFakeVaultKV struct {
	data map[string]map[string]string
}

func newCheckoutFakeVaultKV() *checkoutFakeVaultKV {
	return &checkoutFakeVaultKV{data: map[string]map[string]string{}}
}

func (f *checkoutFakeVaultKV) ReadSecret(_ context.Context, path string) (map[string]string, int, error) {
	if value, ok := f.data[path]; ok {
		return value, 1, nil
	}
	return nil, 0, nil
}

func (f *checkoutFakeVaultKV) WriteSecret(_ context.Context, path string, data map[string]string) (int, error) {
	f.data[path] = data
	return 1, nil
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

func (r checkoutStaticProviderSecretResolver) scope() merchants.PSPScope {
	return merchants.PSPScope{
		ID:          merchants.PspID(r.rail, r.environment, r.accountID),
		Rail:        r.rail,
		Environment: r.environment,
		AccountID:   r.accountID,
		Key:         r.rail,
		Settings:    r.settings,
	}
}

func (r checkoutStaticProviderSecretResolver) ActivePSPSecretName(_ context.Context, _ merchant.ID, rail, environment, key string) (string, bool, error) {
	name, err := merchants.PSPSecretName(r.rail, r.environment, r.accountID, key)
	return name, err == nil, err
}

func (r checkoutStaticProviderSecretResolver) ActivePSPScope(context.Context, merchant.ID, string, string) (merchants.PSPScope, bool, error) {
	return r.scope(), true, nil
}

func (r checkoutStaticProviderSecretResolver) PSPScopeByKey(_ context.Context, _ merchant.ID, key, _ string) (merchants.PSPScope, bool, error) {
	if !strings.EqualFold(strings.TrimSpace(key), strings.TrimSpace(r.rail)) {
		return merchants.PSPScope{}, false, nil
	}
	return r.scope(), true, nil
}

func (r checkoutStaticProviderSecretResolver) ActivePSPScopesForRail(_ context.Context, _ merchant.ID, rail, _ string) ([]merchants.PSPScope, error) {
	if !strings.EqualFold(strings.TrimSpace(rail), strings.TrimSpace(r.rail)) {
		return nil, nil
	}
	return []merchants.PSPScope{r.scope()}, nil
}

type checkoutMissingProviderSecretResolver struct{}

func (checkoutMissingProviderSecretResolver) ActivePSPSecretName(context.Context, merchant.ID, string, string, string) (string, bool, error) {
	return "", false, nil
}

func (checkoutMissingProviderSecretResolver) ActivePSPScope(context.Context, merchant.ID, string, string) (merchants.PSPScope, bool, error) {
	return merchants.PSPScope{}, false, nil
}

func (checkoutMissingProviderSecretResolver) PSPScopeByKey(context.Context, merchant.ID, string, string) (merchants.PSPScope, bool, error) {
	return merchants.PSPScope{}, false, nil
}

func (checkoutMissingProviderSecretResolver) ActivePSPScopesForRail(context.Context, merchant.ID, string, string) ([]merchants.PSPScope, error) {
	return nil, nil
}

func TestCheckoutResolvesMobiusClientFromVaultMerchantSecret(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	fakeKV := newCheckoutFakeVaultKV()
	store := merchants.NewVaultSecretStore("secret", fakeKV)
	secretName, err := merchants.PSPSecretName("nmi", "live", "mobius-account", "security_key")
	require.NoError(t, err)
	_, err = store.Put(ctx, dbtest.TestMerchantID, secretName, "merchant-mobius-key")
	require.NoError(t, err)

	svc := &CheckoutService{Config: checkoutRailConfig(true), Rails: checkoutRailSet("static-mobius-key")}
	svc.SetMerchantSecretStore(store)
	svc.SetPSPSecretResolver(checkoutStaticProviderSecretResolver{rail: "nmi", environment: "live", accountID: "mobius-account"})

	client, err := svc.resolveNMIClient(ctx, "nmi")
	require.NoError(t, err)
	require.Equal(t, "merchant-mobius-key", client.SecurityKey)
	require.Contains(t, fakeKV.data, "secret/openrails/merchants/"+dbtest.TestMerchantID.String()+"/psps/nmi/live/mobius-account/security_key")
}

// #788: the boot-config plane is gone — with no scoped merchant secret store
// wired, NMI client resolution fails closed instead of falling back to a
// static client.
func TestCheckoutWithoutScopedResolutionFailsClosed(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := &CheckoutService{Config: checkoutRailConfig(true), Rails: checkoutRailSet("static-mobius-key")}

	_, err := svc.resolveNMIClient(ctx, "nmi")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not configured")
}

func TestCheckoutMissingMobiusPSPFailsClosed(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := &CheckoutService{Config: checkoutRailConfig(true), Rails: checkoutRailSet("static-mobius-key")}
	svc.SetMerchantSecretStore(merchants.NewMemorySecretStore())
	svc.SetPSPSecretResolver(checkoutMissingProviderSecretResolver{})

	_, err := svc.resolveNMIClient(ctx, "nmi")
	require.Error(t, err)
	require.Contains(t, err.Error(), "has no armed PSP")
}

func TestCheckoutFailsClosedWhenMerchantSecretBackendUnavailable(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := &CheckoutService{Config: checkoutRailConfig(true), Rails: checkoutRailSet("static-mobius-key")}
	svc.SetMerchantSecretStore(unavailableSecretStore{})
	svc.SetPSPSecretResolver(checkoutStaticProviderSecretResolver{rail: "nmi", environment: "live", accountID: "mobius-account"})

	_, err := svc.resolveNMIClient(ctx, "nmi")
	require.Error(t, err)
	require.True(t, errors.Is(err, merchants.ErrSecretBackendUnavailable), "err = %v", err)
}

func TestCheckoutResolvesCCBillConfigFromMerchantSecret(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	store := merchants.NewMemorySecretStore()
	secretName, err := merchants.PSPSecretName("ccbill", "live", "945280-0000", "salt")
	require.NoError(t, err)
	_, err = store.Put(ctx, dbtest.TestMerchantID, secretName, "merchant-salt")
	require.NoError(t, err)

	svc := &CheckoutService{Config: checkoutRailConfig(true), Rails: checkoutRailSet("static-mobius-key")}
	svc.SetMerchantSecretStore(store)
	svc.SetPSPSecretResolver(checkoutStaticProviderSecretResolver{
		rail:        "ccbill",
		environment: "live",
		accountID:   "945280-0000",
	})

	client, err := svc.resolveCCBillClient(ctx)
	require.NoError(t, err)
	resp, err := client.GenerateFlexFormURL(&ccbill.GenerateFlexFormURLParams{
		Username: "alice",
		Email:    "alice@example.com",
		FormName: "premium",
		FlexID:   "flex-123",
		Currency: "USD",
	})
	require.NoError(t, err)
	parsed, err := url.Parse(resp.RedirectURL)
	require.NoError(t, err)
	require.Equal(t, "945280", parsed.Query().Get("clientAccnum"))
	require.Equal(t, "0000", parsed.Query().Get("clientSubacc"))
	require.NotEmpty(t, parsed.Query().Get("signature"))
}

func TestCheckoutPSPResolverMissingCCBillSecretDoesNotUseStaticConfig(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := &CheckoutService{Config: checkoutRailConfig(true), Rails: checkoutRailSet("static-mobius-key")}
	svc.SetMerchantSecretStore(merchants.NewMemorySecretStore())
	svc.SetPSPSecretResolver(checkoutMissingProviderSecretResolver{})

	_, err := svc.resolveCCBillClient(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing scoped merchant CCBill PSP")
}

// #697: the composite CCBill identity is dash-joined; a legacy slash-form
// account_id must fail loudly, never split on '/'.
func TestCheckoutCCBillSlashAccountIDRejected(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := &CheckoutService{Config: checkoutRailConfig(true), Rails: checkoutRailSet("static-mobius-key")}
	svc.SetMerchantSecretStore(merchants.NewMemorySecretStore())
	svc.SetPSPSecretResolver(checkoutStaticProviderSecretResolver{rail: "ccbill", environment: "live", accountID: "945280/0000"})

	_, err := svc.resolveCCBillClient(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "CCBill account_id uses a dash: clientAccnum-clientSubacc, e.g. 945280-0000")
}

func TestCheckoutCCBillSubscriptionUsesMerchantSecret(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	store := merchants.NewMemorySecretStore()
	secretName, err := merchants.PSPSecretName("ccbill", "live", "945280-0000", "salt")
	require.NoError(t, err)
	_, err = store.Put(ctx, dbtest.TestMerchantID, secretName, "merchant-salt")
	require.NoError(t, err)

	svc := &CheckoutService{Config: checkoutRailConfig(true), Rails: checkoutRailSet("static-mobius-key")}
	svc.SetMerchantSecretStore(store)
	svc.SetPSPSecretResolver(checkoutStaticProviderSecretResolver{rail: "ccbill", environment: "live", accountID: "945280-0000"})

	email := "alice@example.com"
	resp, err := svc.processCCBillSubscription(ctx, &CheckoutRequest{
		NameOnCard: "Alice Example",
		Zip:        "10001",
		Country:    "US",
	}, &UserIdentity{
		ID:       "user-1",
		Email:    &email,
		Username: "alice",
	}, &models.Price{
		ID:       uuid.New(),
		Currency: "USD",
		PSPLinks: map[string]map[string]string{
			"ccbill": {
				models.RailKeyRail:           "ccbill",
				models.RailKeyCCBillFormName: "premium",
				models.RailKeyCCBillFlexID:   "flex-123",
			},
		},
	})
	require.NoError(t, err)
	parsed, err := url.Parse(resp.RedirectURL)
	require.NoError(t, err)
	require.Equal(t, "945280", parsed.Query().Get("clientAccnum"))
	require.Equal(t, "0000", parsed.Query().Get("clientSubacc"))
}

func checkoutRailConfig(testMode bool) *config.Config {
	posture := config.CredentialPostureLive
	if testMode {
		posture = config.CredentialPostureSandbox
	}
	return &config.Config{
		ProviderWriteMode: config.ProviderWriteModeFull,
		TestMode:          posture,
	}
}

func checkoutRailSet(mobiusKey string) railresolve.FixedSet {
	return railresolve.FixedSet{
		"mobius": {
			Rail: models.RailNMI,
			NMI:  &config.NMIRailConfig{SecurityKey: mobiusKey},
		},
		"ccbill": {
			Rail: models.RailCCBill,
			// #711: the clientAccnum/clientSubacc pair derives from the account_id.
			AccountID: "945280-0000",
			CCBill: &config.CCBillRailConfig{
				Salt: "static-salt",
			},
		},
	}
}
