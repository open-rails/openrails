package bootstrap

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseBootstrapManifest(t *testing.T) {
	// Registration is purely tenant + issuer + JWKS URI. Audiences are optional
	// (default openrails); there is no per-issuer permission grant — a tenant has
	// full authority over its own resources.
	manifest, err := ParseBootstrapManifest([]byte(`
version: 1
tenants:
  - slug: cozy-art
    name: Cozy Art
    issuers:
      - issuer: https://auth.cozy.art
        jwks_uri: https://auth.cozy.art/.well-known/jwks.json
catalogs:
  - name: default
    tier_groups:
      - slug: plans
        display_name: Plans
        products:
          - slug: starter
            display_name: Starter
            tier_rank: 1
            prices:
              - currency: usd
                unit_amount: 1000
                interval: month
`))
	require.NoError(t, err)
	require.Len(t, manifest.Tenants, 1)
	require.Len(t, manifest.Catalogs, 1)
	cat, err := manifest.CatalogManifest(0)
	require.NoError(t, err)
	require.Equal(t, "starter", cat.TierGroups[0].Products[0].Slug)
}

func TestParseBootstrapManifestValidationErrors(t *testing.T) {
	base := func(fragment string) string {
		return `
version: 1
tenants:
  - slug: cozy-art
    name: Cozy Art
` + fragment
	}
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "unknown key",
			body: `
version: 1
tenantz: []
`,
			want: "tenantz",
		},
		{
			name: "missing tenant name",
			body: `
version: 1
tenants:
  - slug: cozy-art
`,
			want: `tenant "cozy-art" name is required`,
		},
		{
			name: "invalid issuer url",
			body: base(`
    issuers:
      - issuer: not-a-url
        jwks_uri: https://auth.cozy.art/.well-known/jwks.json
        audiences: [openrails]
`),
			want: "must be an http(s) URL",
		},
		{
			name: "service tokens removed",
			body: base(`
	    service_tokens:
	      - name: runtime
	        permissions: [openrails:admin]
        outputs:
	          - vault:
	              path: openrails/runtime
	`),
			want: "service_tokens",
		},
		{
			name: "service_jwt_principals removed",
			body: base(`
    service_jwt_principals:
      - issuer: https://auth.cozy.art
        subject: service:runtime
        permissions: [openrails:entitlements:read]
`),
			want: "service_jwt_principals",
		},
		{
			name: "duplicate product slug",
			body: `
version: 1
catalogs:
  - tier_groups:
      - slug: g1
        display_name: G1
        products:
          - {slug: p, display_name: P, tier_rank: 1, prices: [{currency: usd, unit_amount: 1, interval: month}]}
      - slug: g2
        display_name: G2
        products:
          - {slug: p, display_name: P, tier_rank: 1, prices: [{currency: usd, unit_amount: 1, interval: month}]}
`,
			want: "duplicate product slug",
		},
		{
			name: "duplicate price terms",
			body: `
version: 1
catalogs:
  - tier_groups:
      - slug: g
        display_name: G
        products:
          - slug: p
            display_name: P
            tier_rank: 1
            prices:
              - {currency: usd, unit_amount: 1000, interval: month}
              - {currency: usd, unit_amount: 1000, interval: month}
`,
			want: "duplicate price terms",
		},
		{
			name: "invalid provider config",
			body: `
version: 1
catalogs:
  - tier_groups:
      - slug: g
        display_name: G
        products:
          - slug: p
            display_name: P
            tier_rank: 1
            providers: [solana]
            prices:
              - {currency: eur, unit_amount: 1000, interval: month}
`,
			want: "solana requires a stablecoin",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseBootstrapManifest([]byte(tc.body))
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}
