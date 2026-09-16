//go:build integration

package controlplane_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

// TestMerchantCreationPolicy is the or#914 item-3 proof, end-to-end over the
// exported host surface against real Postgres + the real in-process authkit
// core: MerchantCreationAdmission requires a verified email always, allows a
// free allowance of OWNED merchants, and beyond it requires a vaulted payment
// method (via the host seam / SubjectHasVaultedPaymentMethod over openrails'
// own vault).
func TestMerchantCreationPolicy(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	cfg := hostedTestConfig(t, dsn, "https://or914b.openrails.test")
	e := newHostApp(t, cfg)

	vaulted := map[string]bool{} // subject -> has card (the host seam, stubbed)
	policy := embcp.MerchantCreationPolicy{
		FreeAllowance: 1,
		HasVaultedPaymentMethod: func(_ context.Context, subject string) (bool, error) {
			return vaulted[subject], nil
		},
	}
	admission, err := embcp.MerchantCreationAdmission(e.App(), policy)
	require.NoError(t, err)
	_, err = embcp.MerchantCreationAdmission(e.App(), embcp.MerchantCreationPolicy{FreeAllowance: 0})
	require.Error(t, err, "a zero allowance is a refused construction, not a silent default")

	sender := &captureEmailSender{}
	require.NoError(t, embcp.AttachWithOptions(ctx, e.App(), cfg, nil, embcp.AttachOptions{
		HostedPosture:    true,
		EmailSender:      sender,
		MerchantCreation: &embcp.MerchantCreationConfig{Admission: admission},
	}))
	cp := embcp.Get(e.App())
	core := cp.Core()

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// seedScoped runs one statement under the merchant's RLS GUC (the test DSN
	// role answers to FORCE RLS like everything else).
	seedScoped := func(t *testing.T, merchantID, sql string, args ...any) string {
		t.Helper()
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		_, err = tx.Exec(ctx, `SELECT set_config('app.merchant_id', $1, true)`, merchantID)
		require.NoError(t, err)
		var id string
		if strings.Contains(strings.ToUpper(sql), "RETURNING") {
			require.NoError(t, tx.QueryRow(ctx, sql, args...).Scan(&id))
		} else {
			_, err = tx.Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		require.NoError(t, tx.Commit(ctx))
		return id
	}

	sfx := strings.ToLower(uuid.NewString()[:8])

	t.Run("item 3: verified email, allowance, then card-on-file", func(t *testing.T) {
		// An UNVERIFIED user never claims a merchant name.
		unverified, err := core.CreateUser(ctx, "unv-"+sfx+"@example.test", "unv"+sfx)
		require.NoError(t, err)
		_, err = embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{
			Slug: "unv-shop-" + sfx, OwnerUserID: unverified.ID,
		})
		require.ErrorIs(t, err, embcp.ErrCreationRefused)
		require.ErrorIs(t, err, embcp.ErrEmailUnverified)

		// A verified user (real register + verify flow).
		srv := mountAuthRoutes(t, e)
		email := "pol-" + sfx + "@example.test"
		status, body := postJSON(t, srv.URL+"/register",
			`{"identifier":"`+email+`","username":"pol`+sfx+`","password":"str0ng-horse-battery!"}`)
		require.Equal(t, 202, status, "register: %v", body)
		status, body = postJSON(t, srv.URL+"/verify/confirm",
			`{"identifier":"`+email+`","code":"`+sender.code(email)+`"}`)
		require.Equal(t, 200, status, "verify: %v", body)
		user, err := core.GetUserByEmail(ctx, email)
		require.NoError(t, err)

		// Within the allowance: first merchant is free.
		first, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{
			Slug: "pol-one-" + sfx, OwnerUserID: user.ID,
		})
		require.NoError(t, err)
		require.True(t, first.Created)

		// Re-posting a merchant this user already owns repairs provisioning; it
		// is not another allowance-consuming claim and never consults the vault.
		again, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{
			Slug: "pol-one-" + sfx, OwnerUserID: user.ID,
		})
		require.NoError(t, err)
		require.False(t, again.Created)

		// AuthKit forwards renamed-away slugs to the same internal group. The
		// admission predicate must compare that stable identity, not only the
		// group's current display slug.
		renamedSlug := "pol-renamed-" + sfx
		_, err = core.UpdateGroupInstanceAs(ctx, user.ID, first.GroupID, authkit.GroupInstanceUpdate{Slug: &renamedSlug})
		require.NoError(t, err)
		require.NoError(t, admission(ctx, "pol-one-"+sfx, user.ID), "an owned tombstone is still an idempotent repair")

		// Beyond it: refused until a payment method is on file.
		_, err = embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{
			Slug: "pol-two-" + sfx, OwnerUserID: user.ID,
		})
		require.ErrorIs(t, err, embcp.ErrCreationRefused)
		require.ErrorIs(t, err, embcp.ErrVaultedPaymentMethodRequired)

		vaulted[user.ID] = true
		second, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{
			Slug: "pol-two-" + sfx, OwnerUserID: user.ID,
		})
		require.NoError(t, err, "a vaulted payment method unlocks creation beyond the allowance")
		require.True(t, second.Created)

		// SubjectHasVaultedPaymentMethod reads openrails' OWN vault: false on
		// an empty book; true once an un-parked method exists for the subject
		// under the vault merchant; false again when it is parked.
		vaultMerchant := first.MerchantID
		subject := user.ID
		has, err := embcp.SubjectHasVaultedPaymentMethod(ctx, e.App(), vaultMerchant, subject)
		require.NoError(t, err)
		require.False(t, has)

		customerID := seedScoped(t, vaultMerchant.String(),
			`INSERT INTO openrails.customers (merchant_id, issuer, id) VALUES ($1::uuid, 'test', $2) RETURNING id::text`,
			vaultMerchant.String(), subject)
		pspID := seedScoped(t, vaultMerchant.String(),
			`INSERT INTO openrails.psps (merchant_id, rail, environment, account_id, key) VALUES ($1::uuid, 'nmi', 'test', 'acct-`+sfx+`', 'vaultpsp') RETURNING id::text`,
			vaultMerchant.String())
		pmID := seedScoped(t, vaultMerchant.String(), `
			INSERT INTO openrails.payment_methods (rail, initial_transaction_id, merchant_id, customer_id, psp_id)
			VALUES ('nmi', 'txn-`+sfx+`', $1::uuid, $2::uuid, $3::uuid) RETURNING id::text`,
			vaultMerchant.String(), customerID, pspID)
		has, err = embcp.SubjectHasVaultedPaymentMethod(ctx, e.App(), vaultMerchant, subject)
		require.NoError(t, err)
		require.True(t, has)
		seedScoped(t, vaultMerchant.String(),
			`UPDATE openrails.payment_methods SET parked_at = now() WHERE id = $1::uuid`, pmID)
		has, err = embcp.SubjectHasVaultedPaymentMethod(ctx, e.App(), vaultMerchant, subject)
		require.NoError(t, err)
		require.False(t, has, "a parked method is not a usable card on file")
	})
}
