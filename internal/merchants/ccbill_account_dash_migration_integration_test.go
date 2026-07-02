//go:build integration

package merchants

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	postgresmigrations "github.com/open-rails/openrails/migrations/postgres"
)

// #697: migration 068 hard-cuts the CCBill composite account identity from
// clientAccnum/clientSubacc (slash) to clientAccnum-clientSubacc (dash) —
// rewriting declared rail_merchant_accounts.account_id values plus the
// account-id segment of stored secret names (both the canonical %2F-escaped
// form and the defensive raw-slash six-segment form), and leaving every
// non-ccbill row/name untouched.
func TestMigration068RewritesCCBillSlashAccountIDs(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	mid := uuid.NewString()

	seedAccount := func(rail, accountID string) {
		t.Helper()
		_, err := pool.Exec(ctx, `
			INSERT INTO openrails.rail_merchant_accounts (merchant_id, rail, environment, account_id)
			VALUES ($1::uuid, $2, 'live', $3)
		`, mid, rail, accountID)
		require.NoError(t, err)
	}
	seedAccount("ccbill", "945280/0000") // slash form: rewritten
	seedAccount("ccbill", "945281-0000") // already canonical: untouched
	seedAccount("nmi", "579145")         // other rail: untouched
	seedAccount("nmi", "odd/slash")      // other rail with a slash: untouched

	seededNames := []string{
		// Canonical writer URL-escaped the embedded slash (5 segments, %2F).
		"rail_merchant_accounts/ccbill/live/945280%2F0000/salt",
		// Defensive: raw slash inside the id = one extra segment (6 total).
		"rail_merchant_accounts/ccbill/live/945280/0000/datalink_username",
		// Already dash-form: untouched.
		"rail_merchant_accounts/ccbill/live/945281-0000/salt",
		// Other rail: untouched.
		"rail_merchant_accounts/nmi/live/579145/security_key",
		// Legacy broad merchant secret name: untouched.
		"stripe/secret_key",
	}
	for _, name := range seededNames {
		_, err := pool.Exec(ctx, `
			INSERT INTO openrails.merchant_secrets (merchant_id, name, value)
			VALUES ($1::uuid, $2, 'v')
		`, mid, name)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `
			INSERT INTO openrails.merchant_credential_audit (merchant_id, name, action)
			VALUES ($1::uuid, $2, 'put')
		`, mid, name)
		require.NoError(t, err)
	}

	migrationSQL, err := postgresmigrations.FS.ReadFile("068_ccbill_account_id_dash.up.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(migrationSQL))
	require.NoError(t, err)

	accountIDs := func(rail string) []string {
		t.Helper()
		rows, err := pool.Query(ctx, `
			SELECT account_id FROM openrails.rail_merchant_accounts
			WHERE merchant_id = $1::uuid AND rail = $2
		`, mid, rail)
		require.NoError(t, err)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id string
			require.NoError(t, rows.Scan(&id))
			out = append(out, id)
		}
		require.NoError(t, rows.Err())
		return out
	}
	require.ElementsMatch(t, []string{"945280-0000", "945281-0000"}, accountIDs("ccbill"))
	require.ElementsMatch(t, []string{"579145", "odd/slash"}, accountIDs("nmi"))

	wantNames := []string{
		"rail_merchant_accounts/ccbill/live/945280-0000/salt",
		"rail_merchant_accounts/ccbill/live/945280-0000/datalink_username",
		"rail_merchant_accounts/ccbill/live/945281-0000/salt",
		"rail_merchant_accounts/nmi/live/579145/security_key",
		"stripe/secret_key",
	}
	for _, table := range []string{"merchant_secrets", "merchant_credential_audit"} {
		rows, err := pool.Query(ctx, `
			SELECT name FROM openrails.`+table+`
			WHERE merchant_id = $1::uuid
		`, mid)
		require.NoError(t, err)
		var got []string
		for rows.Next() {
			var name string
			require.NoError(t, rows.Scan(&name))
			got = append(got, name)
		}
		require.NoError(t, rows.Err())
		rows.Close()
		require.ElementsMatch(t, wantNames, got, "table %s", table)
	}

	// The rewritten canonical name round-trips through the parser to the dash id.
	rail, environment, accountID, key, ok, err := ParseRailMerchantAccountSecretName("rail_merchant_accounts/ccbill/live/945280-0000/salt")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "ccbill", rail)
	require.Equal(t, "live", environment)
	require.Equal(t, "945280-0000", accountID)
	require.Equal(t, "salt", key)
}
