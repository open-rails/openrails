//go:build integration

package merchants_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/embedded"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestAliveMerchantKeepsIdentityAfterNameReclaim(t *testing.T) {
	ctx := context.Background()
	_, dsn := dbtest.SharedRLSPostgres(t)
	admin := dbtest.SharedSuperuserPGXPool(t)
	poolConfig, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	poolConfig.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	require.NoError(t, err)
	defer pool.Close()
	database, err := db.NewWithPGXPool(pool, config.DefaultSchema)
	require.NoError(t, err)
	core, err := authcore.New(authcore.Config{Keys: authcore.KeysConfig{VerifyOnly: true}, Token: authcore.TokenConfig{Issuer: "https://names.test", IssuedAudiences: []string{"test"}}, RBAC: []authcore.PersonaDef{{Name: "merchant", Parent: authcore.RootPersona}}}, admin)
	require.NoError(t, err)
	require.NoError(t, core.SeedPermissionGroupContainment(ctx))
	_, err = core.EnsureRootGroup(ctx)
	require.NoError(t, err)
	names, err := authcore.NewGroupDirectory(admin, "")
	require.NoError(t, err)
	authority := controlplane.MerchantNameAuthority(names)
	directory, err := merchants.NewDirectoryService(database.DataPool())
	require.NoError(t, err)
	directory.WithNameAuthority(authority)
	suffix := uuid.NewString()[:8]
	old, newName := "former-"+suffix, "current-"+suffix
	ownerA, err := core.CreateUser(ctx, "owner-a-"+suffix+"@example.test", "owner_a_"+suffix)
	require.NoError(t, err)
	ownerB, err := core.CreateUser(ctx, "owner-b-"+suffix+"@example.test", "owner_b_"+suffix)
	require.NoError(t, err)
	groupA, err := core.CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{Persona: "merchant", InstanceSlug: old, OwnerSubjectID: ownerA.ID})
	require.NoError(t, err)
	first, made, err := directory.Provision(ctx, merchants.ProvisionRequest{Slug: old, PermissionGroupID: groupA})
	require.NoError(t, err)
	require.True(t, made)
	customer := identity.CustomerID(uuid.New())
	ms := money.NewMoneyService(database)
	require.NoError(t, database.RunInMerchantConn(merchant.WithID(ctx, first.ID), func(ctx context.Context) error {
		_, err := ms.Deposit(ctx, money.DepositParams{CustomerID: &customer, Invoker: customer.String(), Currency: "USD", Amount: 100, Source: "name-test"})
		return err
	}))
	_, err = core.UpdateGroupInstanceAs(ctx, ownerA.ID, groupA, authkit.GroupInstanceUpdate{Slug: &newName})
	require.NoError(t, err)
	for _, name := range []string{old, newName} {
		selected, err := directory.GetBySlug(ctx, name)
		require.NoError(t, err)
		require.Equal(t, first.ID, selected.ID)
		require.Equal(t, newName, selected.Slug)
	}
	cfg := &config.Config{DB: &config.DBConfig{Schema: config.DefaultSchema}, MerchantSource: "manifest"}
	catalogFor := func(name, display string) []byte {
		return []byte("version: 1\ncatalogs:\n  - merchant: " + name + "\n    products:\n      - key: owner_product\n        display_name: " + display + "\n")
	}
	require.NoError(t, embedded.PushMerchantCatalog(ctx, embedded.CatalogPushOptions{
		Config: cfg, PGXPool: pool, NameAuthority: authority, Manifest: catalogFor(old, "Original catalog"), Insert: true, Overwrite: true, Prune: true,
	}))
	_, err = core.CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{Persona: "merchant", InstanceSlug: old, OwnerSubjectID: ownerB.ID})
	require.Error(t, err, "retained former name cannot be reclaimed")
	var projection, status string
	var deleted *time.Time
	require.NoError(t, admin.QueryRow(ctx, `SELECT slug,status,deleted_at FROM openrails.merchants WHERE id=$1`, first.ID.UUID()).Scan(&projection, &status, &deleted))
	require.Equal(t, old, projection, "resolving a name must not mutate its non-authoritative projection")
	require.Equal(t, "active", status)
	require.Nil(t, deleted)
	// Move only this isolated fixture's stored deadline past expiry; the claim
	// decision and forwarding reads still run through AuthKit's actual APIs.
	_, err = admin.Exec(ctx, `UPDATE profiles.name_claims SET expires_at=now()-interval '1 second' WHERE owner_id=$1::uuid AND name=$2`, groupA, old)
	require.NoError(t, err)
	_, err = names.GroupInstanceForSlug(ctx, "merchant", old)
	require.ErrorIs(t, err, authkit.ErrGroupNotFound)
	groupB, err := core.CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{Persona: "merchant", InstanceSlug: old, OwnerSubjectID: ownerB.ID})
	require.NoError(t, err)
	require.NotEqual(t, groupA, groupB)
	second, made, err := directory.Provision(ctx, merchants.ProvisionRequest{Slug: old, PermissionGroupID: groupB})
	require.NoError(t, err)
	require.True(t, made)
	require.NotEqual(t, first.ID, second.ID)
	current, err := directory.GetBySlug(ctx, old)
	require.NoError(t, err)
	require.Equal(t, second.ID, current.ID)
	original, err := directory.GetBySlug(ctx, newName)
	require.NoError(t, err)
	require.Equal(t, first.ID, original.ID)
	again, made, err := directory.Provision(ctx, merchants.ProvisionRequest{Slug: newName, PermissionGroupID: groupA})
	require.NoError(t, err)
	require.False(t, made)
	require.Equal(t, first.ID, again.ID)
	for _, tc := range []struct {
		id      merchant.ID
		balance int64
	}{{first.ID, 100}, {second.ID, 0}} {
		require.NoError(t, database.RunInMerchantConn(merchant.WithID(ctx, tc.id), func(ctx context.Context) error {
			balance, err := ms.GetBalanceForCustomer(ctx, customer, "USD")
			if err != nil {
				return err
			}
			require.Equal(t, tc.balance, balance.Balance)
			return nil
		}))
	}
	// The alias initially selected A's catalog; after reclaim, the same external
	// name selects B. UUID-scoped import still writes only to captured A.
	require.NoError(t, embedded.PushMerchantCatalog(ctx, embedded.CatalogPushOptions{
		Config: cfg, PGXPool: pool, NameAuthority: authority, Manifest: catalogFor(old, "New owner catalog"), Insert: true, Overwrite: true, Prune: true,
	}))
	var originalCatalog, reclaimedCatalog bytes.Buffer
	require.NoError(t, embedded.DumpMerchantCatalog(ctx, embedded.CatalogDumpOptions{Config: cfg, PGXPool: pool, NameAuthority: authority, Merchant: newName, Out: &originalCatalog}))
	require.NoError(t, embedded.DumpMerchantCatalog(ctx, embedded.CatalogDumpOptions{Config: cfg, PGXPool: pool, NameAuthority: authority, Merchant: old, Out: &reclaimedCatalog}))
	require.Contains(t, originalCatalog.String(), "Original catalog")
	require.NotContains(t, originalCatalog.String(), "New owner catalog")
	require.Contains(t, originalCatalog.String(), "merchant: "+newName)
	require.Contains(t, reclaimedCatalog.String(), "New owner catalog")
	require.NotContains(t, reclaimedCatalog.String(), "Original catalog")
	importedCustomer := uuid.New()
	_, err = embedded.ImportBilling(ctx, embedded.BillingImportOptions{PGXPool: pool, MerchantID: first.ID,
		Book: embedded.DeclaredBilling{AsOf: time.Now().UTC(), Customers: []embedded.DeclaredCustomer{{Customer: importedCustomer}}},
	})
	require.NoError(t, err)
	var importedOwner uuid.UUID
	require.NoError(t, admin.QueryRow(ctx, `SELECT merchant_id FROM openrails.customers WHERE id=$1`, importedCustomer).Scan(&importedOwner))
	require.Equal(t, first.ID.UUID(), importedOwner)
	resolved, canonical, err := embedded.ResolveMerchantName(ctx, embedded.MerchantNameOptions{PGXPool: pool, Name: old, NameAuthority: authority})
	require.NoError(t, err)
	require.Equal(t, second.ID, resolved)
	require.Equal(t, old, canonical)
	_, _, err = embedded.ResolveMerchantName(ctx, embedded.MerchantNameOptions{PGXPool: pool, Name: old})
	require.Error(t, err, "a local-only directory cannot choose either bound projection")
	host, err := db.RegisterUnboundMerchant(ctx, database.Qx(ctx), db.RegisterUnboundMerchantOptions{Slug: old})
	require.NoError(t, err)
	require.NotEqual(t, first.ID, host)
	require.NotEqual(t, second.ID, host)
	hostAgain, err := db.RegisterUnboundMerchant(ctx, database.Qx(ctx), db.RegisterUnboundMerchantOptions{Slug: old})
	require.NoError(t, err)
	require.Equal(t, host, hostAgain)
	resolved, _, err = embedded.ResolveMerchantName(ctx, embedded.MerchantNameOptions{PGXPool: pool, Name: old})
	require.NoError(t, err)
	require.Equal(t, host, resolved)
	found, err := directory.SearchMerchants(ctx, old, 50)
	require.NoError(t, err)
	require.Len(t, found, 2)
	for _, row := range found {
		require.NotEqual(t, first.ID, row.ID)
	}
	// A complete canonical authority page without billing bindings cannot hide
	// the next page's provisioned merchant.
	prefix := "paged-" + suffix
	for i := 0; i < 200; i++ {
		_, err := core.CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{Persona: "merchant", InstanceSlug: fmt.Sprintf("%s-%03d", prefix, i)})
		require.NoError(t, err)
	}
	eligibleName := prefix + "-200"
	eligibleGroup, err := core.CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{Persona: "merchant", InstanceSlug: eligibleName})
	require.NoError(t, err)
	eligible, _, err := directory.Provision(ctx, merchants.ProvisionRequest{Slug: eligibleName, PermissionGroupID: eligibleGroup})
	require.NoError(t, err)
	found, err = directory.SearchMerchants(ctx, prefix, 1)
	require.NoError(t, err)
	require.Len(t, found, 1)
	require.Equal(t, eligible.ID, found[0].ID)
}
