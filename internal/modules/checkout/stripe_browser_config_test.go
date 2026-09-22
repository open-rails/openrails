package checkout

import (
	"encoding/json"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestStripeBrowserKeyOnlyUsesExactPublicAccountDeclaration(t *testing.T) {
	for _, tc := range []struct {
		name, key, environment string
		allowed                bool
	}{
		{"sandbox", "pk_test_fixture", "test", true}, {"live", "pk_live_fixture", "live", true}, {"live_on_test", "pk_live_fixture", "test", false}, {"secret_never_public", "sk_test_do_not_expose", "test", false}, {"missing", "", "test", false}, {"missing_environment", "pk_live_fixture", "", false}, {"whitespace", "pk_test_fixture\n", "test", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"public_config": map[string]string{"publishable_key": tc.key}, "secret_key": "sk_should_never_escape"})
			key, err := stripeBrowserKey(gen.OpenrailsPsp{Rail: "stripe", Environment: tc.environment, Evidence: raw})
			if tc.allowed {
				require.NoError(t, err)
				require.Equal(t, tc.key, key)
			} else {
				require.Error(t, err)
				require.Empty(t, key)
				require.NotContains(t, err.Error(), "sk_")
			}
		})
	}
}

func TestStripeBrowserKeySupportsManifestSettingsAndRejectsConflicts(t *testing.T) {
	account := gen.OpenrailsPsp{Rail: "stripe", Environment: "test", Evidence: []byte(`{"settings":{"publishable_key":"pk_test_manifest"}}`)}
	key, err := stripeBrowserKey(account)
	require.NoError(t, err)
	require.Equal(t, "pk_test_manifest", key)
	account.Evidence = []byte(`{"settings":{"publishable_key":"pk_test_manifest"},"public_config":{"publishable_key":"pk_test_other"}}`)
	_, err = stripeBrowserKey(account)
	require.ErrorContains(t, err, "conflict")
}
