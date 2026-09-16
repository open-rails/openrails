//go:build integration

package riverjobs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/merchant"
)

// or#877: the two scheduled workers FC-16 turned up. Both are driven on the
// BARE context a River job actually receives — handing them a pinned merchant
// connection would do the worker's own job for it and turn an inert pass green,
// which is exactly the harness mistake that hid this whole family.

// singleStripeAccount arms one Stripe account for one merchant and refuses
// every other rail. It verifies the lister pinned the account it was given.
type singleStripeAccount struct {
	railresolve.Source
	merchantID uuid.UUID
	pspID      uuid.UUID
	pinned     atomic.Int32
}

func (s *singleStripeAccount) Armed(ctx context.Context, rail string) (bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	return rail == string(models.RailStripe) && mid.UUID() == s.merchantID, nil
}

func (s *singleStripeAccount) RailConfig(ctx context.Context, rail, _ string) (*config.PSPConfig, error) {
	if ok, err := s.Armed(ctx, rail); err != nil || !ok {
		return nil, railresolve.ErrRailNotArmed
	}
	if db.PSPIDFromContext(ctx) == s.pspID {
		s.pinned.Add(1)
	}
	return &config.PSPConfig{ID: s.pspID, Rail: models.RailStripe, AccountID: "acct_test",
		Stripe: &config.StripeRailConfig{SecretKey: "sk_test_catalog_drift"}}, nil
}

// TestCatalogReconciliationPullRunsPerMerchant proves the scheduled pass runs
// inside each merchant's scope and that a complete read of that merchant's
// active account resolves a vanished finding, while another account's finding
// stays open because nothing read it.
func TestCatalogReconciliationPullRunsPerMerchant(t *testing.T) {
	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	dbtest.EnsureTestMerchant(ctx, t, pool)
	mid := dbtest.TestMerchantID.UUID()

	insertPSP := func(rail string) uuid.UUID {
		var id uuid.UUID
		require.NoError(t, pool.QueryRow(ctx, `INSERT INTO openrails.psps (merchant_id, rail, environment, account_id)
			VALUES ($1, $2, 'test', $3) RETURNING id`, mid, rail, "acct_or877_"+uuid.NewString()[:8]).Scan(&id))
		return id
	}
	stripePSP, nmiPSP := insertPSP("stripe"), insertPSP("nmi")
	insertFinding := func(psp uuid.UUID, rail, kind string) uuid.UUID {
		var id uuid.UUID
		external := "obj_" + uuid.NewString()[:8]
		require.NoError(t, pool.QueryRow(ctx, `INSERT INTO openrails.reconciliation_findings
			(merchant_id, finding_type, subject_key, severity, status, rail, psp_id, openrails_resource_type, external_resource_id, last_seen_at)
			VALUES ($1, 'catalog.' || $4, jsonb_build_array($2::uuid::text, 'price', '', $5::text, '')::text, 'low', 'reconcile_required', $3, $2::uuid, 'price', $5, now() - interval '1 minute')
			RETURNING id`, mid, psp, rail, kind, external).Scan(&id))
		return id
	}
	stripeFinding := insertFinding(stripePSP, "stripe", "orphan_in_stripe")
	nmiFinding := insertFinding(nmiPSP, "nmi", "orphan_in_nmi")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.reconciliation_findings WHERE id = ANY($1)", []uuid.UUID{stripeFinding, nmiFinding})
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.psps WHERE id = ANY($1)", []uuid.UUID{stripePSP, nmiPSP})
	})

	var calls atomic.Int32
	stripeAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "Bearer sk_test_catalog_drift", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[],"has_more":false}`))
	}))
	t.Cleanup(stripeAPI.Close)
	rails := &singleStripeAccount{merchantID: mid, pspID: stripePSP}
	worker := CatalogReconciliationPullWorker{DB: dbi, Config: &config.Config{}, Rails: rails, StripeBaseURL: stripeAPI.URL}
	require.NoError(t, worker.Work(ctx, nil))
	require.EqualValues(t, 2, calls.Load(), "products and prices were listed")
	require.GreaterOrEqual(t, rails.pinned.Load(), int32(2), "every list call was pinned to the active account")

	var stripeResolved, nmiResolved *time.Time
	require.NoError(t, pool.QueryRow(ctx, "SELECT resolved_at FROM openrails.reconciliation_findings WHERE id = $1", stripeFinding).Scan(&stripeResolved))
	require.NoError(t, pool.QueryRow(ctx, "SELECT resolved_at FROM openrails.reconciliation_findings WHERE id = $1", nmiFinding).Scan(&nmiResolved))
	require.NotNil(t, stripeResolved, "a complete read of the merchant's Stripe account proves the orphan is gone")
	require.Nil(t, nmiResolved, "an unread NMI account keeps its finding")
}
