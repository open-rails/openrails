//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func TestMerchantConfigurationLocalCLI(t *testing.T) {
	slug := "cli-" + uuid.NewString()
	_, err := dbtest.SharedSuperuserPGXPool(t).Exec(t.Context(), "INSERT INTO billing.merchants(id,slug) VALUES($1,$2)", uuid.New(), slug)
	require.NoError(t, err)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(fmt.Sprintf("db:\n  url: %q\ntest_mode: sandbox\nprovider_write_mode: readonly\nsecret_backend: snapshot\nrate_limits_disabled: true\n", dbtest.SharedPostgresDSN(t))), 0600))
	execute := func(command string, extra ...string) string {
		t.Helper()
		root := newRootCmd()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs(append([]string{command, "--merchant", slug, "--config", configPath, "--unbound-merchants"}, extra...))
		require.NoError(t, root.Execute())
		return out.String()
	}
	var state openrails.MerchantConfigurationState
	require.NoError(t, json.Unmarshal([]byte(execute("get-merchant-config")), &state))
	document := filepath.Join(t.TempDir(), "application.yaml")
	require.NoError(t, os.WriteFile(document, []byte(fmt.Sprintf("application_id: cli-operation\nexpected_revision: %q\ndisplay_name: CLI name\n", state.Revision)), 0600))
	require.Contains(t, execute("apply-merchant-config", "--file", document), `"replayed":false`)
	require.Contains(t, execute("apply-merchant-config", "--file", document), `"replayed":true`)
	require.Contains(t, execute("get-merchant-config"), "CLI name")
}
