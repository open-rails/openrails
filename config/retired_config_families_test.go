package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// or#893 phase 3. Seven retired config families used to log.Warn and boot, so an
// operator could believe ignored security or tenant config was active. Every one
// of them now REFUSES boot, and the error names the replacement.
//
// The exact text is pinned: an operator reads it once, in the crash, and has to
// be able to act on it without reading the source.

func TestRetiredConfigFamiliesRefuseBootFromEnv(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "store (#520)",
			env:  map[string]string{"STORE_NAME": "Acme"},
			want: "store config was removed (#520): seed merchant profile fields with openrails push-merchant-config under merchants[].profile; delete the store yaml key and STORE_* env vars",
		},
		{
			name: "merchant (#520/#521)",
			env:  map[string]string{"MERCHANT": "acme"},
			want: "merchant config was removed (#520/#521): seed merchants with openrails push-merchant-config; standalone no longer pins a process-wide merchant — delete the merchant yaml key and MERCHANT env var",
		},
		{
			name: "cors_origins (#519/#765)",
			env:  map[string]string{"CORS_ORIGINS": "https://app.example.test"},
			want: "cors_origins config was removed (#519/#765): browser CORS is a fixed engine policy (checkout/self-service = public *, everything else = none) — bearer JWTs are the security boundary, not a configurable origin allowlist; delete the cors_origins yaml key and CORS_ORIGINS env var",
		},
		{
			name: "db.require_rls",
			env:  map[string]string{"DB_REQUIRE_RLS": "true"},
			want: "db.require_rls config was removed: RLS enforcement is derived from env — development may bypass RLS, every other env requires an RLS-enforcing DB role; delete the db.require_rls yaml key and DB_REQUIRE_RLS env var",
		},
		{
			name: "auth.issuers (#521/#527)",
			env:  map[string]string{"AUTH_ISSUERS": `["http://a.test"]`},
			want: "auth.issuers / auth.expected_audience config was removed (#521/#527): declare each merchant's host-app trust under merchants[].remote_application in the merchant config manifest; delete the keys and AUTH_ISSUERS / AUTH_EXPECTED_AUDIENCE env vars",
		},
		{
			name: "auth.expected_audience (#521/#527)",
			env:  map[string]string{"AUTH_EXPECTED_AUDIENCE": "legacy-audience"},
			want: "auth.issuers / auth.expected_audience config was removed (#521/#527): declare each merchant's host-app trust under merchants[].remote_application in the merchant config manifest; delete the keys and AUTH_ISSUERS / AUTH_EXPECTED_AUDIENCE env vars",
		},
		{
			name: "rails (#521)",
			env:  map[string]string{"RAILS_STRIPE_SECRET_KEY": "sk_test_x"},
			want: "rails config was removed (#521): seed merchant PSPs and secrets with openrails push-merchant-config under merchants[].psps; delete the rails yaml key and RAILS_* env vars",
		},
		{
			name: "auth.control_plane (#521)",
			env:  map[string]string{"AUTH_CONTROL_PLANE_ISSUER": "http://cp.test"},
			want: "auth.control_plane config was removed (#521): use auth.issuer (env AUTH_ISSUER) — audiences are fixed to openrails, standalone public hosted registration is unavailable in this repo, and platform-superadmin belongs in openrails-saas; delete the auth.control_plane keys and AUTH_CONTROL_PLANE_* env vars",
		},
		{
			name: "private_port",
			env:  map[string]string{"PRIVATE_PORT": "9443"},
			want: "private_port was removed: OpenRails serves a single HTTP listener and there is no separate internal port; delete the private_port yaml key and PRIVATE_PORT env var",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := Load("nonexistent-config.yaml")
			require.Error(t, err, "a retired config family must never boot")
			require.EqualError(t, err, tc.want)
		})
	}
}

func TestRetiredConfigFamiliesRefuseBootFromYAML(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"store", "store:\n  name: Acme\n", "store config was removed (#520)"},
		{"merchant", "merchant: acme\n", "merchant config was removed (#520/#521)"},
		{"cors_origins", "cors_origins:\n  - https://app.example.test\n", "cors_origins config was removed (#519/#765)"},
		{"db.require_rls", "db:\n  require_rls: true\n", "db.require_rls config was removed"},
		{"auth.issuers", "auth:\n  issuers:\n    - http://a.test\n", "auth.issuers / auth.expected_audience config was removed (#521/#527)"},
		{"rails", "rails:\n  stripe:\n    secret_key: sk_test_x\n", "rails config was removed (#521)"},
		{"auth.control_plane", "auth:\n  control_plane:\n    issuer: http://cp.test\n", "auth.control_plane config was removed (#521)"},
		{"private_port", "private_port: 9443\n", "private_port was removed"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(path, []byte(tc.yaml), 0o600))
			_, err := Load(path)
			require.Error(t, err, "a retired config family must never boot")
			require.ErrorContains(t, err, tc.want)
		})
	}
}
