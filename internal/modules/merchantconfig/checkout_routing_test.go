package merchantconfig

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
)

func TestNormalizeCheckoutRoutingCanonicalises(t *testing.T) {
	t.Parallel()

	out, err := NormalizeCheckoutRouting([]models.CheckoutRoutingRule{
		{
			Match:  models.CheckoutRoutingMatch{Currency: " USD ", Country: "us", Mode: " Subscription "},
			Prefer: []string{" Mobius ", "CCBill"},
		},
		{Prefer: []string{"stripe"}},
	})
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.Equal(t, "usd", out[0].Match.Currency)
	assert.Equal(t, "US", out[0].Match.Country)
	assert.Equal(t, "subscription", out[0].Match.Mode)
	assert.Equal(t, []string{"mobius", "ccbill"}, out[0].Prefer)
	assert.True(t, out[1].Match.IsCatchAll())
}

func TestNormalizeCheckoutRoutingRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		rules []models.CheckoutRoutingRule
		want  string
	}{
		{
			name:  "empty prefer routes nothing",
			rules: []models.CheckoutRoutingRule{{Prefer: nil}},
			want:  "must name at least one PSP",
		},
		{
			name:  "repeated selector is a paste error",
			rules: []models.CheckoutRoutingRule{{Prefer: []string{"mobius", "mobius"}}},
			want:  `repeats "mobius"`,
		},
		{
			name: "a rule after the catch-all can never match",
			rules: []models.CheckoutRoutingRule{
				{Prefer: []string{"mobius"}},
				{Match: models.CheckoutRoutingMatch{Currency: "eur"}, Prefer: []string{"ccbill"}},
			},
			want: "unreachable",
		},
		{
			name:  "bad currency",
			rules: []models.CheckoutRoutingRule{{Match: models.CheckoutRoutingMatch{Currency: "dollars"}, Prefer: []string{"mobius"}}},
			want:  "ISO-4217",
		},
		{
			name:  "bad country",
			rules: []models.CheckoutRoutingRule{{Match: models.CheckoutRoutingMatch{Country: "USA"}, Prefer: []string{"mobius"}}},
			want:  "ISO-3166-1",
		},
		{
			name:  "bad mode",
			rules: []models.CheckoutRoutingRule{{Match: models.CheckoutRoutingMatch{Mode: "recurring"}, Prefer: []string{"mobius"}}},
			want:  "one_off or subscription",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := NormalizeCheckoutRouting(tt.rules)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestNormalizeCheckoutRoutingEmptyIsNoPolicy(t *testing.T) {
	t.Parallel()

	out, err := NormalizeCheckoutRouting(nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}
