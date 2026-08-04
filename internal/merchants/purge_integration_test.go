//go:build integration

package merchants

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/pkg/merchant"
)

// or#858 hazard 2. `Export` wrote a manifest of row COUNTS and secret NAMES —
// not one byte of data — and `Delete` was gated on it plus a bare
// `Confirm: true`. An operator who read the name, saw a completed export row and
// typed the boolean destroyed 13 tables of customer-bearing state with no
// restore point of any kind, having never been shown how many rows that was.
//
// The pin is the OBSERVABLE OUTCOME, not the flags: after every refusal below,
// the rows are still there. Reverting any one guard in delete.go makes exactly
// one sub-test fail on a row count, which is what a guard is for.
func TestMerchantPurgeRefusesUntilTheBlastRadiusIsSeenAndTyped(t *testing.T) {
	ctx := context.Background()
	superDSN, appDSN := dbtest.SharedRLSPostgres(t)

	superRaw, err := pgxpool.New(ctx, superDSN)
	require.NoError(t, err)
	defer superRaw.Close()
	super := db.WrapPool(superRaw, config.DefaultSchema)

	appRaw, err := pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	defer appRaw.Close()
	appPool := db.WrapPool(appRaw, config.DefaultSchema)

	suffix := uuid.NewString()[:8]
	merchantID := uuid.New()
	slug := "or858-purge-" + suffix
	mid := merchant.ID(merchantID)

	_, err = super.Exec(ctx,
		`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1::uuid, $2, 'active')`, merchantID, slug)
	require.NoError(t, err)

	// A small but real book: a product, its price, a customer and the local
	// mirror of that customer's stored card.
	productID, priceID, customerID := uuid.New(), uuid.New(), uuid.New()
	_, err = super.Exec(ctx,
		`INSERT INTO openrails.products (id, key, display_name, tier_group, entitlements_spec, merchant_id)
		 VALUES ($1,$2,$2,$3,'{"pro": null}'::jsonb,$4)`,
		productID, "or858-"+suffix, "or858-tier-"+suffix, merchantID)
	require.NoError(t, err)
	_, err = super.Exec(ctx,
		`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
		 VALUES ($1,$2,999,'USD',720,true,$3)`, priceID, productID, merchantID)
	require.NoError(t, err)
	_, err = super.Exec(ctx,
		`INSERT INTO openrails.customers (id, merchant_id, subject) VALUES ($1,$2,$3)`,
		customerID, merchantID, "or858-subject-"+suffix)
	require.NoError(t, err)
	_, err = super.Exec(ctx,
		`INSERT INTO openrails.payment_methods (merchant_id, customer_id, rail, initial_transaction_id, custodian)
		 VALUES ($1,$2,'nmi',$3,'psp')`, merchantID, customerID, "txn-"+suffix)
	require.NoError(t, err)

	const seededRows = 3 // products + prices + payment_methods

	rowsLeft := func(t *testing.T) int {
		t.Helper()
		var n int
		require.NoError(t, super.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM openrails.products WHERE merchant_id = $1)
			     + (SELECT count(*) FROM openrails.prices WHERE merchant_id = $1)
			     + (SELECT count(*) FROM openrails.payment_methods WHERE merchant_id = $1)
		`, merchantID).Scan(&n))
		return n
	}
	require.Equal(t, seededRows, rowsLeft(t))

	svc, err := NewService(appPool, nil, "test")
	require.NoError(t, err)

	// --- 1. no confirmation: refused, and the refusal STATES the blast radius.
	t.Run("unconfirmed purge destroys nothing and names the row count", func(t *testing.T) {
		err := svc.Delete(ctx, mid, DeleteOptions{})
		require.Error(t, err)
		var notConfirmed *ErrPurgeNotConfirmed
		require.ErrorAs(t, err, &notConfirmed)
		require.Equal(t, seededRows, notConfirmed.TotalRows, "the refusal must carry the TRUE row count")
		require.Contains(t, err.Error(), PurgeConfirmPhrase(slug))
		require.Contains(t, err.Error(), "not a backup")
		require.Equal(t, seededRows, rowsLeft(t), "a refused purge must leave every row in place")
	})

	// --- 2. the inventory says, in its own recorded manifest, that it is not one.
	var inv PurgeInventory
	t.Run("the purge inventory declares what it does not capture", func(t *testing.T) {
		var err error
		inv, err = svc.TakePurgeInventory(ctx, mid)
		require.NoError(t, err)
		require.False(t, inv.IsBackup())
		require.Equal(t, seededRows, inv.TotalRows)
		require.Equal(t, 1, inv.RowCounts["payment_methods"])
		require.NotEmpty(t, inv.NotCaptured)

		joined := strings.ToLower(strings.Join(inv.NotCaptured, "\n"))
		for _, must := range []string{
			"row data",     // counts, not rows
			"secret value", // names only
			"stored payment methods",
			"point-in-time recovery",
		} {
			require.Contains(t, joined, must, "the inventory must spell out this omission")
		}
		require.Contains(t, joined, "not revoked",
			"an operator must be told the instruments survive at the PSP and are not revoked by a purge")

		// The recorded row carries the same statement, for whoever reads the
		// table rather than the Go type.
		var raw []byte
		require.NoError(t, super.QueryRow(ctx,
			`SELECT manifest FROM openrails.merchant_purge_inventories WHERE id = $1::uuid`, inv.ID).Scan(&raw))
		var manifest map[string]any
		require.NoError(t, json.Unmarshal(raw, &manifest))
		require.Equal(t, false, manifest["is_backup"])
		require.Equal(t, "nothing", manifest["restores"])
		require.Contains(t, manifest["restore_path"], "point-in-time recovery")
		require.Equal(t, seededRows, rowsLeft(t), "taking an inventory must not touch a row")
	})

	// --- 3. fully confirmed, but the destructive gate is not wired: fail closed.
	expect := seededRows
	confirmed := DeleteOptions{ConfirmPhrase: PurgeConfirmPhrase(slug), ExpectRows: &expect, Actor: "or858-test"}
	t.Run("an unwired destructive gate denies", func(t *testing.T) {
		err := svc.Delete(ctx, mid, confirmed)
		require.ErrorContains(t, err, "destructive gate not wired")
		require.Equal(t, seededRows, rowsLeft(t))
	})

	// --- 4. gate wired but the instance kill switch is OFF (the shipped default).
	appDB := dbtest.OpenAppDB(t, appDSN)
	svc.WithDestructivePolicy(destructive.New(appDB))
	t.Run("the kill switch holds a purge exactly as it holds a mass cancellation", func(t *testing.T) {
		err := svc.Delete(ctx, mid, confirmed)
		require.ErrorContains(t, err, "kill switch is OFF")
		require.Equal(t, seededRows, rowsLeft(t))
	})

	dbtest.ArmDestructiveActions(ctx, t, merchantID)

	// --- 5. armed, but the typed count is wrong.
	t.Run("a wrong typed row count destroys nothing", func(t *testing.T) {
		wrong := seededRows + 1
		err := svc.Delete(ctx, mid, DeleteOptions{
			ConfirmPhrase: PurgeConfirmPhrase(slug), ExpectRows: &wrong, Actor: "or858-test"})
		var mismatch *ErrPurgeRowCountMismatch
		require.ErrorAs(t, err, &mismatch)
		require.Equal(t, seededRows, mismatch.Found)
		require.Equal(t, seededRows, rowsLeft(t))
	})

	// --- 6. the book changed since the inventory: the operator looked at a state
	//        that no longer exists, so the inventory no longer authorises anything.
	t.Run("a stale inventory does not authorise a purge", func(t *testing.T) {
		_, err := super.Exec(ctx,
			`INSERT INTO openrails.prices (product_id, key, amount, currency, access_duration_hours, auto_renew, merchant_id)
			 VALUES ($1,$2,1999,'USD',720,true,$3)`, productID, "or858-second-"+suffix, merchantID)
		require.NoError(t, err)

		grown := seededRows + 1
		err = svc.Delete(ctx, mid, DeleteOptions{
			ConfirmPhrase: PurgeConfirmPhrase(slug), ExpectRows: &grown, Actor: "or858-test"})
		var stale *ErrPurgeInventoryStale
		require.ErrorAs(t, err, &stale)
		require.Equal(t, grown, stale.TotalRows)
		require.Equal(t, grown, rowsLeft(t), "a refused purge must leave every row in place")
	})

	// --- 7. fresh inventory + phrase + matching count: it goes through, and it is
	//        recorded in the same destructive-run ledger --prune writes.
	t.Run("a seen, typed, armed purge proceeds and is attributable", func(t *testing.T) {
		fresh, err := svc.TakePurgeInventory(ctx, mid)
		require.NoError(t, err)
		require.Equal(t, seededRows+1, fresh.TotalRows)

		total := fresh.TotalRows
		require.NoError(t, svc.Delete(ctx, mid, DeleteOptions{
			ConfirmPhrase: PurgeConfirmPhrase(slug), ExpectRows: &total, Actor: "or858-operator"}))
		require.Equal(t, 0, rowsLeft(t))

		var status string
		require.NoError(t, super.QueryRow(ctx,
			`SELECT status FROM openrails.merchants WHERE id = $1::uuid`, merchantID).Scan(&status))
		require.Equal(t, "deleted", status)

		var kind, actor string
		var expectedRows int64
		var affected []byte
		require.NoError(t, super.QueryRow(ctx, `
			SELECT kind, actor, expected_rows, affected FROM openrails.destructive_runs
			 WHERE merchant_id = $1::uuid AND kind = $2`,
			merchantID, DestructiveRunKindMerchantPurge).Scan(&kind, &actor, &expectedRows, &affected))
		require.Equal(t, DestructiveRunKindMerchantPurge, kind)
		require.Equal(t, "or858-operator", actor)
		require.Equal(t, int64(total), expectedRows)

		var counts map[string]int
		require.NoError(t, json.Unmarshal(affected, &counts))
		require.Equal(t, 1, counts["payment_methods"])
	})
}

// The grant log FK-pins the products and payments it justifies, and a purge may
// not delete it (the app role holds no DELETE on grants; entitlements are
// RECOMPUTED from it). So a merchant with real billing history cannot be purged
// at all — and the honest outcome is one refusal that destroys nothing, not a
// half-purged merchant and a raw SQLSTATE.
//
// Before or#858 the purge order also put products BEFORE prices, so this aborted
// on prices_product_id_fkey for every merchant that owned a priced product. The
// only test that existed seeded a single entitlement row and never saw it.
func TestMerchantPurgeRefusesWhenRetainedHistoryPinsRows(t *testing.T) {
	ctx := context.Background()
	superDSN, appDSN := dbtest.SharedRLSPostgres(t)

	superRaw, err := pgxpool.New(ctx, superDSN)
	require.NoError(t, err)
	defer superRaw.Close()
	super := db.WrapPool(superRaw, config.DefaultSchema)

	appRaw, err := pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	defer appRaw.Close()

	suffix := uuid.NewString()[:8]
	merchantID := uuid.New()
	slug := "or858-pinned-" + suffix
	mid := merchant.ID(merchantID)

	_, err = super.Exec(ctx,
		`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1::uuid, $2, 'active')`, merchantID, slug)
	require.NoError(t, err)

	productID, customerID := uuid.New(), uuid.New()
	_, err = super.Exec(ctx,
		`INSERT INTO openrails.products (id, key, display_name, tier_group, entitlements_spec, merchant_id)
		 VALUES ($1,$2,$2,$3,'{"pro": null}'::jsonb,$4)`,
		productID, "or858p-"+suffix, "or858p-tier-"+suffix, merchantID)
	require.NoError(t, err)
	_, err = super.Exec(ctx,
		`INSERT INTO openrails.customers (id, merchant_id, subject) VALUES ($1,$2,$3)`,
		customerID, merchantID, "or858p-subject-"+suffix)
	require.NoError(t, err)
	// The append-only grant that justifies the product's ownership.
	_, err = super.Exec(ctx, `
		INSERT INTO openrails.grants (merchant_id, customer_id, product_id, kind, source_type, event)
		VALUES ($1,$2,$3,'ownership','purchase','grant')`, merchantID, customerID, productID)
	require.NoError(t, err)

	svc, err := NewService(db.WrapPool(appRaw, config.DefaultSchema), nil, "test")
	require.NoError(t, err)
	svc.WithDestructivePolicy(allowAllDestructive{})

	inv, err := svc.TakePurgeInventory(ctx, mid)
	require.NoError(t, err)
	require.Equal(t, 1, inv.TotalRows)

	err = svc.Delete(ctx, mid, DeleteOptions{
		ConfirmPhrase: PurgeConfirmPhrase(slug), ExpectRows: &inv.TotalRows, Actor: "or858-test"})
	var blocked *ErrPurgeBlockedByRetainedHistory
	require.ErrorAs(t, err, &blocked, "a pinned purge must refuse with an explanation, got %v", err)
	require.Contains(t, err.Error(), "NOTHING was deleted")

	// The refusal is total: product, grant and the merchant itself survive.
	var products, grants int
	var status string
	require.NoError(t, super.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM openrails.products WHERE merchant_id=$1),
		        (SELECT count(*) FROM openrails.grants WHERE merchant_id=$1),
		        (SELECT status FROM openrails.merchants WHERE id=$1)`,
		merchantID).Scan(&products, &grants, &status))
	require.Equal(t, 1, products)
	require.Equal(t, 1, grants)
	require.Equal(t, "active", status, "a blocked purge must not tombstone the merchant")
}
