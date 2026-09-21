//go:build integration

package railresolve_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/stretchr/testify/require"
)

func TestExactProviderReadsPreserveCredentialFloorAndArchivedBinding(t *testing.T) {
	ctx := dbtest.WithTestMerchant(t.Context())
	owner := dbtest.TestMerchantID
	pool := dbtest.SharedMerchantPool(t, owner.UUID())
	database, err := db.NewWithPGXPool(pool, "billing")
	require.NoError(t, err)
	account, accountKey := "scope-"+uuid.NewString(), "declared-key-"+uuid.NewString()
	psp, custodian := uuid.New(), uuid.New()
	_, err = database.Qx(ctx).Exec(ctx, `INSERT INTO billing.custodians(id,merchant_id,key,kind,environment,account_id,settings) VALUES($1,$2,$3,'basis_theory','live',$3,'{"public_api_key":"synthetic-public"}')`, custodian, owner.UUID(), account)
	require.NoError(t, err)
	_, err = database.Qx(ctx).Exec(ctx, `INSERT INTO billing.psps(id,merchant_id,rail,environment,account_id,key,custodian_id,evidence,archived) VALUES($1,$2,'nmi','live',$3,$4,$5,'{"credential_versions":{"security_key":2}}',true)`, psp, owner.UUID(), account, accountKey, custodian)
	require.NoError(t, err)
	backend := merchants.NewMemorySecretStore()
	secretName, err := merchants.PSPSecretName("nmi", "live", account, "security_key")
	require.NoError(t, err)
	_, err = backend.Put(ctx, owner, secretName, "synthetic-first")
	require.NoError(t, err)
	cache := merchants.NewCachedSecretStore(backend, time.Hour)
	_, err = cache.Get(ctx, owner, secretName)
	require.NoError(t, err)
	svc, err := merchants.NewService(db.WrapPool(pool, "billing"), cache, "live")
	require.NoError(t, err)
	armer := &railresolve.NMIArmer{DB: database, MerchantsFn: func() *merchants.Service { return svc }}

	t.Run("stamped operation preserves full scope", func(t *testing.T) {
		scope, found, err := armer.ResolveScope(ctx, owner, "nmi", &psp)
		require.NoError(t, err)
		require.True(t, found)
		ref, err := scope.SecretRef("security_key")
		require.NoError(t, err)
		require.Equal(t, 2, ref.MinVersion)
		require.Equal(t, accountKey, scope.Key)
		require.Equal(t, &custodian, scope.CustodianID)
		_, err = armer.RequireSecret(ctx, owner, scope, "security_key")
		require.Error(t, err)
	})
	t.Run("archived drain preserves full scope", func(t *testing.T) {
		scope, found, err := svc.PullPSPScope(ctx, owner, "nmi", "live")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, psp, scope.ID)
		require.Equal(t, accountKey, scope.Key)
		require.Equal(t, &custodian, scope.CustodianID)
	})
	t.Run("saved method refuses stale credential", func(t *testing.T) {
		service := paymentmethods.NewRailPaymentMethodService(nil, nil, database, &config.Config{})
		service.SetMerchantSecretStore(cache)
		service.SetPSPSecretResolver(svc)
		method := &models.PaymentMethod{ID: uuid.New(), PspID: psp, Rail: models.RailNMI, Custodian: models.CustodianPSP}
		client, err := service.ResolveClientForPaymentMethod(ctx, method)
		require.Error(t, err)
		require.Nil(t, client)
	})

	_, err = backend.Put(ctx, owner, secretName, "synthetic-second")
	require.NoError(t, err)
	scope, found, err := armer.ResolveScope(ctx, owner, "nmi", &psp)
	require.NoError(t, err)
	require.True(t, found)
	secret, err := armer.RequireSecret(ctx, owner, scope, "security_key")
	require.NoError(t, err)
	require.Equal(t, "synthetic-second", secret)
}
