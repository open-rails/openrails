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
	days := 7
	price := &models.Price{Amount: 15_000_000, TrialUnitAmount: &initial, TrialDurationDays: &days}

	require.Equal(t, now.AddDate(0, 0, 7).Unix(), stripeCheckoutTrialEnd(price, nil, now))
}

func TestStripeCheckoutTrialEndKeepsCoverageDelay(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	initial := int64(0)
	days := 7
	price := &models.Price{Amount: 15_000_000, TrialUnitAmount: &initial, TrialDurationDays: &days}
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
	days := 30
	price := &models.Price{Amount: 14_950_000, TrialUnitAmount: &initial, TrialDurationDays: &days}

	require.Zero(t, stripeCheckoutTrialEnd(price, nil, now))
	require.True(t, stripePaidIntroUnsupported(price))
}
