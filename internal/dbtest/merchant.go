package dbtest

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// There is no "default merchant" (#336). Integration fixtures that insert
// merchant-owned rows must run under an explicit merchant; tests use this canonical
// test merchant. EnsureTestMerchant materializes the openrails.merchants row, and
// MerchantID()/WithTestMerchant pin it on the context so merchant.Require resolves and
// WithMerchantConn sets the app.merchant_id GUC.
var (
	// TestMerchantID is the canonical merchant id for integration fixtures.
	TestMerchantID = merchant.ID(uuid.MustParse("a5a5a5a5-0000-4000-8000-000000000001"))
	// TestMerchantSlug is its stable slug.
	TestMerchantSlug = "test"
)

// EnsureTestMerchant inserts the canonical test merchant (idempotent). qx should be
// a privileged/RLS-bypassing handle (the merchants table is control-plane).
func EnsureTestMerchant(ctx context.Context, t testing.TB, qx gen.DBTX) {
	t.Helper()
	_, err := qx.Exec(ctx,
		`INSERT INTO openrails.merchants (id, slug, status)
		 VALUES ($1, $2, 'active')
		 ON CONFLICT (slug) DO NOTHING`,
		TestMerchantID.UUID(), TestMerchantSlug)
	require.NoError(t, err, "ensure test merchant")
}

// WithTestMerchant returns a context pinned to the canonical test merchant, so
// merchant.Require resolves and merchant-conn helpers set the app.merchant_id GUC.
func WithTestMerchant(ctx context.Context) context.Context {
	return merchant.WithID(ctx, TestMerchantID)
}

// ArmDestructiveActions turns ON the #836 instance kill switch and arms the
// given merchant for enforcing pulls (#835). Both ship DISABLED so that a fresh
// deployment cancels nothing until an operator deliberately arms it; a test
// that exercises destructive convergence must therefore put itself in the state
// a live, reviewed deployment would be in. Tests that assert the SAFE default
// must not call this.
//
// It takes *t* and the merchant id rather than a caller-supplied handle, and
// derives the correctly-pinned one itself. The earlier handle-taking signature
// invited a silent mismatch — several callers passed a pool pinned to the
// canonical test merchant while arming a DIFFERENT, freshly minted one, and the
// merchant-scoped INSERT below was denied by RLS (42501). The two statements
// need different handles anyway: destructive_action_switch is the deployment-wide
// kill switch (RLS-exempt), merchant_destructive_policy is merchant-owned.
func ArmDestructiveActions(ctx context.Context, t testing.TB, merchantID uuid.UUID) {
	t.Helper()
	tt, ok := t.(*testing.T)
	require.True(t, ok, "ArmDestructiveActions needs a *testing.T to open a merchant-pinned pool")
	pool := SharedMerchantPool(tt, merchantID)
	_, err := pool.Exec(ctx, `UPDATE openrails.destructive_action_switch SET enabled = true`)
	require.NoError(t, err, "arm destructive action switch")
	_, err = pool.Exec(ctx,
		`INSERT INTO openrails.merchant_destructive_policy (merchant_id, destructive_actions_enabled, enforce_armed_at)
		 VALUES ($1, true, now())
		 ON CONFLICT (merchant_id) DO UPDATE SET destructive_actions_enabled = true, enforce_armed_at = now()`,
		merchantID)
	require.NoError(t, err, "arm merchant enforcement")
}

// DisarmDestructiveActions restores the default-safe state (the instance kill
// switch OFF), for tests that flip it mid-scenario.
func DisarmDestructiveActions(ctx context.Context, t testing.TB, qx gen.DBTX) {
	t.Helper()
	_, err := qx.Exec(ctx, `UPDATE openrails.destructive_action_switch SET enabled = false`)
	require.NoError(t, err, "disarm destructive action switch")
}
