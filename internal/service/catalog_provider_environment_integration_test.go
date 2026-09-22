//go:build integration

package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

type recordingCatalogAdapter struct{ targets []string }

func (a *recordingCatalogAdapter) Name() string { return "stripe" }
func (a *recordingCatalogAdapter) AutoCreate(_ context.Context, in autoCreateContext) (map[string]string, error) {
	a.targets = append(a.targets, in.TargetAccountID)
	return map[string]string{"account_id": in.TargetAccountID, "price_id": "fake-price"}, nil
}
func (a *recordingCatalogAdapter) Attach(ctx context.Context, _ map[string]string, in autoCreateContext) (map[string]string, error) {
	return a.AutoCreate(ctx, in)
}
func (a *recordingCatalogAdapter) Verify(context.Context, map[string]string, *priceVerifyContext) ([]DriftField, bool, error) {
	return nil, false, nil
}
func (a *recordingCatalogAdapter) Update(context.Context, map[string]string, mutableUpdate) error {
	return nil
}
func (a *recordingCatalogAdapter) PendingActionTemplate(uuid.UUID) PendingAction {
	return PendingAction{Provider: "stripe", Hint: "fake adapter"}
}

func TestCreatorProviderEnvironmentSurvivesDispatchAndSecondarySync(t *testing.T) {
	svc, admin := creatorCatalogService(t)
	owner, catalogID := creatorContext(t, svc, admin, "environment-test")
	mid, err := merchant.Require(admin)
	require.NoError(t, err)
	suffix := "-" + uuid.NewString()
	for _, environment := range []string{"live", "test"} {
		for _, key := range []string{"named", "stripe", "retired"} {
			_, err := svc.rt.DB.Qx(admin).Exec(admin, `INSERT INTO billing.psps(id,merchant_id,rail,environment,account_id,key,archived)
				VALUES($1,$2,'stripe',$3,$4,$5,$6)`, uuid.New(), mid.UUID(), environment, environment+"-"+key+suffix, key, key == "retired")
			require.NoError(t, err)
		}
	}
	product := &models.Product{ID: uuid.New(), MerchantID: mid.UUID(), CatalogID: catalogID.UUID(), Key: "environment-product"}
	for _, tc := range []struct {
		name, environment string
		posture           config.CredentialPosture
	}{
		{name: "live", environment: "live", posture: config.CredentialPostureLive},
		{name: "sandbox", environment: "test", posture: config.CredentialPostureSandbox},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc.rt.Config.TestMode = tc.posture
			svc.rt.Config.ProviderWriteMode = config.ProviderWriteModeFull
			keys, err := svc.creatorProviderKeys(owner)
			require.NoError(t, err)
			require.Equal(t, []string{"named", "stripe"}, keys)
			adapter := &recordingCatalogAdapter{}
			request := CreatePriceRequest{ProductID: openrails.ProductID(product.ID), PSPs: keys, UnitAmount: 1_000_000, Currency: "USD"}
			rails, _, pending, err := svc.resolveProvidersWithAdapters(owner, product, request, uuid.New(), map[string]providerAdapter{"stripe": adapter})
			require.NoError(t, err)
			require.Empty(t, pending)
			require.Equal(t, tc.environment+"-named"+suffix, rails["named"]["account_id"], "duplicate account keys must resolve in the selected credential environment")
			// The canonical rail target uses its armed default, then synchronizes
			// the active accounts on that rail. No real adapter/network is invoked.
			require.ElementsMatch(t, []string{tc.environment + "-named" + suffix, "", tc.environment + "-named" + suffix, tc.environment + "-stripe" + suffix}, adapter.targets)

			adapter.targets = nil
			svc.rt.Config.ProviderWriteMode = config.ProviderWriteModeReadOnly
			_, states, pending, err := svc.resolveProvidersWithAdapters(owner, product, request, uuid.New(), map[string]providerAdapter{"stripe": adapter})
			require.NoError(t, err)
			require.Empty(t, adapter.targets, "read-only posture must suppress primary and secondary provider writes")
			require.Len(t, pending, 2)
			require.Equal(t, ProviderStatusPendingManualLink, states["named"].Status)

			svc.rt.Config.ProviderWriteMode = config.ProviderWriteModeFull
			svc.rt.Config.NewSubscriptionCollectionPolicy = "engine"
			for _, recurring := range []bool{false, true} {
				request.AutoRenew = recurring
				if recurring {
					hours := 720
					request.AccessDurationHours = &hours
				}
				links, states, pending, err := svc.resolveProvidersWithAdapters(owner, product, request, uuid.New(), map[string]providerAdapter{"stripe": adapter})
				require.NoError(t, err)
				require.Empty(t, adapter.targets, "engine prices must skip every primary and secondary Stripe account")
				require.Empty(t, links)
				require.Empty(t, states)
				require.Empty(t, pending)
			}
			request.PSPLinks = map[string]map[string]string{"named": {"price_id": "price_legacy"}}
			links, _, _, err := svc.resolveProvidersWithAdapters(owner, product, request, uuid.New(), map[string]providerAdapter{"stripe": adapter})
			require.NoError(t, err)
			require.Equal(t, tc.environment+"-named"+suffix, links["named"]["account_id"])
			require.Equal(t, []string{tc.environment + "-named" + suffix}, adapter.targets, "explicit legacy attachment must not fan out into new provider objects")
			svc.rt.Config.NewSubscriptionCollectionPolicy = ""
		})
	}
}
