package orders

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

func TestCheckAttribution(t *testing.T) {
	text := func(s string) *string { return &s }
	got, err := CheckAttribution(&billing.OrderAttribution{Source: text(" ads "), Medium: text("  "), Referrer: text("https://Shop.Example.COM./p?q=1")})
	require.NoError(t, err)
	require.Equal(t, "ads", *got.Source)
	require.Nil(t, got.Medium, "blank is absent")
	require.Equal(t, "shop.example.com", *got.Referrer)
	got, err = CheckAttribution(&billing.OrderAttribution{Referrer: text("news.example.org")})
	require.NoError(t, err)
	require.Equal(t, "news.example.org", *got.Referrer)
	for field, bad := range map[string]billing.OrderAttribution{
		"attribution.source":    {Source: text(strings.Repeat("x", 101))},
		"attribution.medium":    {Medium: text("a\tb")},
		"attribution.campaign":  {Campaign: text("someone@example.com")},
		"attribution.affiliate": {Affiliate: text("4111111111111111")},
		"attribution.referrer":  {Referrer: text("https://[::1]/")},
	} {
		_, err := CheckAttribution(&bad)
		require.Error(t, err, field)
		require.Contains(t, err.Error(), field)
	}
}

func TestCountry(t *testing.T) {
	require.Equal(t, "DE", Country(" de "))
	require.Empty(t, Country("DEU"))
	require.Empty(t, Country("D1"))
}
