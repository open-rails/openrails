package checkout

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestAcceptedPurchasedCreditPromise(t *testing.T) {
	face := int64(20_000_000)
	product := &models.Product{CreditGrant: &catalog.CreditGrantSpec{Currency: "USD", Amount: &face}}
	price := &models.Price{Amount: 18_000_000, Currency: "USD"}
	terms, err := acceptedCreditGrant(product, price)
	require.NoError(t, err)
	require.EqualValues(t, 20_000_000, terms.Amount, "bulk-discount credit differs from cash charged")
	require.Equal(t, 365, terms.ExpiresAfterDays)
	require.True(t, terms.StartsAt.IsZero(), "credit lifetime starts at fulfillment, not admission")
	require.Nil(t, terms.ExpiresAt)
	face = 10_000_000
	require.EqualValues(t, 20_000_000, terms.Amount, "accepted amount does not alias mutable product")

	product.CreditGrant.Amount, product.CreditGrant.FromPayment = nil, true
	price.Amount = 100_000_000
	terms, err = acceptedCreditGrant(product, price)
	require.NoError(t, err)
	require.Equal(t, price.Amount, terms.Amount, "deposit grants the chosen and charged amount")
	price.Currency = "EUR"
	_, err = acceptedCreditGrant(product, price)
	require.ErrorContains(t, err, "currency")
	price.Currency, price.AutoRenew = "USD", true
	_, err = acceptedCreditGrant(product, price)
	require.ErrorContains(t, err, "recurring")
}

func TestCreditPromiseRetainsFirstFulfillmentDates(t *testing.T) {
	promise := &models.CreditGrantSnapshot{Amount: 1_000_000, Currency: "USD", ExpiresAfterDays: 365}
	fulfilled := models.CloneCreditGrantSnapshot(promise)
	fulfilled.StartsAt = time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	expiry := fulfilled.StartsAt.AddDate(0, 0, 365)
	fulfilled.ExpiresAt = &expiry
	require.NoError(t, fulfilled.Validate())
	require.True(t, models.SameCreditGrantPromise(promise, fulfilled))
	bad := models.CloneCreditGrantSnapshot(fulfilled)
	*bad.ExpiresAt = bad.ExpiresAt.Add(time.Hour)
	require.Error(t, bad.Validate())
	require.Equal(t, expiry, *fulfilled.ExpiresAt)
	require.False(t, models.SameCreditGrantPromise(nil, fulfilled), "legacy no-benefit agreement never gains today's benefit")
}
