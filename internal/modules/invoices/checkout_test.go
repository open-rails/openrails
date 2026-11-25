package invoices

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
)

func TestCheckAttribution(t *testing.T) {
	text := func(s string) *string { return &s }
	got, drops := CheckAttribution(&billing.InvoiceAttribution{Source: text(" ads "), Medium: text("  "), Campaign: text("spring%2D2026"), Term: text("shoes"), Content: text("banner%20a"), Referrer: text("https://Shop.Example.COM./p?q=1")})
	require.Empty(t, drops)
	require.Equal(t, "ads", *got.Source)
	require.Nil(t, got.Medium, "blank is absent")
	require.Equal(t, "spring-2026", *got.Campaign)
	require.Equal(t, "shoes", *got.Term)
	require.Equal(t, "banner a", *got.Content)
	require.Equal(t, "shop.example.com", *got.Referrer)
	got, drops = CheckAttribution(&billing.InvoiceAttribution{Referrer: text("partner_shop.example.org:8443/landing"), Campaign: text("50%off")})
	require.Empty(t, drops)
	require.Equal(t, "partner_shop.example.org", *got.Referrer)
	require.Equal(t, "50%off", *got.Campaign, "a lone % stays as written")
	for want, bad := range map[AttributionDrop]billing.InvoiceAttribution{
		{"source", "too_long"}:         {Source: text(strings.Repeat("x", 101))},
		{"medium", "unprintable"}:      {Medium: text("a\tb")},
		{"source", "unprintable"}:      {Source: text("zero\u200bwidth")},
		{"campaign", "unprintable"}:    {Campaign: text("bidi\u202eflip")},
		{"campaign", "personal_data"}:  {Campaign: text("someone\uff20example.com")},
		{"medium", "personal_data"}:    {Medium: text("someone\ufe6bexample.com")},
		{"source", "personal_data"}:    {Source: text("someone%2540example.com")},
		{"affiliate", "personal_data"}: {Affiliate: text("4242.4242.4242.4242")},
		{"term", "personal_data"}:      {Term: text("a@example.com")},
		{"content", "too_long"}:        {Content: text(strings.Repeat(" ", maxRawAttribution) + "x")},
		{"referrer", "too_long"}:       {Referrer: text("example.com/" + strings.Repeat("a", maxRawAttribution))},
	} {
		kept, drops := CheckAttribution(&bad)
		require.Equal(t, []AttributionDrop{want}, drops)
		require.Equal(t, billing.InvoiceAttribution{}, kept, "%v leaves nothing", want)
	}
	for _, digits := range []string{"4242/4242/4242/4242", "\uff14\uff12\uff14\uff12\uff14\uff12\uff14\uff12\uff14\uff12\uff14\uff12\uff14\uff12\uff14\uff12"} {
		_, drops := CheckAttribution(&billing.InvoiceAttribution{Affiliate: text(digits)})
		require.Equal(t, []AttributionDrop{{"affiliate", "personal_data"}}, drops, digits)
	}
	for _, host := range []string{"https://[2001:db8::1]/", "https://exa%zzmple.com/", "not a host"} {
		kept, drops := CheckAttribution(&billing.InvoiceAttribution{Referrer: text(host), Source: text("kept")})
		require.Equal(t, []AttributionDrop{{"referrer", "not_a_host"}}, drops, host)
		require.Equal(t, "kept", *kept.Source)
	}
}

func TestCountry(t *testing.T) {
	require.Equal(t, "DE", Country(" de "))
	require.Empty(t, Country("DEU"))
	require.Empty(t, Country("D1"))
}

func TestCustomerType(t *testing.T) {
	for _, c := range []struct {
		row  gen.GetCheckoutCustomerRow
		want billing.CustomerType
	}{
		{gen.GetCheckoutCustomerRow{}, billing.CustomerTypeNew},
		{gen.GetCheckoutCustomerRow{Purchases: 1}, billing.CustomerTypeReturning},
		{gen.GetCheckoutCustomerRow{Purchases: 2, HadMrr: true, HasMrr: true}, billing.CustomerTypeReturning},
		{gen.GetCheckoutCustomerRow{HadMrr: true, HasMrr: true}, billing.CustomerTypeReturning},
		{gen.GetCheckoutCustomerRow{Purchases: 1, HadMrr: true}, billing.CustomerTypeReactivated},
		{gen.GetCheckoutCustomerRow{HadMrr: true}, billing.CustomerTypeReactivated},
	} {
		require.Equal(t, c.want, customerType(c.row), "%+v", c.row)
	}
}
