package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/stretchr/testify/require"
)

func TestCustodianEnvironmentYAML(t *testing.T) {
	for _, environment := range []string{"", "live", "test"} {
		name := environment
		if name == "" {
			name = "omitted"
		}
		t.Run(name, func(t *testing.T) {
			raw := "kind: basis_theory\naccount_id: tenant-a\n"
			if environment != "" {
				raw += "environment: " + environment + "\n"
			}
			var single config.CustodianConfig
			require.NoError(t, yaml.Unmarshal([]byte(raw), &single))
			require.Equal(t, environment, single.Environment)
			encoded, err := yaml.Marshal(single)
			require.NoError(t, err)
			var restored config.CustodianConfig
			require.NoError(t, yaml.Unmarshal(encoded, &restored))
			require.Equal(t, single, restored)

			var named config.Custodians
			require.NoError(t, yaml.Unmarshal([]byte("vault:\n  "+strings.ReplaceAll(raw, "\n", "\n  ")), &named))
			require.Equal(t, single, named["vault"])
			encoded, err = yaml.Marshal(named)
			require.NoError(t, err)
			var restoredNamed config.Custodians
			require.NoError(t, yaml.Unmarshal(encoded, &restoredNamed))
			require.Equal(t, named, restoredNamed)

			declared := merchantdocs.Declared(billing.MerchantID{}, config.MerchantDeclaration{Custodians: restoredNamed}, time.Time{})
			want := environment
			if want == "" {
				want = "live"
			}
			require.Equal(t, want, declared.Custodians["vault"].Value.Environment)
		})
	}
}
