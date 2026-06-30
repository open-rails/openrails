package checkout

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestStripeCheckoutTrialEndUsesFreeTrialIntro(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	initial := int64(0)
	trialHours := 7 * 24
	price := &models.Price{Amount: 15_000_000, TrialUnitAmount: &initial, TrialDurationHours: &trialHours}

	require.Equal(t, now.AddDate(0, 0, 7).Unix(), stripeCheckoutTrialEnd(price, nil, now))
}

func TestStripeCheckoutTrialEndKeepsCoverageDelay(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	initial := int64(0)
	trialHours := 7 * 24
	price := &models.Price{Amount: 15_000_000, TrialUnitAmount: &initial, TrialDurationHours: &trialHours}
	coverageEnd := now.Add(48 * time.Hour)

	require.Equal(t, coverageEnd.Unix(), stripeCheckoutTrialEnd(price, &CoverageInfo{
		HasCoverage: true,
		EndDate:     &coverageEnd,
	}, now))
}

func TestStripeCheckoutTrialEndIgnoresPaidIntro(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	initial := int64(19_950_000)
	trialHours := 30 * 24
	price := &models.Price{Amount: 14_950_000, TrialUnitAmount: &initial, TrialDurationHours: &trialHours}

	require.Zero(t, stripeCheckoutTrialEnd(price, nil, now))
	require.True(t, stripePaidIntroUnsupported(price))
}
