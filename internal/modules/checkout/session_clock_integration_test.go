//go:build integration

package checkout

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func TestFindOpenCheckoutUsesBusinessClockForExpiry(t *testing.T) {
	fx := newSaleIntentFixture(t)
	now := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	expiry := now.Add(time.Hour)
	clock := clockwork.NewFakeClockAt(now)
	repo := NewCheckoutSessionRepo(fx.db)
	session := &models.CheckoutSession{
		ID: uuid.New(), CustomerID: uuid.MustParse(fx.userID), PriceID: fx.priceID,
		PspID: dbtest.EnsureTestPSP(fx.ctx, t, fx.db.Pool(), dbtest.TestMerchantID.UUID(), "mobius"),
		Mode:  models.CheckoutSessionModeOneOff, Rail: models.RailNMI,
		Status: models.CheckoutSessionStatusCreated, Amount: 100, Currency: "USD",
		CreatedAt: now, UpdatedAt: now, ExpiresAt: &expiry,
	}
	require.NoError(t, repo.Create(fx.ctx, session))
	t.Cleanup(func() {
		_, _ = fx.db.Pool().Exec(fx.ctx, "DELETE FROM openrails.checkout_sessions WHERE id = $1", session.ID)
	})
	svc := &CheckoutSessionService{repo: repo, clock: clock}
	got, err := svc.FindOpenByUserPriceRail(fx.ctx, fx.userID, fx.priceID, models.RailNMI)
	require.NoError(t, err)
	require.NotNil(t, got, "physical time must not expire a simulated checkout")
	require.Equal(t, session.ID, got.ID)
	// At the precise expiry boundary it can no longer be reused.
	clock.Advance(time.Hour)
	got, err = svc.FindOpenByUserPriceRail(fx.ctx, fx.userID, fx.priceID, models.RailNMI)
	require.NoError(t, err)
	require.Nil(t, got)
}
