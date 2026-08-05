package dbtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
)

// EnsureTestPSP materializes one openrails.psps row for (merchant, rail) and
// returns its id. or#893 made psp_id required on every provider-bound table, so
// a fixture that inserts a subscription / payment / payment method / checkout
// session / intent / mutation log must name a real PSP. This is the canonical
// one: deterministic per (merchant, rail), idempotent, and distinct per merchant
// so uq_psps_identity (rail, environment, account_id) never collides across
// fixtures sharing a database.
//
// Call it with a privileged/RLS-bypassing handle, or under the merchant's own
// connection — psps is merchant-scoped and RLS-protected either way.
func EnsureTestPSP(ctx context.Context, t testing.TB, qx gen.DBTX, merchantID uuid.UUID, rail string) uuid.UUID {
	t.Helper()
	// Reuse an account the merchant already has on this rail. Adding a second
	// one is not a no-op: it makes the merchant multi-account, and the
	// new-work selector then picks the NEWEST — which would be this
	// credential-less fixture row instead of the armed account another test in
	// the same database seeded with real secrets.
	var existing uuid.UUID
	if err := qx.QueryRow(ctx,
		`SELECT id FROM openrails.psps
		  WHERE merchant_id = $1 AND rail = lower($2) AND archived = false
		  ORDER BY created_at DESC, id DESC LIMIT 1`,
		merchantID, rail).Scan(&existing); err == nil && existing != uuid.Nil {
		return existing
	}
	id := TestPSPID(merchantID, rail)
	_, err := qx.Exec(ctx,
		// environment='test': integration runs boot in test mode, so 'test' is the
		// environment every resolver expects — and a fixture PSP claiming to be a
		// LIVE account also trips real gates that ask "is a live account armed?"
		// (the CCBill source-IP allowlist is one).
		// created_at is back-dated deliberately. The new-work selector prefers the
		// NEWEST non-archived account, and this generic row carries no
		// credentials — so an account a test seeded on purpose (with real
		// secrets) must win that selection whatever order the tests ran in. The
		// row stays non-archived because fixtures also need it to be ROUTABLE.
		`INSERT INTO openrails.psps (id, merchant_id, rail, environment, account_id, key, archived, created_at, first_seen_at)
		 VALUES ($1, $2, $3, 'test', $4, $5, false, 'epoch'::timestamptz, 'epoch'::timestamptz)
		 ON CONFLICT (id) DO NOTHING`,
		id, merchantID, rail, testPSPAccountID(merchantID, rail), rail)
	require.NoError(t, err, "ensure test psp %s/%s", merchantID, rail)
	return id
}

// TestPSPID is the deterministic id EnsureTestPSP writes. Exposed so a fixture
// can stamp rows in raw SQL without threading the return value around.
func TestPSPID(merchantID uuid.UUID, rail string) uuid.UUID {
	return uuid.NewSHA1(pspNamespace, []byte(merchantID.String()+"|"+rail))
}

var pspNamespace = uuid.MustParse("9c7f0b6e-0000-4000-8000-000000000893")

func testPSPAccountID(merchantID uuid.UUID, rail string) string {
	sum := sha256.Sum256([]byte(merchantID.String() + "|" + rail))
	return "test-" + rail + "-" + hex.EncodeToString(sum[:6])
}
