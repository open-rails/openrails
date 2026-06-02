//go:build integration

package riverjobs_test

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/credits"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// recordingSink captures every LowBalanceEvent the job emits so the test can
// assert how many alerts fired and for which owners.
type recordingSink struct {
	mu     sync.Mutex
	events []credits.LowBalanceEvent
}

func (r *recordingSink) Handle(_ context.Context, ev credits.LowBalanceEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func (r *recordingSink) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func (r *recordingSink) ownerIDs() []uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]uuid.UUID, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.OwnerID)
	}
	return out
}

func TestLowBalanceAlertWorker_AlertsOnceRespectsCooldownAndOwnerScoped(t *testing.T) {
	dsn := os.Getenv("OPENRAILS_TEST_DB_URL")
	if dsn == "" {
		t.Skip("set OPENRAILS_TEST_DB_URL to run integration tests")
	}

	sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = sqlDB.Close() })
	bunDB := bun.NewDB(sqlDB, pgdialect.New())
	models.RegisterModels(bunDB)

	ctx := context.Background()
	require.NoError(t, bunDB.PingContext(ctx))

	// Require the #240 columns to exist.
	var hasCols bool
	require.NoError(t, bunDB.NewSelect().
		ColumnExpr("EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='billing' AND table_name='user_credit_balances' AND column_name='last_low_balance_alert_at')").
		Scan(ctx, &hasCols))
	if !hasCols {
		t.Skip("migration 052 not applied; run migrations before integration tests")
	}

	dbi, err := db.NewWithBun(bunDB)
	require.NoError(t, err)

	clock := clockwork.NewFakeClockAt(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	now := clock.Now().UTC()

	tenantID := uuid.New()
	threshold := int64(100)
	creditTypeName := "test_low_balance_" + uuid.NewString()
	creditTypeID := uuid.New()

	// Owner A: under threshold (should alert). Owner B: above threshold (must not
	// alert) — exercises owner scoping. Owner C: under threshold on a DIFFERENT
	// (un-thresholded) credit type would not alert, but we keep one type for focus.
	ownerUnder := uuid.New()
	ownerAbove := uuid.New()
	ownerSecond := uuid.New() // a second under-threshold owner, must alert independently

	_, err = bunDB.NewInsert().Model(&models.CreditType{
		ID:                  creditTypeID,
		Name:                creditTypeName,
		DisplayName:         "Test Low Balance",
		Unit:                "units",
		DecimalPlaces:       0,
		IsActive:            true,
		LowBalanceThreshold: &threshold,
		CreatedAt:           now,
	}).Exec(ctx)
	require.NoError(t, err)

	mkBalance := func(owner uuid.UUID, balance, held int64) {
		_, err := bunDB.NewInsert().Model(&models.UserCreditBalance{
			ID:           uuidutil.NewV7(),
			TenantID:     tenantID,
			OwnerID:      owner,
			UserID:       owner.String(),
			CreditTypeID: creditTypeID,
			Balance:      balance,
			HeldBalance:  held,
			CreatedAt:    now,
			UpdatedAt:    now,
		}).Exec(ctx)
		require.NoError(t, err)
	}
	// available = balance - held
	mkBalance(ownerUnder, 50, 0)    // 50 < 100 -> alert
	mkBalance(ownerAbove, 500, 0)   // 500 >= 100 -> no alert
	mkBalance(ownerSecond, 120, 40) // available 80 < 100 -> alert

	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.UserCreditBalance)(nil)).Where("credit_type_id = ?", creditTypeID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.CreditType)(nil)).Where("id = ?", creditTypeID).Exec(ctx)
	})

	sink := &recordingSink{}
	cfg := &config.Config{Credits: &config.CreditsConfig{LowBalanceAlertCooldownHours: 24}}
	worker := &riverjobs.LowBalanceAlertWorker{
		DB:     dbi,
		Config: cfg,
		Clock:  clock,
		Sink:   sink,
	}
	runJob := func() {
		require.NoError(t, worker.Work(ctx, &river.Job[riverjobs.LowBalanceAlertArgs]{Args: riverjobs.LowBalanceAlertArgs{}}))
	}

	// Run 1: both under-threshold owners alert; the above-threshold owner does not.
	runJob()
	require.Equal(t, 2, sink.count(), "exactly the two under-threshold owners should alert")
	require.ElementsMatch(t, []uuid.UUID{ownerUnder, ownerSecond}, sink.ownerIDs())

	// last_low_balance_alert_at stamped for the under-threshold owners only.
	var stamped time.Time
	require.NoError(t, bunDB.NewSelect().
		Model((*models.UserCreditBalance)(nil)).
		Column("last_low_balance_alert_at").
		Where("owner_id = ? AND credit_type_id = ?", ownerUnder, creditTypeID).
		Scan(ctx, &stamped))
	require.WithinDuration(t, now, stamped, time.Second)

	var aboveStamp sql.NullTime
	require.NoError(t, bunDB.NewSelect().
		Model((*models.UserCreditBalance)(nil)).
		Column("last_low_balance_alert_at").
		Where("owner_id = ? AND credit_type_id = ?", ownerAbove, creditTypeID).
		Scan(ctx, &aboveStamp))
	require.False(t, aboveStamp.Valid, "above-threshold owner must not be stamped")

	// Run 2 within cooldown: no NEW alerts (still under threshold, but cooled down).
	clock.Advance(1 * time.Hour)
	runJob()
	require.Equal(t, 2, sink.count(), "cooldown must suppress duplicate alerts")

	// Run 3 after cooldown elapses: the still-under-threshold owners alert again.
	clock.Advance(25 * time.Hour)
	runJob()
	require.Equal(t, 4, sink.count(), "owners still under threshold re-alert after cooldown")
}
