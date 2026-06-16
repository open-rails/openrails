package bootstrap

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseBootstrapManifest(t *testing.T) {
	// A manifest merchant is a billing merchant (#480): slug + name + optional
	// owner-org ownership link (#481). Issuer/JWKS trust is AuthKit's
	// remote_application registry (#74), no longer declared here.
	manifest, err := ParseBootstrapManifest([]byte(`
version: 1
merchants:
  - slug: cozy-art
    name: Cozy Art
    owner_org_id: 11111111-1111-1111-1111-111111111111
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
	require.Len(t, manifest.Merchants, 1)
	require.Len(t, manifest.Catalogs, 1)
	cat, err := manifest.CatalogManifest(0)
	require.NoError(t, err)
	require.Equal(t, "starter", cat.TierGroups[0].Products[0].Slug)
}

func TestParseBootstrapManifestValidationErrors(t *testing.T) {
	base := func(fragment string) string {
		return `
version: 1
merchants:
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
			name: "missing merchant name",
			body: `
version: 1
merchants:
  - slug: cozy-art
`,
			want: `merchant "cozy-art" name is required`,
		},
		{
			name: "issuers field removed",
			body: base(`
    issuers:
      - issuer: https://auth.cozy.art
        jwks_uri: https://auth.cozy.art/.well-known/jwks.json
`),
			want: "issuers",
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
