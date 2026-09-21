//go:build integration

package checkout

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/stretchr/testify/require"
)

func TestCapturedPSPCredentialsSurviveRenameAndArchive(t *testing.T) {
	database := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	ctx := dbtest.WithTestMerchant(context.Background())
	store := merchants.NewMemorySecretStore()
	registry, err := merchants.NewService(db.WrapPool(database.Pool(), ""), store, "test")
	require.NoError(t, err)
	cfg := &config.Config{TestMode: config.CredentialPostureSandbox}
	service := &CheckoutService{Config: cfg, Rails: railresolve.NewMerchantsSource(cfg, func() *merchants.Service { return registry })}
	service.SetMerchantSecretStore(store)
	service.SetPSPSecretResolver(registry)
	pspA, pspB := uuid.New(), uuid.New()
	keyA, keyB := "capture-a-"+uuid.NewString(), "capture-b-"+uuid.NewString()
	for id, key := range map[uuid.UUID]string{pspA: keyA, pspB: keyB} {
		_, err = database.Qx(ctx).Exec(ctx, `INSERT INTO billing.psps(id,merchant_id,rail,environment,account_id,key) VALUES($1,$2,'nmi','test',$3,$3)`, id, dbtest.TestMerchantID.UUID(), key)
		require.NoError(t, err)
		secretName, err := merchants.PSPSecretName("nmi", "test", key, "security_key")
		require.NoError(t, err)
		_, err = store.Put(ctx, dbtest.TestMerchantID, secretName, "secret-for-"+key)
		require.NoError(t, err)
	}
	captured := db.WithPSPID(ctx, pspA)
	_, err = database.Qx(ctx).Exec(ctx, `UPDATE billing.psps SET key=$2,archived=true WHERE id=$1`, pspA, keyA+"-renamed")
	require.NoError(t, err)
	client, err := service.resolveNMIClient(captured, keyA)
	require.NoError(t, err)
	require.Equal(t, "secret-for-"+keyA, client.SecurityKey)
	sibling, err := service.resolveNMIClient(db.WithPSPID(ctx, pspB), keyB)
	require.NoError(t, err)
	require.Equal(t, "secret-for-"+keyB, sibling.SecurityKey)
	_, err = service.resolveRailTarget(ctx, keyA+"-renamed")
	require.Error(t, err, "archive disallows a fresh purchase even while replay can read its own credentials")
	resolved, err := service.Rails.RailConfig(captured, "nmi", "")
	require.NoError(t, err)
	require.Equal(t, keyA, resolved.AccountID)
	require.Equal(t, "secret-for-"+keyA, resolved.NMI.SecurityKey)
}
