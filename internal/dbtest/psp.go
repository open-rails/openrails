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
	id := TestPSPID(merchantID, rail)
	_, err := qx.Exec(ctx,
		`INSERT INTO openrails.psps (id, merchant_id, rail, environment, account_id, key, archived)
		 VALUES ($1, $2, $3, 'live', $4, $5, false)
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
