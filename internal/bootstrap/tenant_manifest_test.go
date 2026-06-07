package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/pkg/tenant"
	"github.com/stretchr/testify/require"
)

func TestLoadTenantManifestRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tenants.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
version: 2
tenants:
  - slug: cozy-art
    operator_tenant_slug: legacy
`), 0o600))

	_, err := loadTenantManifest(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "operator_tenant_slug")
}

func TestLoadTenantManifestRejectsServiceTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tenants.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
version: 2
tenants:
  - slug: cozy-art
    name: Cozy Art
    service_tokens:
      - name: runtime
        permissions: [openrails:admin]
        outputs:
          - file:
              path: /tmp/openrails-runtime-token
`), 0o600))

	_, err := loadTenantManifest(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "service_tokens")
}

func TestLoadTenantManifestRequiresVersion2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tenants.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
version: 1
tenants:
  - slug: cozy-art
`), 0o600))

	_, err := loadTenantManifest(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "version must be 2")
}

func TestResolveManifestResourcesMapsTenantAliasAndLeavesHostResourcesOpaque(t *testing.T) {
	tid := tenant.DefaultID
	resources, err := resolveManifestResources([]ManifestResource{
		{Kind: controlplane.ResourceKindTenant, ID: "$tenant"},
		{Kind: "custom.resource", ID: "alpha"},
	}, tid, "cozy-art")
	require.NoError(t, err)
	require.Len(t, resources, 2)
	require.Equal(t, controlplane.ResourceKindTenant, resources[0].Kind)
	require.Equal(t, tid.String(), resources[0].ID)
	require.Equal(t, "custom.resource", resources[1].Kind)
	require.Equal(t, "alpha", resources[1].ID)
}

func TestResolveManifestResourcesDefaultsToTenantScope(t *testing.T) {
	resources, err := resolveManifestResources(nil, tenant.DefaultID, "cozy-art")
	require.NoError(t, err)
	require.Len(t, resources, 1)
	require.Equal(t, controlplane.ResourceKindTenant, resources[0].Kind)
	require.Equal(t, tenant.DefaultID.String(), resources[0].ID)
}

func TestParseBootstrapManifest(t *testing.T) {
	manifest, err := ParseBootstrapManifest([]byte(`
version: 1
tenants:
  - slug: cozy-art
    name: Cozy Art
    issuers:
      - issuer: https://auth.cozy.art
        jwks_uri: https://auth.cozy.art/.well-known/jwks.json
        audiences: [openrails]
    service_jwt_principals:
      - issuer: https://auth.cozy.art
        subject: service:cozy-art-runtime
        permissions: [openrails:entitlements:read]
catalogs:
  - name: default
    default_currency: usd
    tier_groups:
      - slug: plans
        display_name: Plans
        products:
          - slug: starter
            display_name: Starter
            tier_rank: 1
            prices:
              - unit_amount: 1000
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
			name: "missing issuer audiences",
			body: base(`
    issuers:
      - issuer: https://auth.cozy.art
        jwks_uri: https://auth.cozy.art/.well-known/jwks.json
`),
			want: "audiences are required",
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
			name: "invalid resource shape",
			body: base(`
    service_jwt_principals:
      - issuer: https://auth.cozy.art
        subject: service:runtime
        permissions: [openrails:entitlements:read]
        resources:
          - kind: openrails.tenant
`),
			want: "resource kind and id are required",
		},
		{
			name: "duplicate product slug",
			body: `
version: 1
catalogs:
  - default_currency: usd
    tier_groups:
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
  - default_currency: usd
    tier_groups:
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
  - default_currency: usd
    tier_groups:
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
