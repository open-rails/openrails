//go:build integration

package checkout

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
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

// pspCapturingCancel records the PSP pinned on the context it is confirmed
// with, then refuses so the flow stops before MarkSucceeded (the fixture has no
// real subscription row for the FK the succeeded session would carry).
type pspCapturingCancel struct{ pinned uuid.UUID }

var errCancelProbe = errors.New("probe: stop after capturing the pin")

func (c *pspCapturingCancel) Confirm(ctx context.Context, _ uuid.UUID, _ string) error {
	c.pinned = db.PSPIDFromContext(ctx)
	return errCancelProbe
}

// A Solana lifecycle session must be persistable (the mode CHECK admits
// solana_cancel) and the poller-driven confirm must run with the session's PSP
// pinned (or#893/#704) so the rows it writes are attributable.
func TestConfirmSolanaLifecycleSessionPersistsAndPinsSessionPSP(t *testing.T) {
	fx := newSaleIntentFixture(t)
	now := time.Now().UTC()
	expiry := now.Add(time.Hour)
	repo := NewCheckoutSessionRepo(fx.db)
	pspID := dbtest.EnsureTestPSP(fx.ctx, t, fx.db.Pool(), dbtest.TestMerchantID.UUID(), "solana")
	subscriptionID := uuid.New()
	session := &models.CheckoutSession{
		ID: uuid.New(), CustomerID: uuid.MustParse(fx.userID), PriceID: fx.priceID, PspID: pspID,
		Mode: models.CheckoutSessionModeSolanaCancel, Rail: models.RailSolana,
		Status: models.CheckoutSessionStatusRequiresAction, Amount: 100, Currency: "USD",
		RailState: map[string]any{"subscription_id": subscriptionID.String()},
		CreatedAt: now, UpdatedAt: now, ExpiresAt: &expiry,
	}
	require.NoError(t, repo.Create(fx.ctx, session), "solana_cancel sessions must be admitted by checkout_sessions_mode_check")
	t.Cleanup(func() {
		_, _ = fx.db.Pool().Exec(fx.ctx, "DELETE FROM openrails.checkout_sessions WHERE id = $1", session.ID)
	})

	cancel := &pspCapturingCancel{}
	svc := &CheckoutSessionService{repo: repo}
	svc.SetSolanaLifecycleForTest(nil, nil, cancel, nil, nil, nil)

	err := svc.ConfirmSolanaLifecycleSession(fx.ctx, session.ID, "sig")
	require.ErrorIs(t, err, errCancelProbe, "the confirm must reach the cancel mirror")
	require.Equal(t, pspID, cancel.pinned, "confirm must run with the session's PSP pinned")
}
