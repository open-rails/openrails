//go:build integration

package embed_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	embedoperator "github.com/open-rails/openrails/embed/operator"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestOperatorDeclaresImportAttributionAfterMerchantProvision(t *testing.T) {
	ctx := t.Context()
	owner, pool, dsn := scopeWithoutRLSDatabase(t)
	opts := func() embed.Options {
		return embed.Options{Config: &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly, Encryption: &config.EncryptionConfig{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="},
			TestMode:           config.CredentialPostureSandbox,
			MerchantConfigHTTP: true, SecretBackend: config.SecretBackendDB,
			DB: &config.DBConfig{URL: dsn},
		}, PGXPool: pool, River: embed.RiverFromHost()}
	}
	// The hosting runtime already exists when its control plane obtains the ID.
	runtime, err := embed.New(ctx, opts())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	slug := "operator-attribution-" + uuid.NewString()
	provisioned, mid, err := newDeclaredMerchant(ctx, opts(), slug, embed.MerchantConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, provisioned.Close(context.Background())) })
	local := embedoperator.New(runtime)
	declaration := embed.PSPDeclaration{Key: "platform", Rail: "platform", AccountID: mid.String()}
	id, err := local.DeclarePSP(ctx, mid, declaration)
	require.NoError(t, err)
	again, err := local.DeclarePSP(ctx, mid, declaration)
	require.NoError(t, err)
	require.Equal(t, id, again)
	// A declaration is attribution, not provider reconfiguration or reactivation.
	_, err = owner.Exec(ctx, `UPDATE billing.psps SET archived=true,replaced_at=now(),evidence='{"sentinel":"preserved"}'::jsonb WHERE id=$1`, id)
	require.NoError(t, err)
	var before time.Time
	require.NoError(t, owner.QueryRow(ctx, `SELECT updated_at FROM billing.psps WHERE id=$1`, id).Scan(&before))
	again, err = local.DeclarePSP(ctx, mid, declaration)
	require.NoError(t, err)
	require.Equal(t, id, again)
	var archived bool
	var evidence, key string
	var replaced *time.Time
	var updated time.Time
	require.NoError(t, owner.QueryRow(ctx, `SELECT archived,evidence->>'sentinel',key,replaced_at,updated_at FROM billing.psps WHERE id=$1`, id).Scan(&archived, &evidence, &key, &replaced, &updated))
	require.True(t, archived)
	require.Equal(t, "preserved", evidence)
	require.Equal(t, "platform", key)
	require.NotNil(t, replaced)
	require.True(t, before.Equal(updated))
	renamed := declaration
	renamed.Key = "another-alias"
	_, err = local.DeclarePSP(ctx, mid, renamed)
	require.ErrorIs(t, err, openrails.ErrConflict)
	other, otherID, err := newDeclaredMerchant(ctx, opts(), slug+"-other", embed.MerchantConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close(context.Background())) })
	_, err = local.DeclarePSP(ctx, otherID, declaration)
	require.ErrorContains(t, err, "owned by another merchant")
	absent := embed.PSPDeclaration{Key: "absent", Rail: "platform", AccountID: uuid.NewString()}
	_, err = local.DeclarePSP(ctx, merchant.ID(uuid.New()), absent)
	require.Error(t, err)
	var count int
	require.NoError(t, owner.QueryRow(ctx, `SELECT count(*) FROM billing.psps WHERE account_id=$1`, absent.AccountID).Scan(&count))
	require.Zero(t, count)
}
