package payment

import (
	"context"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchants"
)

// ccbillScope is a one-account PSP catalog for credential resolution.
type ccbillScope struct{ scope merchants.PSPScope }

func (c ccbillScope) ActivePSPSecretName(_ context.Context, _ billing.MerchantID, rail, env, key string) (string, bool, error) {
	name, err := merchants.PSPSecretName(rail, env, c.scope.AccountID, key)
	return name, err == nil, err
}
func (c ccbillScope) ActivePSPScope(_ context.Context, _ billing.MerchantID, rail, _ string) (merchants.PSPScope, bool, error) {
	return c.scope, rail == c.scope.Rail, nil
}
func (c ccbillScope) PSPScopeByID(_ context.Context, _ billing.MerchantID, id uuid.UUID) (merchants.PSPScope, bool, error) {
	return c.scope, id == c.scope.ID, nil
}

func ccbillService(t *testing.T, accountID string) *PurchaseService {
	store := merchants.SecretMap{}
	if name, err := merchants.PSPSecretName("ccbill", "live", accountID, "salt"); err == nil {
		store[name] = "merchant-salt"
	}
	svc := &PurchaseService{Config: &config.Config{ProviderWriteMode: config.ProviderWriteModeFull}}
	svc.SetMerchantSecretStore(store)
	svc.SetPSPSecretResolver(ccbillScope{merchants.PSPScope{ID: uuid.New(), Rail: "ccbill", Environment: "live", AccountID: accountID, Key: "ccbill"}})
	return svc
}

// #819/#697: a CCBill upgrade form carries the target price's own currency
// and the dash-split account identity, signed with the merchant's secret.
func TestCCBillUpgradeWire(t *testing.T) {
	email := "alice@example.com"
	user := &UserIdentity{ID: "user-1", Username: "alice", Email: &email}
	sub := &models.Subscription{ID: uuid.New(), Rail: models.RailCCBill, RailSubscriptionID: "ccbill-sub-1"}
	price := func(currency string) *models.Price {
		return &models.Price{ID: uuid.New(), Currency: currency, PSPLinks: map[string]map[string]string{
			"ccbill": {models.RailKeyRail: "ccbill", models.RailKeyCCBillFormName: "premium", models.RailKeyCCBillFlexID: "flex-123"}}}
	}

	resp, err := ccbillService(t, "999999-0000").processCCBillUpgrade(merchantCtx(), user, price("eur"), sub)
	require.NoError(t, err)
	parsed, err := url.Parse(resp.RedirectURL)
	require.NoError(t, err)
	q := parsed.Query()
	require.Equal(t, "978", q.Get("currencyCode"))
	require.Equal(t, "999999", q.Get("clientAccnum"))
	require.Equal(t, "0000", q.Get("clientSubacc"))
	require.Equal(t, "ccbill-sub-1", q.Get("originalSubscriptionId"))
	require.NotEmpty(t, q.Get("signature"))

	for _, currency := range []string{"", "sek"} {
		_, err := ccbillService(t, "999999-0000").processCCBillUpgrade(merchantCtx(), user, price(currency), sub)
		require.Error(t, err, "a currency CCBill cannot bill is never defaulted: %q", currency)
	}
	_, err = ccbillService(t, "999999/0000").processCCBillUpgrade(merchantCtx(), user, price("usd"), sub)
	require.ErrorContains(t, err, "CCBill account_id uses a dash")

	missing := &PurchaseService{}
	missing.SetMerchantSecretStore(merchants.SecretMap{})
	missing.SetPSPSecretResolver(pspCatalog{})
	_, err = missing.resolveCCBillClient(merchantCtx())
	require.ErrorContains(t, err, "missing scoped merchant CCBill PSP")
	_, err = (&PurchaseService{}).resolveCCBillClient(merchantCtx())
	require.ErrorContains(t, err, "not configured", "no boot-config fallback (#788)")
}

// NMI credentials resolve only through the scoped merchant plane (#788) and
// only for the account the operation captured.
func TestNMIClientResolutionFailsClosed(t *testing.T) {
	_, err := (&PurchaseService{}).resolveNMIClient(merchantCtx(), "nmi")
	require.ErrorContains(t, err, "not configured")

	unarmed := &PurchaseService{}
	unarmed.SetMerchantSecretStore(merchants.SecretMap{})
	unarmed.SetPSPSecretResolver(pspCatalog{})
	_, err = unarmed.resolveNMIClient(merchantCtx(), "nmi")
	require.ErrorContains(t, err, "has no armed PSP")

	captured := ccbillService(t, "999999-0000")
	pspID := captured.ProviderSecrets.(ccbillScope).scope.ID
	_, err = captured.resolveNMIClient(db.WithPSPID(merchantCtx(), pspID), "nmi")
	require.ErrorContains(t, err, "captured PSP rail mismatch")
}
