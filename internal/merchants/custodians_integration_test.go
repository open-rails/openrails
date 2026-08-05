//go:build integration

package merchants

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// or#880 phase 3, STORE PLANE: the mode-2 write path for custodians. It runs
// under the enforcing openrails_app role, because the two things worth proving
// are both RLS-shaped:
//
//   - a merchant sees and resolves only its OWN custodians, and
//   - an inbound custodian webhook still routes across merchants, through the
//     SECURITY DEFINER directory function and nothing else.
//
// Both ingestion planes validate through config.ValidateCustodianEntry, so a
// declaration the manifest accepts is never one an API write silently drops.
func TestCustodianStorePlaneUnderEnforcingRLS(t *testing.T) {
	ctx := context.Background()
	superDSN, appDSN := dbtest.SharedRLSPostgres(t)

	suffix := uuid.NewString()[:8]
	ownerID, otherID := uuid.New(), uuid.New()

	superRaw, err := pgxpool.New(ctx, superDSN)
	require.NoError(t, err)
	defer superRaw.Close()
	super := db.WrapPool(superRaw, config.DefaultSchema)
	for id, slug := range map[uuid.UUID]string{ownerID: "or880-owner-" + suffix, otherID: "or880-other-" + suffix} {
		_, err = super.Exec(ctx,
			`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1::uuid, $2, 'active')`, id, slug)
		require.NoError(t, err)
	}

	appRaw, err := pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	defer appRaw.Close()
	appPool := db.WrapPool(appRaw, config.DefaultSchema)
	svc, err := NewService(appPool, nil, "live")
	require.NoError(t, err)

	owner := merchant.ID(ownerID)
	other := merchant.ID(otherID)
	tenantID := "tnt_or880_" + suffix

	entry := config.CustodianEntry{
		Key:       "bt",
		Kind:      models.CustodianBasisTheory,
		AccountID: tenantID,
		Settings: map[string]any{
			custodians.SettingPublicAPIKey: "key_pub_" + suffix,
		},
	}
	scope, err := svc.UpsertCustodian(ctx, owner, entry, "live")
	require.NoError(t, err)
	require.Equal(t, "bt", scope.Key)
	require.Equal(t, models.CustodianBasisTheory, scope.Kind)
	require.Equal(t, tenantID, scope.AccountID)
	require.Equal(t, "key_pub_"+suffix, scope.Settings[custodians.SettingPublicAPIKey])

	// Idempotent re-apply: the same declaration converges the same row.
	again, err := svc.UpsertCustodian(ctx, owner, entry, "live")
	require.NoError(t, err)
	require.Equal(t, scope.ID, again.ID)

	// The SAME validator the manifest plane runs — a half-declared custodian
	// never reaches a row.
	bad := entry
	bad.Settings = map[string]any{"invented_next_year": "x"}
	_, err = svc.UpsertCustodian(ctx, owner, bad, "live")
	require.Error(t, err)

	// Resolution by key and by vendor identity, inside the owner's scope.
	byKey, ok, err := svc.CustodianScopeByKey(ctx, owner, "BT")
	require.NoError(t, err)
	require.True(t, ok, "the reference key is case-insensitive, like every other PSP key")
	require.Equal(t, scope.ID, byKey.ID)

	byIdentity, ok, err := svc.CustodianScopeByIdentity(ctx, owner, models.CustodianBasisTheory, "live", tenantID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, scope.ID, byIdentity.ID)

	// RLS: the other merchant sees nothing, by key or by identity.
	_, ok, err = svc.CustodianScopeByKey(ctx, other, "bt")
	require.NoError(t, err)
	require.False(t, ok, "a custodian is merchant-owned state")
	_, ok, err = svc.CustodianScopeByIdentity(ctx, other, models.CustodianBasisTheory, "live", tenantID)
	require.NoError(t, err)
	require.False(t, ok)

	// The global identity belongs to exactly one merchant: a second merchant
	// claiming the same tenant is refused, not silently co-owned.
	_, err = svc.UpsertCustodian(ctx, other, entry, "live")
	require.Error(t, err)

	// Cross-merchant routing: an inbound Basis Theory event carries only the
	// tenant id and no merchant context. It must still find its owner — and
	// through the SECURITY DEFINER directory function, on this very
	// RLS-enforcing connection.
	identity, ok, err := svc.ResolveCustodianByIdentity(ctx, models.CustodianBasisTheory, "live", tenantID)
	require.NoError(t, err)
	require.True(t, ok, "custodian webhook routing must not depend on a merchant already being pinned")
	require.Equal(t, owner, identity.MerchantID)
	require.Equal(t, scope.ID, identity.ID)
	require.Equal(t, "bt", identity.Key)

	// An unknown tenant is a clean miss, never a wrong merchant.
	_, ok, err = svc.ResolveCustodianByIdentity(ctx, models.CustodianBasisTheory, "live", "tnt_nobody_"+suffix)
	require.NoError(t, err)
	require.False(t, ok)

	// or#812: the custodial credential carries a rotation version floor read
	// off the SAME row every resolution already re-reads, so a key rotated on
	// one node cannot keep being presented from another node's cache.
	ref, err := byKey.SecretRef(custodians.SecretAPIKey)
	require.NoError(t, err)
	require.Equal(t, "custodians/basis_theory/live/"+tenantID+"/api_key", ref.Name)
	require.Zero(t, ref.MinVersion, "a seeded credential carries no floor until something rotates it")

	rotated := entry
	rotated.CredentialVersions = map[string]int{custodians.SecretAPIKey: 7}
	afterRotation, err := svc.UpsertCustodian(ctx, owner, rotated, "live")
	require.NoError(t, err)
	ref, err = afterRotation.SecretRef(custodians.SecretAPIKey)
	require.NoError(t, err)
	require.Equal(t, 7, ref.MinVersion, "the recorded floor must reach every reader through the row")

	// A floor NEVER goes backwards, and a plane that only SEEDS (the manifest)
	// must not clear one a rotation recorded.
	seedOnly := entry
	seedOnly.CredentialVersions = nil
	afterSeed, err := svc.UpsertCustodian(ctx, owner, seedOnly, "live")
	require.NoError(t, err)
	ref, err = afterSeed.SecretRef(custodians.SecretAPIKey)
	require.NoError(t, err)
	require.Equal(t, 7, ref.MinVersion, "a seeding write must not erase a rotation floor")
}
