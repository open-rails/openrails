//go:build integration

package controlplane_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

// TestMerchantCreationPolicyAndDormancySweep is the or#914 items-3+5 proof,
// end-to-end over the exported host surface against real Postgres + the real
// in-process authkit core:
//
//   - item 3: MerchantCreationAdmission — verified email always; a free
//     allowance of OWNED merchants; beyond it a vaulted payment method (via
//     the host seam / SubjectHasVaultedPaymentMethod over openrails' own
//     vault) unlocks more;
//   - item 5: SweepDormantMerchants — never-used merchants past TTL are
//     warned in openrails.merchant_dormancy_notices, active/young/reserved
//     merchants are untouched, an armed pass past the lead deletes the group
//     WITH the slug released plus a directory soft-delete, and a merchant
//     that regains activity has its notice withdrawn.
func TestMerchantCreationPolicyAndDormancySweep(t *testing.T) {
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
		status, body = postJSON(t, srv.URL+"/email/verify/confirm",
			`{"email":"`+email+`","code":"`+sender.code(email)+`"}`)
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
			`INSERT INTO openrails.customers (merchant_id, issuer, subject) VALUES ($1::uuid, 'test', $2) RETURNING id::text`,
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

	t.Run("item 5: warn, hold, delete-with-release, withdraw", func(t *testing.T) {
		owner, err := core.CreateUser(ctx, "dorm-"+sfx+"@example.test", "dorm"+sfx)
		require.NoError(t, err)
		vaulted[owner.ID] = true
		_, err = pool.Exec(ctx, `UPDATE profiles.users SET email_verified = true WHERE id = $1::uuid`, owner.ID)
		require.NoError(t, err)

		mk := func(slug string) embcp.ProvisionMerchantResult {
			t.Helper()
			res, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{Slug: slug})
			require.NoError(t, err)
			return *res
		}
		backdate := func(id string, d time.Duration) {
			t.Helper()
			_, err := pool.Exec(ctx,
				`UPDATE openrails.merchants SET created_at = now() - make_interval(hours => $2) WHERE id = $1::uuid`,
				id, int(d.Hours()))
			require.NoError(t, err)
		}

		neverUsed := mk("dorm-a-" + sfx)
		young := mk("dorm-b-" + sfx)
		active := mk("dorm-c-" + sfx)
		regains := mk("dorm-d-" + sfx)
		ttl := 30 * 24 * time.Hour
		backdate(neverUsed.MerchantID.String(), ttl+24*time.Hour)
		backdate(active.MerchantID.String(), ttl+24*time.Hour)
		backdate(regains.MerchantID.String(), ttl+24*time.Hour)
		_ = young // stays at now(): under TTL
		// "active" has a customer — ANY setup/usage disqualifies.
		seedScoped(t, active.MerchantID.String(),
			`INSERT INTO openrails.customers (merchant_id, issuer, subject) VALUES ($1::uuid, 'test', $2)`,
			active.MerchantID.String(), uuid.NewString())

		noticeCount := func(merchantID string) int {
			t.Helper()
			tx, err := pool.Begin(ctx)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback(ctx) }()
			_, err = tx.Exec(ctx, `SELECT set_config('app.merchant_id', $1, true)`, merchantID)
			require.NoError(t, err)
			var n int
			require.NoError(t, tx.QueryRow(ctx,
				`SELECT count(*) FROM openrails.merchant_dormancy_notices WHERE merchant_id = $1::uuid`,
				merchantID).Scan(&n))
			return n
		}

		cfgSweep := embcp.DormancySweepConfig{TTL: ttl, WarningLead: 7 * 24 * time.Hour}
		res, err := embcp.SweepDormantMerchants(ctx, e.App(), cfgSweep)
		require.NoError(t, err)
		require.GreaterOrEqual(t, res.Warned, 2, "neverUsed + regains get first warnings")
		require.Equal(t, 0, res.Deleted)
		require.GreaterOrEqual(t, res.SkippedActive, 1, "the merchant with a customer is not dormant")

		require.Equal(t, 1, noticeCount(neverUsed.MerchantID.String()))
		require.Equal(t, 1, noticeCount(regains.MerchantID.String()))
		require.Equal(t, 0, noticeCount(young.MerchantID.String()))
		require.Equal(t, 0, noticeCount(active.MerchantID.String()))

		// Lead not served: even an ARMED pass deletes nothing.
		res, err = embcp.SweepDormantMerchants(ctx, e.App(), embcp.DormancySweepConfig{
			TTL: ttl, WarningLead: cfgSweep.WarningLead, Armed: true,
		})
		require.NoError(t, err)
		require.Equal(t, 0, res.Deleted)

		// Serve the lead; an UNARMED pass still only reports would-delete.
		seedScoped(t, neverUsed.MerchantID.String(),
			`UPDATE openrails.merchant_dormancy_notices SET first_warned_at = now() - interval '8 days'
			  WHERE merchant_id = $1::uuid`, neverUsed.MerchantID.String())
		res, err = embcp.SweepDormantMerchants(ctx, e.App(), cfgSweep)
		require.NoError(t, err)
		require.Equal(t, 0, res.Deleted)
		require.GreaterOrEqual(t, res.WouldDelete, 1)

		// "regains" shows activity -> its notice is withdrawn.
		seedScoped(t, regains.MerchantID.String(),
			`INSERT INTO openrails.customers (merchant_id, issuer, subject) VALUES ($1::uuid, 'test', $2)`,
			regains.MerchantID.String(), uuid.NewString())

		// ARMED: neverUsed is deleted — group gone, slug RELEASED, row
		// soft-deleted, notice cleaned; regains' notice withdrawn.
		res, err = embcp.SweepDormantMerchants(ctx, e.App(), embcp.DormancySweepConfig{
			TTL: ttl, WarningLead: cfgSweep.WarningLead, Armed: true,
		})
		require.NoError(t, err)
		require.Equal(t, 1, res.Deleted)
		require.GreaterOrEqual(t, res.Withdrawn, 1)

		_, err = core.GroupInstanceForSlug(ctx, "merchant", "dorm-a-"+sfx)
		require.ErrorIs(t, err, authkit.ErrGroupNotFound, "released, not tombstoned")
		var status string
		var deleted bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT status, deleted_at IS NOT NULL FROM openrails.merchants WHERE id = $1::uuid`,
			neverUsed.MerchantID.String()).Scan(&status, &deleted))
		require.Equal(t, "deleted", status)
		require.True(t, deleted)
		require.Equal(t, 0, noticeCount(neverUsed.MerchantID.String()))

		// The released name is claimable again, as a NEW merchant.
		re, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{
			Slug: "dorm-a-" + sfx, OwnerUserID: owner.ID,
		})
		require.NoError(t, err)
		require.True(t, re.Created)
		require.NotEqual(t, neverUsed.MerchantID.String(), re.MerchantID.String())

		// Config discipline: a non-positive TTL/lead refuses the pass.
		_, err = embcp.SweepDormantMerchants(ctx, e.App(), embcp.DormancySweepConfig{TTL: 0, WarningLead: time.Hour})
		require.Error(t, err)
	})
}
