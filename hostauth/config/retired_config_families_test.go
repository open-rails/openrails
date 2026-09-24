package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// FC-5b (or#893): every retired config family REFUSES boot from env and yaml
// alike. The text is pinned exactly: the operator reads it once, in the crash,
// and must be able to act without the source.
func TestRetiredConfigFamiliesRefuseBoot(t *testing.T) {
	const issuers = "auth.issuers / auth.expected_audience config was removed (#521/#527): declare each merchant's host-app trust under merchants[].remote_application in the merchant config manifest; delete the keys and AUTH_ISSUERS / AUTH_EXPECTED_AUDIENCE env vars"
	const controlPlane = "auth.control_plane config was removed (#521): use auth.issuer (env AUTH_ISSUER) — audiences are fixed to openrails, standalone public hosted registration is unavailable in this repo, and platform-superadmin belongs in openrails-saas; delete the auth.control_plane keys and AUTH_CONTROL_PLANE_* env vars"
	for _, row := range []struct {
		env, value, yaml, want string
	}{
		{"STORE_NAME", "Acme", "store:\n  name: Acme\n", "store config was removed (#520): seed merchant profile fields with openrails push-merchant-config under merchants[].profile; delete the store yaml key and STORE_* env vars"},
		{"MERCHANT", "acme", "merchant: acme\n", "merchant config was removed (#520/#521): seed merchants with openrails push-merchant-config; standalone no longer pins a process-wide merchant — delete the merchant yaml key and MERCHANT env var"},
		{"CORS_ORIGINS", "https://app.example.test", "cors_origins:\n  - https://app.example.test\n", "cors_origins config was removed (#519/#765): browser CORS is a fixed engine policy (checkout/self-service = public *, everything else = none) — bearer JWTs are the security boundary, not a configurable origin allowlist; delete the cors_origins yaml key and CORS_ORIGINS env var"},
		{"DB_REQUIRE_RLS", "true", "db:\n  require_rls: true\n", "db.require_rls config was removed: merchant isolation uses explicit scoped queries, not PostgreSQL RLS; delete the db.require_rls yaml key and DB_REQUIRE_RLS env var"},
		{"AUTH_ISSUERS", `["http://a.test"]`, "auth:\n  issuers:\n    - http://a.test\n", issuers},
		{"AUTH_EXPECTED_AUDIENCE", "legacy", "auth:\n  expected_audience: legacy\n", issuers},
		{"RAILS_STRIPE_SECRET_KEY", "sk_test_x", "rails:\n  stripe:\n    secret_key: sk_test_x\n", "rails config was removed (#521): seed merchant PSPs and secrets with openrails push-merchant-config under merchants[].psps; delete the rails yaml key and RAILS_* env vars"},
		{"AUTH_CONTROL_PLANE_ISSUER", "http://cp.test", "auth:\n  control_plane:\n    issuer: http://cp.test\n", controlPlane},
		{"AUTH_CONTROL_PLANE_PUBLIC_HOSTED", "true", "auth:\n  control_plane:\n    platform_admin_user_id: u\n", controlPlane},
		{"PRIVATE_PORT", "9443", "private_port: 9443\n", "private_port was removed: OpenRails serves a single HTTP listener and there is no separate internal port; delete the private_port yaml key and PRIVATE_PORT env var"},
	} {
		t.Run(row.env, func(t *testing.T) {
			bootEnv(t)
			_, err := Load(writeFile(t, filepath.Join(t.TempDir(), "config.yaml"), row.yaml))
			require.EqualError(t, err, row.want, "yaml")
			t.Setenv(row.env, row.value)
			_, err = Load("absent.yaml")
			require.EqualError(t, err, row.want, "env")
		})
	}
}

// Other hard cuts and strict decoding: a setting that would be ignored refuses
// boot and names its replacement.
func TestRetiredAndUnknownInputsRefuseBoot(t *testing.T) {
	for _, row := range []struct {
		env, value string
		want       []string
	}{
		{"ENV", "", []string{"ENV is retired"}},
		{"API_URL", "https://x", []string{"API_URL is retired", "public_billing_base_url"}},
		{"NEW_SUBSCRIPTION_COLLECTION_POLICY", "x", []string{"is retired"}},
		{"CATALOG_SOURCE", "", []string{"catalogs always use the database", "allow_catalog_updates"}},
		{"MERCHANT_SOURCE", "manifest", []string{"select secret_backend and merchant_config_http independently"}},
		{"MERCHANT_CONFIG_SOURCE", "api", []string{"select secret_backend and merchant_config_http independently"}},
		{"MODE", "full", []string{"mode was removed", "provider_write_mode"}},
		{"BILLING_MODE", "full", []string{"mode was removed", "provider_write_mode"}},
		{"TEST_ENV", "true", []string{"TEST_ENV was never consumed and is removed (or#915)"}},
		{"OPENRAILS_CATALOG_RECONCILIATION_INTERVAL", "30m", []string{"renamed", "CATALOG_RECONCILIATION_INTERVAL"}},
		{"OPENRAILS_SQL_TRACE", "1", []string{"renamed", "db.sql_trace"}},
		{"ACTIVE_KEY_ID", "x", []string{"ACTIVE_KEY_ID was renamed", "AUTHKIT_ACTIVE_KEY_ID"}},
		{"ACTIVE_PRIVATE_KEY_PEM", "x", []string{"AUTHKIT_ACTIVE_PRIVATE_KEY_PEM"}},
		{"PUBLIC_KEYS", "x", []string{"AUTHKIT_PUBLIC_KEYS"}},
		{"OPENRAILS_BILLING_HOT_PATH_FAIL_POLICY", "fail_open", []string{"billing_hot_path was removed"}},
		{"CLICKHOUSE_HTTP_ADDR", "http://ch.example:8123", []string{"clickhouse config was removed (#735)"}},
		{"DB_TYPO_FIELD", "x", []string{"unknown keys refuse boot", "typo_field"}},
		{"CATALOG_RECONCILIATION_INTERVAL", "30minutes", []string{"catalog_reconciliation_interval"}},
	} {
		t.Run("env "+row.env, func(t *testing.T) {
			bootEnv(t)
			t.Setenv(row.env, row.value)
			_, err := Load("")
			for _, want := range row.want {
				require.ErrorContains(t, err, want)
			}
		})
	}
	// Authority selectors refuse even the offline database loader.
	for _, key := range []string{"CATALOG_SOURCE", "MERCHANT_SOURCE", "MERCHANT_CONFIG_SOURCE", "ENV"} {
		t.Run("database "+key, func(t *testing.T) {
			bootEnv(t)
			t.Setenv(key, "")
			_, err := LoadDatabase("")
			require.Error(t, err)
		})
	}
	for yaml, want := range map[string]string{
		"not_a_real_key: 1\n":                       "not_a_real_key",
		"env: prod\n":                               "ENV is retired",
		"api_url: https://x\n":                      "API_URL is retired",
		"new_subscription_collection_policy: x\n":   "is retired",
		"catalog_source: api\n":                     "allow_catalog_updates",
		"merchant_source: manifest\n":               "merchant_config_http independently",
		"merchant_config_source: manifest\n":        "merchant_config_http independently",
		"mode: readonly\n":                          "mode was removed",
		"billing_hot_path:\n  fail_policy: open\n":  "billing_hot_path was removed",
		"clickhouse:\n  addr: x\n":                  "clickhouse config was removed",
		"merchant_cors:\n  - https://a.test\n":      "merchant_cors was removed",
		"auth:\n  control_plane:\n    enabled: 1\n": "auth.control_plane.enabled was removed",
	} {
		t.Run("yaml "+yaml, func(t *testing.T) {
			bootEnv(t)
			_, err := Load(writeFile(t, filepath.Join(t.TempDir(), "config.yaml"), yaml))
			require.ErrorContains(t, err, want)
		})
	}
}
